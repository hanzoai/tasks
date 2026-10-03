// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	// hanzosqlite is the ONE sqlite driver across the Hanzo stack: it registers
	// the "sqlite" database/sql driver exactly once — pure-Go modernc under !cgo,
	// mattn+SQLCipher under cgo — and builds the backend-correct DSN. Every store
	// opens through it (never modernc directly) so a store embedded in the unified
	// cloud binary shares that single registration instead of double-registering
	// "sqlite" and panicking at init.
	"github.com/hanzoai/cek"
	"github.com/hanzoai/namespace"
	hanzosqlite "github.com/hanzoai/sqlite"
	"github.com/hanzoai/tasks/pkg/tasks/replication"
)

// Shard owns one SQLite file. WAL mode + foreign keys + 5s busy
// timeout. One writer; readers share the cache. Replicator hooks fire
// from put/del before local commit so the cluster sees the mutation
// first.
type Shard struct {
	principal  Principal
	ns         string
	path       string
	db         *db
	replicator replication.Replicator
	last       atomic.Int64
	closeOnce  sync.Once
	closed     atomic.Bool
}

const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;
PRAGMA synchronous=NORMAL;

CREATE TABLE IF NOT EXISTS kv (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL,
  upd   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS history (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  event_id  INTEGER NOT NULL,
  event_type TEXT NOT NULL,
  ts        TEXT NOT NULL,
  payload   BLOB
);

CREATE TABLE IF NOT EXISTS idem (
  workflow_id TEXT NOT NULL,
  request_id  TEXT NOT NULL,
  run_id      TEXT NOT NULL,
  PRIMARY KEY (workflow_id, request_id)
);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`

// openShard opens the shard holding namespace ns for tenant, under dir.
//
// cek derives the file's key from the process master and the tenant that
// owns it, picks the path, creates the directory and opens it encrypted —
// so the file's location and its key are two renderings of one name and
// cannot be paired wrongly. hanzoai/sqlite routes the keyed open through
// the live libsqlcipher codec when one is linked and through its pure-Go
// SQLCipher codec envelope when one is not, so a shard is ciphertext on
// every build and there is nothing here to branch on.
func openShard(tenant namespace.Namespace, dir string, p Principal, ns string) (*Shard, error) {
	// WAL + busy_timeout + synchronous=NORMAL + foreign_keys are set by schemaSQL
	// below (explicit PRAGMA statements), so they apply on both backends rather
	// than relying on driver-specific DSN params.
	conn, err := cek.Open(tenant, ns, dir)
	if err != nil {
		return nil, fmt.Errorf("store.openShard: open %s/%s: %w", tenant, ns, err)
	}
	path, err := namespace.Path(dir, tenant, ns)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	conn.SetMaxOpenConns(1) // single writer
	conn.SetMaxIdleConns(1)
	if _, err := conn.Exec(schemaSQL); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("store.openShard: schema: %w", err)
	}
	s := &Shard{principal: p, ns: ns, path: path, db: conn}
	s.last.Store(time.Now().UnixNano())
	return s, nil
}

// touch refreshes the idle timer.
func (s *Shard) touch() { s.last.Store(time.Now().UnixNano()) }

// lastUsed returns the time the shard was last touched.
func (s *Shard) lastUsed() time.Time { return time.Unix(0, s.last.Load()) }

// Principal returns the tenant that owns the shard.
func (s *Shard) Principal() Principal { return s.principal }

// Namespace returns the shard's namespace.
func (s *Shard) Namespace() string { return s.ns }

// Path returns the underlying file path.
func (s *Shard) Path() string { return s.path }

// Close flushes WAL and releases the connection. Idempotent.
func (s *Shard) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if s.db == nil {
		return nil
	}
	_, _ = s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	return s.db.Close()
}

// Checkpoint makes the on-disk file fully self-contained: it truncates the
// WAL and, for an encrypted shard the pure-Go codec envelope backs, seals
// the committed pages back into the ciphertext file. The seal is a
// successful no-op for a shard that persists per commit (the live codec and
// plaintext paths), so callers never ask which one they hold. Used by the
// migration tool before a copy and by the manager's sweep, which is what
// bounds an envelope-backed shard's exposure to an unclean exit.
func (s *Shard) Checkpoint() error {
	if s.closed.Load() {
		return ErrClosed
	}
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return err
	}
	return hanzosqlite.Checkpoint(s.db)
}

// Put writes value at key. If a Replicator is installed it runs
// Propose first; on Accept the local commit happens. On Reject the
// transaction is dropped and an error is returned.
func (s *Shard) Put(ctx context.Context, key string, value []byte) error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.touch()
	frame := replication.Frame{
		Principal: s.principal.String(),
		Namespace: s.ns,
		Op:        "put",
		Key:       key,
		Value:     append([]byte(nil), value...),
	}
	return s.replicate(ctx, frame, func() error { return s.localPut(key, value) })
}

// Get reads key.
func (s *Shard) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if s.closed.Load() {
		return nil, false, ErrClosed
	}
	s.touch()
	row := s.db.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", key)
	var v []byte
	if err := row.Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("store.Get(%s): %w", key, err)
	}
	return v, true, nil
}

// Del removes key. No-op if missing.
func (s *Shard) Del(ctx context.Context, key string) error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.touch()
	frame := replication.Frame{
		Principal: s.principal.String(),
		Namespace: s.ns,
		Op:        "del",
		Key:       key,
	}
	return s.replicate(ctx, frame, func() error { return s.localDel(key) })
}

// List walks every kv row whose key starts with prefix in lexicographic order.
func (s *Shard) List(ctx context.Context, prefix string, fn func(key string, value []byte) error) error {
	return s.Scan(ctx, prefix, "", 0, fn)
}

// Scan walks, in lexicographic order, at most limit kv rows (all of them when
// limit <= 0) whose key starts with prefix and sorts after `after`. Passing
// the last key one page returned as the next page's `after` walks a prefix in
// pieces; the shard's single connection is free between pages, so a long walk
// never holds the shard against the engine.
func (s *Shard) Scan(ctx context.Context, prefix, after string, limit int, fn func(key string, value []byte) error) error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.touch()
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	q, from := "SELECT key, value FROM kv WHERE key>=? AND key<? ORDER BY key LIMIT ?", prefix
	if after >= prefix {
		q, from = "SELECT key, value FROM kv WHERE key>? AND key<? ORDER BY key LIMIT ?", after
	}
	rows, err := s.db.QueryContext(ctx, q, from, prefixUpperBound(prefix), limit)
	if err != nil {
		return fmt.Errorf("store.Scan(%s): %w", prefix, err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// reclaimPages bounds how many free pages one Reclaim releases from a shard
// in incremental auto-vacuum mode, so a call never holds the shard for long.
const reclaimPages = 4096

// Reclaim gives the shard's free pages back to the filesystem. SQLite keeps
// the pages of deleted rows on a freelist and reuses them for new rows, but
// never shrinks the file on its own.
//
// A shard in incremental auto-vacuum mode releases up to reclaimPages per
// call. A shard in the default mode cannot release pages in place; it can
// only be rebuilt with VACUUM, which costs a copy of everything still in it.
// So it is rebuilt only when the caller passes rebuild — it has finished
// deleting for now, which is when the shard is smallest — and at least a
// quarter of it is free. The rebuild also switches the shard to incremental
// mode, so it happens once per shard. VACUUM keeps the page size and the
// reserved bytes per page that the encryption codec writes into.
//
// The smaller file reaches disk at the shard's next seal (Checkpoint or
// Close). Reclaim is local to this file and never replicated: each replica's
// pages are its own.
func (s *Shard) Reclaim(ctx context.Context, rebuild bool) error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.touch()
	var mode, free, total int64
	for _, p := range []struct {
		pragma string
		v      *int64
	}{{"auto_vacuum", &mode}, {"freelist_count", &free}, {"page_count", &total}} {
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+p.pragma).Scan(p.v); err != nil {
			return fmt.Errorf("store.Reclaim(%s): %s: %w", s.ns, p.pragma, err)
		}
	}
	const incremental = 2 // PRAGMA auto_vacuum: 0 none, 1 full, 2 incremental
	switch {
	case free == 0:
		return nil
	case mode == incremental:
		// incremental_vacuum releases one page per step and yields a row for
		// each, so it is drained as a query; an Exec would step it once.
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", reclaimPages))
		if err != nil {
			return fmt.Errorf("store.Reclaim(%s): %w", s.ns, err)
		}
		defer rows.Close()
		for rows.Next() {
		}
		return rows.Err()
	case mode == 0 && rebuild && free*4 >= total:
		if _, err := s.db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return fmt.Errorf("store.Reclaim(%s): %w", s.ns, err)
		}
		if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
			return fmt.Errorf("store.Reclaim(%s): vacuum: %w", s.ns, err)
		}
	}
	return nil
}

// replicate runs Propose, and on Accept calls apply locally.
func (s *Shard) replicate(ctx context.Context, f replication.Frame, apply func() error) error {
	if s.replicator == nil {
		return apply()
	}
	dec, err := s.replicator.Propose(ctx, f)
	if err != nil {
		return err
	}
	switch dec {
	case replication.DecisionAccept:
		// Subscribe handler installed by Manager.WithReplicator already
		// applied the frame to this shard, so we don't double-apply.
		return nil
	case replication.DecisionReject:
		return replication.ErrRejected
	default:
		return replication.ErrTimeout
	}
}

// applyFrame is the Replicator-driven applier — invoked once per
// accepted frame, on every node (including the proposer).
func (s *Shard) applyFrame(f replication.Frame) error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.touch()
	switch f.Op {
	case "put":
		return s.localPut(f.Key, f.Value)
	case "del":
		return s.localDel(f.Key)
	case "migration.lock", "migration.unlock":
		// Barrier operations have no local storage effect; the
		// coordinator uses Propose round-trip as a synchronization
		// fence across the cluster.
		return nil
	default:
		return fmt.Errorf("store.applyFrame: unknown op %q", f.Op)
	}
}

func (s *Shard) localPut(key string, value []byte) error {
	_, err := s.db.Exec("INSERT INTO kv(key, value, upd) VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, upd=excluded.upd",
		key, value, time.Now().UnixNano())
	return err
}

func (s *Shard) localDel(key string) error {
	_, err := s.db.Exec("DELETE FROM kv WHERE key=?", key)
	return err
}

// prefixUpperBound returns the smallest string greater than every
// string with the given prefix, suitable for SQLite range scans.
func prefixUpperBound(prefix string) string {
	if prefix == "" {
		return "\xff\xff\xff\xff"
	}
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return string(b) + "\xff"
}
