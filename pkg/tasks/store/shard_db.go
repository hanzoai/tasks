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

	"github.com/hanzoai/tasks/pkg/tasks/replication"
)

// Shard owns one SQLite file. WAL mode + foreign keys + 5s busy
// timeout. One writer; readers share the cache. Replicator hooks fire
// from put/del before local commit so the cluster sees the mutation
// first.
type Shard struct {
	org        string
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

func openShard(path, org, ns string) (*Shard, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=journal_mode(wal)&_pragma=synchronous(normal)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store.openShard: open %s: %w", path, err)
	}
	conn.SetMaxOpenConns(1) // single writer
	conn.SetMaxIdleConns(1)
	if _, err := conn.Exec(schemaSQL); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("store.openShard: schema: %w", err)
	}
	s := &Shard{org: org, ns: ns, path: path, db: conn}
	s.last.Store(time.Now().UnixNano())
	if org == SentinelOrg {
		s.org = ""
	}
	return s, nil
}

// touch refreshes the idle timer.
func (s *Shard) touch() { s.last.Store(time.Now().UnixNano()) }

// lastUsed returns the time the shard was last touched.
func (s *Shard) lastUsed() time.Time { return time.Unix(0, s.last.Load()) }

// Org returns the shard's org id ("" for sentinel).
func (s *Shard) Org() string { return s.org }

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

// Checkpoint forces a WAL truncation without releasing the
// connection. Used by the migration tool to make the on-disk file
// fully self-contained before copy.
func (s *Shard) Checkpoint() error {
	if s.closed.Load() {
		return ErrClosed
	}
	_, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	return err
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
		OrgID:     s.org,
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
		OrgID:     s.org,
		Namespace: s.ns,
		Op:        "del",
		Key:       key,
	}
	return s.replicate(ctx, frame, func() error { return s.localDel(key) })
}

// List walks every kv row whose key starts with prefix in lexicographic order.
func (s *Shard) List(ctx context.Context, prefix string, fn func(key string, value []byte) error) error {
	if s.closed.Load() {
		return ErrClosed
	}
	s.touch()
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM kv WHERE key>=? AND key<? ORDER BY key", prefix, prefixUpperBound(prefix))
	if err != nil {
		return fmt.Errorf("store.List(%s): %w", prefix, err)
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
