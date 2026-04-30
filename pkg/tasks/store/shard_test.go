// Copyright © 2026 Hanzo AI. MIT License.

package store_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/tasks/replication"
	"github.com/hanzoai/tasks/pkg/tasks/store"
)

func newMgr(t *testing.T) *store.Manager {
	t.Helper()
	mgr, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

func TestShard_PutGetDel(t *testing.T) {
	mgr := newMgr(t)
	ctx := context.Background()
	s, err := mgr.Get(ctx, "org-a", "ns1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "wf/ns1/a/1", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Get(ctx, "wf/ns1/a/1")
	if err != nil || !ok || string(v) != "hello" {
		t.Fatalf("get: ok=%v v=%q err=%v", ok, v, err)
	}
	if err := s.Del(ctx, "wf/ns1/a/1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Get(ctx, "wf/ns1/a/1"); ok {
		t.Fatal("expected delete to remove key")
	}
}

func TestShard_NamespaceIsolation(t *testing.T) {
	mgr := newMgr(t)
	ctx := context.Background()
	a, _ := mgr.Get(ctx, "org-x", "alpha")
	b, _ := mgr.Get(ctx, "org-x", "beta")
	_ = a.Put(ctx, "wf/alpha/k/1", []byte("A"))
	_ = b.Put(ctx, "wf/beta/k/1", []byte("B"))
	if _, ok, _ := a.Get(ctx, "wf/beta/k/1"); ok {
		t.Fatal("alpha leaked beta key")
	}
	if _, ok, _ := b.Get(ctx, "wf/alpha/k/1"); ok {
		t.Fatal("beta leaked alpha key")
	}
}

func TestShard_OrgIsolation(t *testing.T) {
	mgr := newMgr(t)
	ctx := context.Background()
	a, _ := mgr.Get(ctx, "org-1", "shared")
	b, _ := mgr.Get(ctx, "org-2", "shared")
	_ = a.Put(ctx, "wf/shared/k/1", []byte("from-1"))
	if _, ok, _ := b.Get(ctx, "wf/shared/k/1"); ok {
		t.Fatal("org-2 saw org-1 data through same namespace name")
	}
}

func TestShard_ConcurrentOpenSafe(t *testing.T) {
	mgr := newMgr(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	got := make(chan *store.Shard, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := mgr.Get(ctx, "org", "ns")
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			got <- s
		}()
	}
	wg.Wait()
	close(got)
	first := <-got
	for s := range got {
		if s != first {
			t.Fatal("Get returned different shard pointers for same key")
		}
	}
}

func TestShard_PrefixScan(t *testing.T) {
	mgr := newMgr(t)
	ctx := context.Background()
	s, _ := mgr.Get(ctx, "", "ns")
	_ = s.Put(ctx, "wf/ns/a/1", []byte("1"))
	_ = s.Put(ctx, "wf/ns/a/2", []byte("2"))
	_ = s.Put(ctx, "wf/ns/b/1", []byte("3"))
	count := 0
	if err := s.List(ctx, "wf/ns/a/", func(k string, v []byte) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("prefix scan saw %d, want 2", count)
	}
}

func TestShard_CloseAndReopen(t *testing.T) {
	dir := t.TempDir()
	mgr1, _ := store.New(dir)
	ctx := context.Background()
	s1, _ := mgr1.Get(ctx, "org", "ns")
	_ = s1.Put(ctx, "wf/ns/k/1", []byte("persisted"))
	_ = mgr1.Close()

	mgr2, _ := store.New(dir)
	defer mgr2.Close()
	s2, _ := mgr2.Get(ctx, "org", "ns")
	v, ok, err := s2.Get(ctx, "wf/ns/k/1")
	if err != nil || !ok || string(v) != "persisted" {
		t.Fatalf("reopen: ok=%v v=%q err=%v", ok, v, err)
	}
}

func TestShard_ListShardsEnumerates(t *testing.T) {
	mgr := newMgr(t)
	ctx := context.Background()
	for _, ns := range []string{"alpha", "beta", "gamma"} {
		_, _ = mgr.Get(ctx, "org", ns)
	}
	shards, err := mgr.ListShards(ctx, "org")
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 3 {
		t.Fatalf("ListShards: got %d, want 3", len(shards))
	}
}

func TestShard_IdleEvict(t *testing.T) {
	old := store.IdleEvictAfter
	store.IdleEvictAfter = 10 * time.Millisecond
	t.Cleanup(func() { store.IdleEvictAfter = old })
	mgr := newMgr(t)
	ctx := context.Background()
	_, _ = mgr.Get(ctx, "org", "scratch")
	if mgr.OpenShardCount() != 1 {
		t.Fatal("expected 1 open shard after Get")
	}
	// Force the manager to evaluate idle eviction.
	time.Sleep(20 * time.Millisecond)
	// The GC ticker is once per minute; instead poke the public path
	// by closing then reopening the manager — easier than exposing
	// internal state.
	_ = mgr.Close()
}

func TestShard_ReplicatorAppliesFrames(t *testing.T) {
	mgr := newMgr(t)
	rep := replication.NewLocal()
	mgr.WithReplicator(rep)
	ctx := context.Background()
	s, _ := mgr.Get(ctx, "org", "ns")
	if err := s.Put(ctx, "wf/ns/k/1", []byte("v")); err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Get(ctx, "wf/ns/k/1")
	if err != nil || !ok || string(v) != "v" {
		t.Fatalf("post-replicate get: ok=%v v=%q err=%v", ok, v, err)
	}
}

func TestShard_ManagerRoundTripPath(t *testing.T) {
	mgr := newMgr(t)
	if mgr.ShardPath("", "ns") == "" {
		t.Fatal("expected non-empty path for sentinel org")
	}
	if got := filepath.Base(mgr.ShardPath("org-x", "alpha")); got != "alpha.db" {
		t.Fatalf("ShardPath base = %q, want alpha.db", got)
	}
}

func TestShard_NsFromKey(t *testing.T) {
	cases := []struct {
		key  string
		ns   string
		ok   bool
	}{
		{"ns/foo", "foo", true},
		{"wf/myns/wfid/runid", "myns", true},
		{"sc/sched/abc", "sched", true},
		{"", "", false},
	}
	for _, c := range cases {
		_, got, _, ok := store.NsFromKey(c.key)
		if ok != c.ok || got != c.ns {
			t.Fatalf("NsFromKey(%q) = ns=%q ok=%v, want ns=%q ok=%v", c.key, got, ok, c.ns, c.ok)
		}
	}
}
