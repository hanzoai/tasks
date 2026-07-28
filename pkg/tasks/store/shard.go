// Copyright © 2026 Hanzo AI. MIT License.

// Package store implements per-(org, namespace) SQLite shards with
// optional consensus replication.
//
// Layout on disk:
//
//	<rootDir>/<org>/<project>/<user>/<namespace>.db
//
// The three directory levels are the Principal that owns the shard, each
// unset leg written as the sentinel "_" (see principal.go). The root
// tenant — the unscoped embedded / dev path — is therefore <rootDir>/_/_/_.
//
// A shard is created on first use. Namespaces are not declared up front.
//
// Schema (single table; key/value blob):
//
//	CREATE TABLE kv(
//	  key   TEXT PRIMARY KEY,
//	  value BLOB NOT NULL,
//	  upd   INTEGER NOT NULL
//	);
//
// Encryption:
//
//	A non-nil master key opens every shard at rest encrypted, under a DEK
//	of its own wrapped for its Principal (see dek.go). A nil master key
//	opens plaintext — the zero-config dev posture.
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

	"github.com/hanzoai/tasks/pkg/tasks/replication"
)

// IdleEvictAfter sets how long an open shard may sit unused before the
// manager closes it. Mutable for tests.
var IdleEvictAfter = 10 * time.Minute

// ErrClosed is returned when a method runs after Close.
var ErrClosed = errors.New("store: manager closed")

// Manager owns the on-disk shard layout and the open-shard cache.
type Manager struct {
	rootDir    string
	master     []byte
	replicator replication.Replicator
	mu         sync.Mutex
	shards     map[shardKey]*Shard
	closed     atomic.Bool
	stopGC     chan struct{}
}

type shardKey struct {
	principal Principal
	ns        string
}

// New opens the manager rooted at rootDir. The directory is created on
// demand; errors are returned only for unrecoverable IO problems.
//
// master is the 32-byte root key every shard's DEK is wrapped under. A nil
// master opens shards plaintext — the zero-config dev posture; any other
// length is rejected here rather than at the first shard open.
func New(rootDir string, master []byte) (*Manager, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("store.New: rootDir required")
	}
	if master != nil && len(master) != MasterKeyLen {
		return nil, fmt.Errorf("store.New: master key must be %d bytes, got %d", MasterKeyLen, len(master))
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("store.New: mkdir: %w", err)
	}
	m := &Manager{
		rootDir: rootDir,
		master:  master,
		shards:  make(map[shardKey]*Shard),
		stopGC:  make(chan struct{}),
	}
	go m.gc()
	return m, nil
}

// MasterKeyLen is the required length of the store's root key.
const MasterKeyLen = 32

// Encrypted reports whether shards are opened at rest encrypted.
func (m *Manager) Encrypted() bool { return m.master != nil }

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
		p, err := ParsePrincipal(f.Principal)
		if err != nil {
			return err
		}
		s, err := m.Get(ctx, p, f.Namespace)
		if err != nil {
			return err
		}
		return s.applyFrame(f)
	})
}

// Get returns the open shard for (principal, ns), creating it on disk if
// needed. The returned Shard is safe for concurrent use.
func (m *Manager) Get(ctx context.Context, p Principal, ns string) (*Shard, error) {
	if m.closed.Load() {
		return nil, ErrClosed
	}
	if err := ValidName(ns); err != nil {
		return nil, fmt.Errorf("store.Get: namespace: %w", err)
	}
	if err := p.Valid(); err != nil {
		return nil, err
	}
	k := shardKey{principal: p, ns: ns}
	m.mu.Lock()
	if s, ok := m.shards[k]; ok {
		s.touch()
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	dir := filepath.Join(m.rootDir, p.dir())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store.Get: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, ns+".db")
	dek, err := shardDEK(path, p, m.master)
	if err != nil {
		return nil, err
	}
	s, err := openShard(path, p, ns, dek)
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

// ListPrincipals enumerates every tenant that owns at least one shard.
// Used by the cron sweeper, which must see EVERY tenant's schedules from
// the root engine.
func (m *Manager) ListPrincipals(ctx context.Context) ([]Principal, error) {
	var out []Principal
	err := m.walkTenants(m.rootDir, nil, &out)
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// walkTenants descends exactly Depth directory levels, collecting the
// principal each leaf directory encodes.
func (m *Manager) walkTenants(dir string, legs []string, out *[]Principal) error {
	if len(legs) == Depth {
		p, err := ParsePrincipal(strings.Join(legs, "/"))
		if err != nil {
			// A directory that does not encode a principal is not ours.
			return nil
		}
		*out = append(*out, p)
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := m.walkTenants(filepath.Join(dir, e.Name()), append(legs, e.Name()), out); err != nil {
			return err
		}
	}
	return nil
}

// ListShards enumerates every namespace shard the principal owns. Used by
// cross-namespace operations like ListNamespaces().
func (m *Manager) ListShards(ctx context.Context, p Principal) ([]*Shard, error) {
	dir := filepath.Join(m.rootDir, p.dir())
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
		s, err := m.Get(ctx, p, ns)
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

// gc seals and closes idle shards every minute. Sealing every resident
// shard on the same tick bounds how much an encrypted shard can lose to an
// unclean exit (see Shard.Checkpoint); it is a no-op for a shard that
// persists per commit.
func (m *Manager) gc() {
	t := time.NewTicker(SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-m.stopGC:
			return
		case <-t.C:
			m.evictIdle()
			m.checkpointAll()
		}
	}
}

// SweepEvery is how often idle shards are evicted and resident shards
// sealed. Mutable for tests.
var SweepEvery = time.Minute

// checkpointAll seals every resident shard.
func (m *Manager) checkpointAll() {
	m.mu.Lock()
	live := make([]*Shard, 0, len(m.shards))
	for _, s := range m.shards {
		live = append(live, s)
	}
	m.mu.Unlock()
	for _, s := range live {
		_ = s.Checkpoint()
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

// ShardPath returns the on-disk file for (principal, ns).
func (m *Manager) ShardPath(p Principal, ns string) string {
	return filepath.Join(m.rootDir, p.dir(), ns+".db")
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
