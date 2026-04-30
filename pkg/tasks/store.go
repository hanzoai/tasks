// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
)

// store is the engine-facing facade. Each operation routes through a
// per-(org, namespace) SQLite shard managed by storepkg.Manager. Keys
// are parsed to determine routing — the canonical layout encodes the
// namespace as the second segment for every kind except `ns/<name>`
// (which is the namespace's own registry row, stored in that
// namespace's shard) and bare cross-namespace scans.
//
// orgID == "" preserves the embedded/dev path: shards live under
// <root>/_/ (sentinel directory).
type store struct {
	mgr    *storepkg.Manager
	orgID  string
	mu     sync.RWMutex
	bootNS map[string]struct{}
}

// newStore returns a sqlite-backed single-shard store rooted at a temp dir.
// Used by tests that previously called newStore() against memdb.
func newStore() *store {
	dir, err := os.MkdirTemp("", "tasks-store-")
	if err != nil {
		panic(fmt.Errorf("newStore: tempdir: %w", err))
	}
	mgr, err := storepkg.New(dir)
	if err != nil {
		panic(fmt.Errorf("newStore: %w", err))
	}
	return &store{mgr: mgr, bootNS: map[string]struct{}{}}
}

// newStoreFromEnv selects the backend by TASKSD_STORE.
//
//	(unset) | "sqlite"  → per-namespace SQLite shards under dataDir
//	"memory"            → temp directory under os.TempDir() (volatile)
//	any other value     → error
func newStoreFromEnv(dataDir string) (*store, error) {
	mode := os.Getenv("TASKSD_STORE")
	switch mode {
	case "", "sqlite":
		if dataDir == "" {
			return nil, fmt.Errorf("TASKSD_STORE=sqlite requires non-empty data dir")
		}
		mgr, err := storepkg.New(dataDir)
		if err != nil {
			return nil, err
		}
		return &store{mgr: mgr, bootNS: map[string]struct{}{}}, nil
	case "memory":
		return newStore(), nil
	default:
		return nil, fmt.Errorf("TASKSD_STORE=%q: unsupported (sqlite | memory)", mode)
	}
}

// Manager exposes the underlying shard manager (for migration / cluster diagnostics).
func (s *store) Manager() *storepkg.Manager { return s.mgr }

// withOrg returns a store view scoped to orgID; it shares the manager.
func (s *store) withOrg(orgID string) *store {
	if orgID == s.orgID {
		return s
	}
	return &store{mgr: s.mgr, orgID: orgID, bootNS: s.bootNS}
}

func (s *store) close() error {
	if s == nil || s.mgr == nil {
		return nil
	}
	return s.mgr.Close()
}

// shardFor resolves the shard for a key. ns/<name> writes the
// namespace registry row into that namespace's own shard.
func (s *store) shardFor(ctx context.Context, key string) (*storepkg.Shard, error) {
	_, ns, _, ok := storepkg.NsFromKey(key)
	if !ok {
		return nil, fmt.Errorf("store: cannot route key %q", key)
	}
	sh, err := s.mgr.Get(ctx, s.orgID, ns)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.bootNS[ns] = struct{}{}
	s.mu.Unlock()
	return sh, nil
}

// put serializes v and writes to the resolved shard.
func (s *store) put(key string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store.put: marshal: %w", err)
	}
	ctx := context.Background()
	sh, err := s.shardFor(ctx, key)
	if err != nil {
		return err
	}
	return sh.Put(ctx, key, body)
}

// get loads JSON at key into v. Returns false if missing.
func (s *store) get(key string, v any) (bool, error) {
	ctx := context.Background()
	sh, err := s.shardFor(ctx, key)
	if err != nil {
		return false, err
	}
	body, ok, err := sh.Get(ctx, key)
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
	ctx := context.Background()
	sh, err := s.shardFor(ctx, key)
	if err != nil {
		return err
	}
	return sh.Del(ctx, key)
}

// list iterates entries with the given prefix in lexicographic order.
// Cross-namespace scans (ns/, nx/) fan out across every shard the
// org owns.
func (s *store) list(prefix string, fn func(key string, body []byte) error) error {
	ctx := context.Background()
	if storepkg.IsCrossNamespacePrefix(prefix) {
		shards, err := s.mgr.ListShards(ctx, s.orgID)
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
	sh, err := s.mgr.Get(ctx, s.orgID, ns)
	if err != nil {
		return err
	}
	return sh.List(ctx, prefix, fn)
}

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
