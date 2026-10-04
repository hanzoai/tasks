// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// hanzosqlite is the ONE sqlite driver across the Hanzo stack: it registers
	// the "sqlite" database/sql driver exactly once — pure-Go modernc under !cgo,
	// mattn+SQLCipher under cgo — and builds the backend-correct DSN. Every store
	// opens through it (never modernc directly) so a store embedded in the unified
	// cloud binary shares that single registration instead of double-registering
	// "sqlite" and panicking at init.
	hanzosqlite "github.com/hanzoai/sqlite"
)

// rows is where a shard's kv rows live: its own SQLite file (fileKV), or its slice
// of one table on a shared SQL server (sqlKV). A shard is the same value either way;
// only where its rows are kept differs.
type rows interface {
	get(ctx context.Context, key string) ([]byte, bool, error)
	put(key string, value []byte, upd int64) error
	del(key string) error
	// scan walks rows whose key starts with prefix and sorts after after (from
	// prefix itself when after < prefix), in byte order, at most limit (all when
	// limit <= 0).
	scan(ctx context.Context, prefix, after string, limit int, fn func(key string, value []byte) error) error
	checkpoint() error
	reclaim(ctx context.Context, rebuild bool) error
	close() error
}

// fileKV is a shard's rows in its own SQLite file.
type fileKV struct{ db *sql.DB }

func (f fileKV) get(ctx context.Context, key string) ([]byte, bool, error) {
	var v []byte
	if err := f.db.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", key).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return v, true, nil
}

func (f fileKV) put(key string, value []byte, upd int64) error {
	_, err := f.db.Exec("INSERT INTO kv(key, value, upd) VALUES(?, ?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, upd=excluded.upd",
		key, value, upd)
	return err
}

func (f fileKV) del(key string) error {
	_, err := f.db.Exec("DELETE FROM kv WHERE key=?", key)
	return err
}

func (f fileKV) scan(ctx context.Context, prefix, after string, limit int, fn func(key string, value []byte) error) error {
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}
	q, from := "SELECT key, value FROM kv WHERE key>=? AND key<? ORDER BY key LIMIT ?", prefix
	if after >= prefix {
		q, from = "SELECT key, value FROM kv WHERE key>? AND key<? ORDER BY key LIMIT ?", after
	}
	r, err := f.db.QueryContext(ctx, q, from, prefixUpperBound(prefix), limit)
	if err != nil {
		return err
	}
	defer r.Close()
	for r.Next() {
		var k string
		var v []byte
		if err := r.Scan(&k, &v); err != nil {
			return err
		}
		if err := fn(k, v); err != nil {
			return err
		}
	}
	return r.Err()
}

func (f fileKV) checkpoint() error {
	if _, err := f.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);"); err != nil {
		return err
	}
	return hanzosqlite.Checkpoint(f.db)
}

// reclaimPages bounds how many free pages one Reclaim releases from a shard
// in incremental auto-vacuum mode, so a call never holds the shard for long.
const reclaimPages = 4096

// reclaim gives the shard's free pages back to the filesystem. SQLite keeps
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
func (f fileKV) reclaim(ctx context.Context, rebuild bool) error {
	var mode, free, total int64
	for _, p := range []struct {
		pragma string
		v      *int64
	}{{"auto_vacuum", &mode}, {"freelist_count", &free}, {"page_count", &total}} {
		if err := f.db.QueryRowContext(ctx, "PRAGMA "+p.pragma).Scan(p.v); err != nil {
			return fmt.Errorf("%s: %w", p.pragma, err)
		}
	}
	const incremental = 2 // PRAGMA auto_vacuum: 0 none, 1 full, 2 incremental
	switch {
	case free == 0:
		return nil
	case mode == incremental:
		// incremental_vacuum releases one page per step and yields a row for
		// each, so it is drained as a query; an Exec would step it once.
		r, err := f.db.QueryContext(ctx, fmt.Sprintf("PRAGMA incremental_vacuum(%d)", reclaimPages))
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
		}
		return r.Err()
	case mode == 0 && rebuild && free*4 >= total:
		if _, err := f.db.ExecContext(ctx, "PRAGMA auto_vacuum=INCREMENTAL"); err != nil {
			return err
		}
		if _, err := f.db.ExecContext(ctx, "VACUUM"); err != nil {
			return fmt.Errorf("vacuum: %w", err)
		}
	}
	return nil
}

func (f fileKV) close() error {
	_, _ = f.db.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
	return f.db.Close()
}

// now is the upd stamp a put records.
func now() int64 { return time.Now().UnixNano() }
