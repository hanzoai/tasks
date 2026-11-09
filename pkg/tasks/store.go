// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
)

// store is the engine-facing facade. Each operation goes to a backend: the
// per-(principal, namespace) SQLite shards of ONE process (files), or the tables of
// one database that several replicas open (shared). Keys are parsed to determine
// routing on the files — the canonical layout encodes the namespace as the second
// segment for every kind except `ns/<name>` (which is the namespace's own registry
// row, stored in that namespace's shard) and bare cross-namespace scans. On the
// shared database a key already carries its namespace and there is nothing to route.
//
// The zero principal is the root tenant: the embedded / dev path.
type store struct {
	be        backend
	mgr       *storepkg.Manager // the files; nil on a shared database
	shared    *storepkg.Shared  // the shared database; nil on files
	principal Principal
}

// backend is where the rows live. Both homes keep the same keyed documents and
// answer the same prefix scans; what differs is who may open them at once.
type backend interface {
	put(ctx context.Context, p Principal, key string, value []byte) error
	get(ctx context.Context, p Principal, key string) ([]byte, bool, error)
	del(ctx context.Context, p Principal, key string) error
	list(ctx context.Context, p Principal, prefix string, fn func(key string, body []byte) error) error
	scan(ctx context.Context, p Principal, prefix, after string, limit int, fn func(key string, body []byte) error) error
	every(ctx context.Context, prefix string, fn func(p Principal, key string, body []byte) error) error
	tenants(ctx context.Context) ([]Principal, error)
	reclaim(ctx context.Context, p Principal, ns string, rebuild bool) error
	close() error
}

// newStore returns a sqlite-backed single-shard store rooted at a temp dir.
// Used by tests that previously called newStore() against memdb.
func newStore() *store {
	dir, err := os.MkdirTemp("", "tasks-store-")
	if err != nil {
		panic(fmt.Errorf("newStore: tempdir: %w", err))
	}
	mgr, err := storepkg.New(dir, nil)
	if err != nil {
		panic(fmt.Errorf("newStore: %w", err))
	}
	return newFileStore(mgr)
}

func newFileStore(mgr *storepkg.Manager) *store {
	return &store{be: files{mgr}, mgr: mgr}
}

// newSharedStore keeps the engine's rows in db, which several replicas open at once.
func newSharedStore(ctx context.Context, db *sql.DB) (*store, error) {
	sh, err := storepkg.OpenShared(ctx, db)
	if err != nil {
		return nil, err
	}
	return &store{be: shared{sh}, shared: sh}, nil
}

// newStoreFromEnv selects the backend by TASKSD_STORE.
//
//	(unset) | "sqlite"  → per-namespace SQLite shards under dataDir
//	"memory"            → temp directory under os.TempDir() (volatile)
//	any other value     → error
//
// master is the root key every shard's DEK is wrapped under; nil opens
// shards plaintext.
func newStoreFromEnv(dataDir string, master []byte) (*store, error) {
	mode := os.Getenv("TASKSD_STORE")
	switch mode {
	case "", "sqlite":
		if dataDir == "" {
			return nil, fmt.Errorf("TASKSD_STORE=sqlite requires non-empty data dir")
		}
		mgr, err := storepkg.New(dataDir, master)
		if err != nil {
			return nil, err
		}
		return newFileStore(mgr), nil
	case "memory":
		return newStore(), nil
	default:
		return nil, fmt.Errorf("TASKSD_STORE=%q: unsupported (sqlite | memory)", mode)
	}
}

// Manager exposes the underlying shard manager (for migration / cluster diagnostics).
// It is nil when the rows are on a shared database.
func (s *store) Manager() *storepkg.Manager { return s.mgr }

// as returns a store view scoped to p; it shares the backend.
func (s *store) as(p Principal) *store {
	if p == s.principal {
		return s
	}
	return &store{be: s.be, mgr: s.mgr, shared: s.shared, principal: p}
}

func (s *store) close() error {
	if s == nil || s.be == nil {
		return nil
	}
	return s.be.close()
}

// put serializes v and writes it for the store's tenant.
func (s *store) put(key string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store.put: marshal: %w", err)
	}
	return s.be.put(context.Background(), s.principal, key, body)
}

// get loads JSON at key into v. Returns false if missing.
func (s *store) get(key string, v any) (bool, error) {
	body, ok, err := s.be.get(context.Background(), s.principal, key)
	if err != nil || !ok {
		return false, err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return false, fmt.Errorf("store.get: unmarshal: %w", err)
	}
	return true, nil
}

// del removes a key.
func (s *store) del(key string) error {
	return s.be.del(context.Background(), s.principal, key)
}

// list iterates entries with the given prefix in lexicographic order.
// Cross-namespace scans (ns/, nx/) fan out across every shard the
// org owns.
func (s *store) list(prefix string, fn func(key string, body []byte) error) error {
	return s.be.list(context.Background(), s.principal, prefix, fn)
}

// scan walks one page of a single namespace's entries under prefix: at most
// limit of them, after the key `after` (see storepkg.Shard.Scan).
func (s *store) scan(prefix, after string, limit int, fn func(key string, body []byte) error) error {
	return s.be.scan(context.Background(), s.principal, prefix, after, limit, fn)
}

// reclaim gives ns's free pages back to the filesystem (see
// storepkg.Shard.Reclaim). A shared database has no file to shrink.
func (s *store) reclaim(ns string, rebuild bool) error {
	return s.be.reclaim(context.Background(), s.principal, ns, rebuild)
}

// listEveryTenant iterates entries with the given prefix across EVERY
// tenant's shards — the root cron sweeper's view of the world. fn receives
// the owning principal alongside each entry so the caller can act through
// that tenant's engine view (engine.as) and hit the right shard.
func (s *store) listEveryTenant(prefix string, fn func(p Principal, key string, body []byte) error) error {
	return s.be.every(context.Background(), prefix, fn)
}

// tenants lists every tenant that owns a row.
func (s *store) tenants() ([]Principal, error) {
	return s.be.tenants(context.Background())
}

// files is the backend of one process: a SQLite file per (tenant, namespace).
type files struct{ mgr *storepkg.Manager }

func (f files) shardFor(ctx context.Context, p Principal, key string) (*storepkg.Shard, error) {
	_, ns, _, ok := storepkg.NsFromKey(key)
	if !ok {
		return nil, fmt.Errorf("store: cannot route key %q", key)
	}
	return f.mgr.Get(ctx, p, ns)
}

func (f files) put(ctx context.Context, p Principal, key string, value []byte) error {
	sh, err := f.shardFor(ctx, p, key)
	if err != nil {
		return err
	}
	return sh.Put(ctx, key, value)
}

func (f files) get(ctx context.Context, p Principal, key string) ([]byte, bool, error) {
	sh, err := f.shardFor(ctx, p, key)
	if err != nil {
		return nil, false, err
	}
	return sh.Get(ctx, key)
}

func (f files) del(ctx context.Context, p Principal, key string) error {
	sh, err := f.shardFor(ctx, p, key)
	if err != nil {
		return err
	}
	return sh.Del(ctx, key)
}

func (f files) list(ctx context.Context, p Principal, prefix string, fn func(key string, body []byte) error) error {
	if storepkg.IsCrossNamespacePrefix(prefix) {
		shards, err := f.mgr.ListShards(ctx, p)
		if err != nil {
			return err
		}
		for _, sh := range shards {
			if err := sh.List(ctx, prefix, fn); err != nil {
				return err
			}
		}
		return nil
	}
	_, ns, _ := storepkg.SplitPrefix(prefix)
	if ns == "" {
		return fmt.Errorf("store.list: missing namespace in prefix %q", prefix)
	}
	sh, err := f.mgr.Get(ctx, p, ns)
	if err != nil {
		return err
	}
	return sh.List(ctx, prefix, fn)
}

func (f files) scan(ctx context.Context, p Principal, prefix, after string, limit int, fn func(key string, body []byte) error) error {
	_, ns, _ := storepkg.SplitPrefix(prefix)
	if ns == "" || storepkg.IsCrossNamespacePrefix(prefix) {
		return fmt.Errorf("store.scan: prefix %q does not name one namespace", prefix)
	}
	sh, err := f.mgr.Get(ctx, p, ns)
	if err != nil {
		return err
	}
	return sh.Scan(ctx, prefix, after, limit, fn)
}

func (f files) every(ctx context.Context, prefix string, fn func(p Principal, key string, body []byte) error) error {
	tenants, err := f.mgr.ListPrincipals(ctx)
	if err != nil {
		return err
	}
	for _, p := range tenants {
		shards, err := f.mgr.ListShards(ctx, p)
		if err != nil {
			return err
		}
		for _, sh := range shards {
			if err := sh.List(ctx, prefix, func(key string, body []byte) error {
				return fn(p, key, body)
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f files) tenants(ctx context.Context) ([]Principal, error) { return f.mgr.ListPrincipals(ctx) }

func (f files) reclaim(ctx context.Context, p Principal, ns string, rebuild bool) error {
	sh, err := f.mgr.Get(ctx, p, ns)
	if err != nil {
		return err
	}
	return sh.Reclaim(ctx, rebuild)
}

func (f files) close() error { return f.mgr.Close() }

// shared is the backend of a database several replicas open at once.
type shared struct{ sh *storepkg.Shared }

func (d shared) put(ctx context.Context, p Principal, key string, value []byte) error {
	return d.sh.Put(ctx, p, key, value)
}

func (d shared) get(ctx context.Context, p Principal, key string) ([]byte, bool, error) {
	return d.sh.Get(ctx, p, key)
}

func (d shared) del(ctx context.Context, p Principal, key string) error {
	return d.sh.Del(ctx, p, key)
}

func (d shared) list(ctx context.Context, p Principal, prefix string, fn func(key string, body []byte) error) error {
	return d.sh.List(ctx, p, prefix, fn)
}

func (d shared) scan(ctx context.Context, p Principal, prefix, after string, limit int, fn func(key string, body []byte) error) error {
	return d.sh.Scan(ctx, p, prefix, after, limit, fn)
}

func (d shared) every(ctx context.Context, prefix string, fn func(p Principal, key string, body []byte) error) error {
	return d.sh.ScanAll(ctx, prefix, fn)
}

func (d shared) tenants(ctx context.Context) ([]Principal, error) { return d.sh.Principals(ctx) }

func (d shared) reclaim(context.Context, Principal, string, bool) error { return nil }

func (d shared) close() error { return d.sh.Close() }

// listInto loads every record under prefix into a typed slice.
func listInto[T any](s *store, prefix string) ([]T, error) {
	out := []T{}
	if err := s.list(prefix, func(_ string, body []byte) error {
		var v T
		if err := json.Unmarshal(body, &v); err != nil {
			return err
		}
		out = append(out, v)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }
