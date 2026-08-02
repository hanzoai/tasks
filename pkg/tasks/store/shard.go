// Copyright © 2026 Hanzo AI. MIT License.

// Package store implements per-(org, namespace) SQLite shards with
// optional consensus replication.
//
// Layout on disk — hanzoai/namespace's, shared with every other store in
// the estate rather than one of tasks' own:
//
//	<rootDir>/orgs/<org>/<namespace>.db
//	<rootDir>/orgs/<org>/projects/<project>/<namespace>.db
//	<rootDir>/orgs/_platform/<namespace>.db     the root (unscoped) tenant
//
// A shard is created on first use. Namespaces are not declared up front.
//
// A Principal that narrows to a USER has no place in this layout and is
// refused — see ErrUserLeg.
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
//	Every shard is encrypted at rest, keyed by hanzoai/cek from the process
//	master and the namespace that owns it. There is no plaintext posture and
//	no key material beside the file: a shard is born encrypted or it does not
//	exist. A deployment with no master mints a random one that dies with the
//	process, which is the honest shape of a keyless run.
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
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hanzoai/cek"
	"github.com/hanzoai/namespace"
	"github.com/hanzoai/tasks/pkg/tasks/replication"
)

// shardSuffix names a shard file. One namespace, one file, one suffix.
const shardSuffix = ".db"

// The segments hanzoai/namespace lays its tree out with, READ OUT OF
// namespace ITSELF rather than spelled again here.
//
// These strings are directory names on live volumes. A second spelling of
// one does not fail loudly — it opens an empty database beside the real
// one — so the only safe way to walk a layout you do not own is to ask the
// package that writes it where it put things.
var (
	orgsRoot, platformSlug = splitTail(mustKey(namespace.System()))
	projectsDir            = midSegment(mustKey(namespace.MustOrg("o").WithGroup(namespace.MustGroup("g"))))
)

// mustKey renders a namespace's directory. The inputs are fixed in the
// source, so a failure is a broken dependency, not a runtime condition.
func mustKey(ns namespace.Namespace) []string {
	key, err := namespace.Key(ns, "probe")
	if err != nil {
		panic("store: hanzoai/namespace cannot render its own layout: " + err.Error())
	}
	return strings.Split(path.Dir(key), "/")
}

// splitTail returns the first and last segments: "orgs", "_platform".
func splitTail(segs []string) (string, string) { return segs[0], segs[len(segs)-1] }

// midSegment returns the segment naming the project container in
// orgs/<org>/projects/<group>.
func midSegment(segs []string) string { return segs[len(segs)-2] }

// IdleEvictAfter sets how long an open shard may sit unused before the
// manager closes it. Mutable for tests.
var IdleEvictAfter = 10 * time.Minute

// ErrClosed is returned when a method runs after Close.
var ErrClosed = errors.New("store: manager closed")

// Manager owns the on-disk shard layout and the open-shard cache.
//
// It holds no key. The master lives in hanzoai/cek — one key per process,
// for every database the process opens — because threading it through every
// caller would not make it less global, only harder to see.
type Manager struct {
	rootDir    string
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
// master is the 32-byte root key every shard is derived from, resolved from
// KMS by whoever runs this. It is installed into hanzoai/cek, which is where
// the process keeps it.
//
// A nil master does NOT mean plaintext — there is no such posture any more.
// It means "I have no key to state":
//
//   - Embedded in a process that already installed one (cloud resolves it
//     through credz at boot), that key is used. One deployment, one key —
//     an embedded engine minting a second would write files its host could
//     not read.
//   - Otherwise a random master is minted for this process. Nothing it
//     writes outlives the run, by construction, which is the honest shape
//     of a deployment that was given no key and the reason a developer
//     needs no configuration at all.
func New(rootDir string, master []byte) (*Manager, error) {
	if rootDir == "" {
		return nil, fmt.Errorf("store.New: rootDir required")
	}
	switch {
	case master != nil:
		if len(master) != MasterKeyLen {
			return nil, fmt.Errorf("store.New: master key must be %d bytes, got %d", MasterKeyLen, len(master))
		}
		if err := cek.SetMaster(master); err != nil {
			return nil, fmt.Errorf("store.New: %w", err)
		}
	case !cek.HasMaster():
		if _, err := cek.SetDevMaster(); err != nil {
			return nil, fmt.Errorf("store.New: %w", err)
		}
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

// MasterKeyLen is the required length of the store's root key.
const MasterKeyLen = cek.KeyLen

// Encrypted reports whether shards are opened at rest encrypted. They always
// are; it answers whether this process holds a key to open any at all.
func (m *Manager) Encrypted() bool { return cek.HasMaster() }

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
	tenant, err := p.Namespace()
	if err != nil {
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

	s, err := openShard(tenant, m.rootDir, p, ns)
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
//
// It walks the namespace layout: orgs/_platform is the root tenant, every
// other orgs/<slug> is an org, and each of those may hold projects/<slug>.
// The slug it reads back is ALREADY the folded storage identity, so the
// Principal it builds renders straight back to the directory it came from
// (see Org for why folding twice would not).
func (m *Manager) ListPrincipals(ctx context.Context) ([]Principal, error) {
	var out []Principal
	orgs, err := subdirs(filepath.Join(m.rootDir, orgsRoot))
	if err != nil {
		return nil, err
	}
	for _, org := range orgs {
		p := Principal{Org: org}
		if org == platformSlug {
			p = Principal{}
		}
		if m.holdsShard(p) {
			out = append(out, p)
		}
		if org == platformSlug {
			continue // the deployment itself has no projects
		}
		projects, err := subdirs(filepath.Join(m.rootDir, orgsRoot, org, projectsDir))
		if err != nil {
			return nil, err
		}
		for _, proj := range projects {
			if q := (Principal{Org: org, Project: proj}); m.holdsShard(q) {
				out = append(out, q)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

// holdsShard reports whether p owns at least one shard. A directory that
// holds only projects/ is a container, not a tenant, and listing it would
// hand the sweeper a tenant with nothing in it.
func (m *Manager) holdsShard(p Principal) bool { return len(m.shardNames(p)) > 0 }

// shardNames is every namespace p owns, sorted: the shard files on disk,
// UNION the ones this process holds open.
//
// The union is not belt-and-braces, it is the answer. On the pure-Go
// SQLCipher envelope the ciphertext is written when the file is SEALED —
// at checkpoint or close — so a shard created moments ago has no file yet
// and a disk-only walk reports its tenant as having nothing. That is
// precisely the tenant the cron sweeper most needs to see. Reading the
// resident set as well makes the answer independent of which backend is
// linked and of when the last seal happened.
func (m *Manager) shardNames(p Principal) []string {
	seen := map[string]bool{}
	if dir, err := m.tenantDir(p); err == nil {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() || !strings.HasSuffix(name, shardSuffix) || strings.HasPrefix(name, "-") {
					continue
				}
				seen[strings.TrimSuffix(name, shardSuffix)] = true
			}
		}
	}
	m.mu.Lock()
	for k := range m.shards {
		if k.principal == p {
			seen[k.ns] = true
		}
	}
	m.mu.Unlock()
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// subdirs lists the directory names directly under dir. A missing dir is
// empty, not an error: nothing has been written yet.
func subdirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// tenantDir is the directory holding every shard p owns.
func (m *Manager) tenantDir(p Principal) (string, error) {
	path, err := m.shardPath(p, "any")
	if err != nil {
		return "", err
	}
	return filepath.Dir(path), nil
}

// ListShards enumerates every namespace shard the principal owns. Used by
// cross-namespace operations like ListNamespaces().
func (m *Manager) ListShards(ctx context.Context, p Principal) ([]*Shard, error) {
	names := m.shardNames(p)
	out := make([]*Shard, 0, len(names))
	for _, ns := range names {
		s, err := m.Get(ctx, p, ns)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
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

// ShardPath returns the on-disk file for (principal, ns), or "" for a
// principal the layout cannot name.
func (m *Manager) ShardPath(p Principal, ns string) string {
	path, err := m.shardPath(p, ns)
	if err != nil {
		return ""
	}
	return path
}

// shardPath renders (principal, ns) through namespace — the SAME rendering
// cek.Open uses to place the file, so the path a caller is handed is the
// path the shard was opened at and not a second guess at it.
func (m *Manager) shardPath(p Principal, ns string) (string, error) {
	tenant, err := p.Namespace()
	if err != nil {
		return "", err
	}
	return namespace.Path(m.rootDir, tenant, ns)
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
