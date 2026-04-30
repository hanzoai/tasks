// Copyright © 2026 Hanzo AI. MIT License.

// Package store implements per-(org, namespace) SQLite shards with
// optional consensus replication.
//
// Layout on disk:
//
//	<rootDir>/<orgID>/<namespace>.db   — one SQLite file per namespace
//	<rootDir>/_/<namespace>.db         — orgID == "" (embedded/dev)
//
// Schema (single table; key/value blob):
//
//	CREATE TABLE kv(
//	  key   TEXT PRIMARY KEY,
//	  value BLOB NOT NULL,
//	  upd   INTEGER NOT NULL
//	);
//
// Replication:
//
//	A Replicator may be attached via WithReplicator. Every put/del is
//	wrapped in a replication.Frame and Propose'd before the local apply
//	commits. On Accept the frame is also dispatched to peers via the
//	driver-internal Subscribe path.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"github.com/hanzoai/tasks/pkg/tasks/replication"
)

// SentinelOrg is the on-disk directory name for the empty org slot. We
// avoid using "" so the path stays well-formed.
const SentinelOrg = "_"

// IdleEvictAfter sets how long an open shard may sit unused before the
// manager closes it. Mutable for tests.
var IdleEvictAfter = 10 * time.Minute

// ErrClosed is returned when a method runs after Close.
var ErrClosed = errors.New("store: manager closed")

// Manager owns the on-disk shard layout and the open-shard cache.
type Manager struct {
	rootDir    string
	replicator replication.Replicator
	mu         sync.Mutex
	shards     map[shardKey]*Shard
	closed     atomic.Bool
	stopGC     chan struct{}
}

type shardKey struct {
	org string
	ns  string
}

// New opens the manager rooted at rootDir. The directory is created on
// demand; errors are returned only for unrecoverable IO problems.
func New(rootDir string) (*Manager, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("store.New: rootDir required")
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("store.New: mkdir: %w", err)
	}
	m := &Manager{
		rootDir: rootDir,
		shards:  make(map[shardKey]*Shard),
		stopGC:  make(chan struct{}),
	}
	go m.gc()
	return m, nil
}

// WithReplicator installs r as the consensus driver for every shard
// opened from now on, and re-installs it on already-open shards.
func (m *Manager) WithReplicator(r replication.Replicator) {
	m.mu.Lock()
	m.replicator = r
	for _, s := range m.shards {
		s.replicator = r
	}
	m.mu.Unlock()
	if r == nil {
		return
	}
	r.Subscribe(func(ctx context.Context, f replication.Frame) error {
		s, err := m.Get(ctx, f.OrgID, f.Namespace)
		if err != nil {
			return err
		}
		return s.applyFrame(f)
	})
}

// Get returns the open shard for (org, ns), creating it on disk if
// needed. The returned Shard is safe for concurrent use.
func (m *Manager) Get(ctx context.Context, org, ns string) (*Shard, error) {
	if m.closed.Load() {
		return nil, ErrClosed
	}
	if ns == "" {
		return nil, fmt.Errorf("store.Get: namespace required")
	}
	k := shardKey{org: orgDir(org), ns: ns}
	m.mu.Lock()
	if s, ok := m.shards[k]; ok {
		s.touch()
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	dir := filepath.Join(m.rootDir, k.org)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store.Get: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, ns+".db")
	s, err := openShard(path, k.org, k.ns)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if existing, ok := m.shards[k]; ok {
		// Lost the race; close the duplicate.
		_ = s.Close()
		existing.touch()
		m.mu.Unlock()
		return existing, nil
	}
	s.replicator = m.replicator
	m.shards[k] = s
	m.mu.Unlock()
	return s, nil
}

// ListShards enumerates every namespace shard under org. Used by
// cross-namespace operations like ListNamespaces().
func (m *Manager) ListShards(ctx context.Context, org string) ([]*Shard, error) {
	dir := filepath.Join(m.rootDir, orgDir(org))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]*Shard, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".db") || strings.HasPrefix(name, "-") {
			continue
		}
		ns := strings.TrimSuffix(name, ".db")
		s, err := m.Get(ctx, org, ns)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ns < out[j].ns })
	return out, nil
}

// OpenShardCount reports the number of resident shards (for /v1/tasks/cluster).
func (m *Manager) OpenShardCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.shards)
}

// Close flushes and closes every open shard. Safe to call twice.
func (m *Manager) Close() error {
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(m.stopGC)
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for k, s := range m.shards {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(m.shards, k)
	}
	return firstErr
}

// gc closes idle shards every minute.
func (m *Manager) gc() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-m.stopGC:
			return
		case <-t.C:
			m.evictIdle()
		}
	}
}

func (m *Manager) evictIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-IdleEvictAfter)
	for k, s := range m.shards {
		if s.lastUsed().Before(cutoff) {
			_ = s.Close()
			delete(m.shards, k)
		}
	}
}

// orgDir maps the on-disk directory name for an org id.
func orgDir(org string) string {
	if org == "" {
		return SentinelOrg
	}
	return org
}

// CopyFile is a helper used by the migration tool. Returns bytes copied.
func CopyFile(dst, src string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}

// ShardPath returns the on-disk file for (org, ns).
func (m *Manager) ShardPath(org, ns string) string {
	return filepath.Join(m.rootDir, orgDir(org), ns+".db")
}

// Replicator returns the currently-installed driver, or nil.
func (m *Manager) Replicator() replication.Replicator {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.replicator
}

// RootDir returns the on-disk root.
func (m *Manager) RootDir() string { return m.rootDir }

// db is a typed alias for the open *sql.DB so tests don't need to import database/sql.
type db = sql.DB
