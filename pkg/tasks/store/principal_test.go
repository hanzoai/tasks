// Copyright © 2026 Hanzo AI. MIT License.

package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/hanzoai/tasks/pkg/tasks/store"
)

// A principal's encoding must be injective: no two distinct tenants may
// share a directory, or one would read the other's shards.
func TestPrincipal_EncodingInjective(t *testing.T) {
	tenants := []store.Principal{
		{},
		{Org: "acme"},
		{Org: "acme", Project: "web"},
		{Org: "acme", User: "z"},
		{Org: "acme", Project: "web", User: "z"},
		{Org: "acme", Project: "z"},
		{Org: "web", Project: "acme"},
	}
	seen := map[string]store.Principal{}
	for _, p := range tenants {
		s := p.String()
		if prev, dup := seen[s]; dup {
			t.Fatalf("%+v and %+v both encode to %q", prev, p, s)
		}
		seen[s] = p
		back, err := store.ParsePrincipal(s)
		if err != nil {
			t.Fatalf("ParsePrincipal(%q): %v", s, err)
		}
		if back != p {
			t.Fatalf("round trip %q: got %+v want %+v", s, back, p)
		}
	}
	// The two shapes a single-segment layout would have collapsed.
	user := store.Principal{Org: "acme", User: "z"}
	project := store.Principal{Org: "acme", Project: "z"}
	if user.String() == project.String() {
		t.Fatal("a user and a project of the same name collide")
	}
}

func TestPrincipal_Rejected(t *testing.T) {
	for _, c := range []struct {
		p    store.Principal
		want bool // valid?
	}{
		{store.Principal{}, true},
		{store.Principal{Org: "acme"}, true},
		{store.Principal{Org: "acme", User: "z"}, true}, // a user needs no project
		{store.Principal{Project: "web"}, false},        // narrows under an unset org
		{store.Principal{User: "z"}, false},             // narrows under an unset org
		{store.Principal{Org: "a/b"}, false},            // path separator
		{store.Principal{Org: ".."}, false},             // traversal
		{store.Principal{Org: store.Sentinel}, false},   // the unset-leg sentinel
	} {
		err := c.p.Valid()
		if c.want && err != nil {
			t.Fatalf("%+v should be valid, got %v", c.p, err)
		}
		if !c.want && err == nil {
			t.Fatalf("%+v should be rejected", c.p)
		}
	}
	if _, err := store.ParsePrincipal("acme/web"); err == nil {
		t.Fatal("a principal with the wrong number of legs should be rejected")
	}
}

// The KEK must separate every tenant, and narrowing must derive a child of
// the leg to its left rather than a fresh key off the master.
func TestPrincipal_KEKSeparatesTenants(t *testing.T) {
	master := bytes.Repeat([]byte{0x5a}, store.MasterKeyLen)
	tenants := []store.Principal{
		{},
		{Org: "acme"},
		{Org: "acme", Project: "web"},
		{Org: "acme", User: "z"},
		{Org: "acme", Project: "web", User: "z"},
		{Org: "other"},
	}
	seen := map[string]store.Principal{}
	for _, p := range tenants {
		k, err := p.KEK(master)
		if err != nil {
			t.Fatalf("KEK(%+v): %v", p, err)
		}
		if len(k) != store.MasterKeyLen {
			t.Fatalf("KEK(%+v) is %d bytes", p, len(k))
		}
		if prev, dup := seen[string(k)]; dup {
			t.Fatalf("%+v and %+v derive the same key", prev, p)
		}
		seen[string(k)] = p
	}
	// A different master must move every key.
	other := bytes.Repeat([]byte{0x5b}, store.MasterKeyLen)
	for _, p := range tenants {
		k, _ := p.KEK(other)
		if _, collides := seen[string(k)]; collides {
			t.Fatalf("%+v derives the same key under a different master", p)
		}
	}
}

// Two tenants that name the same namespace must not share a shard.
func TestManager_TenantsAreIsolated(t *testing.T) {
	mgr, err := store.New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()

	tenants := []store.Principal{
		{},
		{Org: "acme"},
		{Org: "acme", Project: "web"},
		{Org: "acme", User: "z"},
		{Org: "acme", Project: "web", User: "z"},
		{Org: "other"},
	}
	for _, p := range tenants {
		sh, err := mgr.Get(ctx, p, "default")
		if err != nil {
			t.Fatalf("Get(%+v): %v", p, err)
		}
		if err := sh.Put(ctx, "wf/default/a/1", []byte(p.String())); err != nil {
			t.Fatalf("Put(%+v): %v", p, err)
		}
	}
	for _, p := range tenants {
		sh, _ := mgr.Get(ctx, p, "default")
		got, ok, err := sh.Get(ctx, "wf/default/a/1")
		if err != nil || !ok {
			t.Fatalf("Get(%+v): ok=%v err=%v", p, ok, err)
		}
		if string(got) != p.String() {
			t.Fatalf("%+v read %q — it is sharing a shard with another tenant", p, got)
		}
	}

	found, err := mgr.ListPrincipals(ctx)
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	if len(found) != len(tenants) {
		t.Fatalf("ListPrincipals = %v, want %d tenants", found, len(tenants))
	}
}

// A namespace is created on demand — nothing declares it first.
func TestManager_NamespaceOnDemand(t *testing.T) {
	mgr, err := store.New(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	ctx := context.Background()
	p := store.Org("acme")
	for _, ns := range []string{"default", "staging", "one-off"} {
		sh, err := mgr.Get(ctx, p, ns)
		if err != nil {
			t.Fatalf("Get(%q): %v", ns, err)
		}
		if err := sh.Put(ctx, "wf/"+ns+"/a/1", []byte(ns)); err != nil {
			t.Fatalf("Put(%q): %v", ns, err)
		}
	}
	shards, err := mgr.ListShards(ctx, p)
	if err != nil {
		t.Fatalf("ListShards: %v", err)
	}
	if len(shards) != 3 {
		t.Fatalf("ListShards = %d shards, want 3", len(shards))
	}
	// A namespace that cannot address a file is still refused.
	for _, bad := range []string{"", "..", "a/b", store.Sentinel} {
		if _, err := mgr.Get(ctx, p, bad); err == nil {
			t.Fatalf("namespace %q should be refused", bad)
		}
	}
}
