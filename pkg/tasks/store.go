// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/luxfi/database"
	"github.com/luxfi/database/memdb"
	"github.com/luxfi/database/zapdb"
	"github.com/luxfi/metric"
)

// store is a tiny key-value layer over luxfi/database. The current
// backend is memdb (volatile); swap to zapdb via factory.New for
// disk persistence without changing this surface.
//
// Key layout (everything ASCII so prefix-iteration just works):
//
//   ns/<name>                   → Namespace
//   wf/<ns>/<workflowId>/<runId>→ WorkflowExecution
//   sc/<ns>/<scheduleId>        → Schedule
//   bt/<ns>/<batchId>           → BatchOperation
//   dp/<ns>/<seriesName>        → Deployment
//   nx/<ns>/<endpointName>      → NexusEndpoint
//   id/<ns>/<email>             → Identity
//
// When orgPrefix is non-empty, every key is transparently prefixed with
// "org:<id>:". Empty prefix preserves legacy embedded-use behavior.
type store struct {
	mu        *sync.RWMutex
	db        database.Database
	orgPrefix string
}

func newStore() *store {
	return &store{mu: &sync.RWMutex{}, db: memdb.New()}
}

// newStoreFromEnv selects the backend by TASKSD_STORE env var.
// "memory" (default) → in-memory, lost on restart.
// "zapdb"            → durable, rooted at dataDir.
// dataDir is only used by durable backends; ignored for memory.
func newStoreFromEnv(dataDir string) (*store, error) {
	switch os.Getenv("TASKSD_STORE") {
	case "zapdb":
		if dataDir == "" {
			return nil, fmt.Errorf("TASKSD_STORE=zapdb requires non-empty data dir")
		}
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %q: %w", dataDir, err)
		}
		db, err := zapdb.New(dataDir, nil, "tasks", metric.NewNoOpRegistry())
		if err != nil {
			return nil, fmt.Errorf("store: zapdb open: %w", err)
		}
		return &store{mu: &sync.RWMutex{}, db: db}, nil
	default:
		return newStore(), nil
	}
}

// withOrg returns a view of s that prefixes every key with "org:<id>:".
// The view shares the underlying db + mutex; concurrent use is safe.
// orgID == "" returns s unchanged.
func (s *store) withOrg(orgID string) *store {
	if orgID == "" {
		return s
	}
	return &store{mu: s.mu, db: s.db, orgPrefix: "org:" + orgID + ":"}
}

func (s *store) k(key string) string { return s.orgPrefix + key }

func (s *store) close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// put serializes v to JSON and stores at key.
func (s *store) put(key string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("store.put: marshal: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Put([]byte(s.k(key)), body)
}

// get loads JSON at key into v. Returns false if missing.
func (s *store) get(key string, v any) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	body, err := s.db.Get([]byte(s.k(key)))
	if err == database.ErrNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.get: %w", err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return false, fmt.Errorf("store.get: unmarshal: %w", err)
	}
	return true, nil
}

// del removes a key. No-op if missing.
func (s *store) del(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.Delete([]byte(s.k(key))); err != nil && err != database.ErrNotFound {
		return err
	}
	return nil
}

// list iterates entries with the given prefix and yields (key, body) pairs
// in lexicographic order. The iteration snapshots under the read lock
// and releases before calling fn, so the callback is free to put/del
// against the same store without deadlocking the RWMutex.
//
// Keys yielded to fn have the org prefix stripped — callers see the
// canonical "ns/", "wf/<ns>/…" layout regardless of org scoping.
func (s *store) list(prefix string, fn func(key string, body []byte) error) error {
	type kv struct{ k, v []byte }
	var snap []kv
	scanPrefix := s.k(prefix)
	if err := func() error {
		s.mu.RLock()
		defer s.mu.RUnlock()
		it := s.db.NewIteratorWithPrefix([]byte(scanPrefix))
		defer it.Release()
		for it.Next() {
			k := append([]byte(nil), it.Key()...)
			v := append([]byte(nil), it.Value()...)
			snap = append(snap, kv{k, v})
		}
		return it.Error()
	}(); err != nil {
		return err
	}
	strip := len(s.orgPrefix)
	for _, e := range snap {
		if err := fn(string(e.k[strip:]), e.v); err != nil {
			return err
		}
	}
	return nil
}

// listInto loads every record under prefix into a slice via the supplied
// allocator + setter — the typical pattern for List ops.
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
