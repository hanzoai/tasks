// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// tenancySecret is the value the at-rest tests look for on disk.
var tenancySecret = "workflow-id-that-must-not-appear-on-disk-7c3e1b"

func embedFixture(t *testing.T, dir string, master []byte) *Embedded {
	t.Helper()
	emb, err := Embed(context.Background(), EmbedConfig{DataDir: dir, MasterKey: master})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	t.Cleanup(func() { _ = emb.Stop(context.Background()) })
	return emb
}

// A tenant may start a workflow in a namespace nothing ever registered.
func TestTenancy_NamespaceOnDemand(t *testing.T) {
	emb := embedFixture(t, t.TempDir(), nil)
	for _, p := range []Principal{
		{},
		Org("acme"),
		{Org: "acme", Project: "web"},
	} {
		v := emb.View(p)
		wf, err := v.en.StartWorkflow("never-declared", tenancySecret, "", TypeRef{Name: "Demo"}, "default", nil)
		if err != nil {
			t.Fatalf("StartWorkflow(%+v): %v", p, err)
		}
		if wf.Execution.WorkflowId != tenancySecret {
			t.Fatalf("got workflow %q", wf.Execution.WorkflowId)
		}
		got, err := v.ListWorkflows("never-declared")
		if err != nil || len(got) != 1 {
			t.Fatalf("ListWorkflows(%+v): %d rows, err=%v", p, len(got), err)
		}
	}
}

// A user-scoped tenant is refused outright. The shared namespace layout
// names an org and an org's project and nothing narrower, so the depth
// tasks used to address by hand no longer has a place to be written — and
// it fails here, at the start, rather than by quietly landing in the org's
// own shard.
func TestTenancy_UserScopedIsRefused(t *testing.T) {
	emb := embedFixture(t, t.TempDir(), nil)
	for _, p := range []Principal{
		{Org: "acme", User: "z"},
		{Org: "acme", Project: "web", User: "z"},
	} {
		if _, err := emb.View(p).en.StartWorkflow("default", "wf", "", TypeRef{Name: "Demo"}, "default", nil); err == nil {
			t.Fatalf("StartWorkflow(%+v) should be refused", p)
		}
	}
}

// Each tenant sees only its own workflows, even in the same namespace.
func TestTenancy_ViewsAreIsolated(t *testing.T) {
	emb := embedFixture(t, t.TempDir(), nil)
	tenants := []Principal{
		{},
		Org("acme"),
		{Org: "acme", Project: "web"},
		{Org: "acme", Project: "api"},
		Org("other"),
	}
	for _, p := range tenants {
		if _, err := emb.View(p).en.StartWorkflow("default", "wf-"+p.String(), "", TypeRef{Name: "Demo"}, "default", nil); err != nil {
			t.Fatalf("StartWorkflow(%+v): %v", p, err)
		}
	}
	for _, p := range tenants {
		rows, err := emb.View(p).ListWorkflows("default")
		if err != nil {
			t.Fatalf("ListWorkflows(%+v): %v", p, err)
		}
		if len(rows) != 1 {
			t.Fatalf("%+v sees %d workflows, want 1 — tenants are sharing a shard", p, len(rows))
		}
		if want := "wf-" + p.String(); rows[0].Execution.WorkflowId != want {
			t.Fatalf("%+v sees %q, want %q", p, rows[0].Execution.WorkflowId, want)
		}
	}
}

// A master key encrypts every shard the engine writes, at every tenancy
// depth, and the workflow survives a restart under the same key.
func TestTenancy_EncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x7e}, 32)
	tenants := []Principal{{}, Org("acme"), {Org: "acme", Project: "web"}}

	emb := embedFixture(t, dir, key)
	for _, p := range tenants {
		if _, err := emb.View(p).en.StartWorkflow("default", tenancySecret, "", TypeRef{Name: "Demo"}, "default", nil); err != nil {
			t.Fatalf("StartWorkflow(%+v): %v", p, err)
		}
	}
	if err := emb.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Nothing under the data directory holds the workflow id in the clear.
	shards := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte(tenancySecret)) {
			t.Fatalf("%s holds the workflow id in the clear", path)
		}
		if filepath.Ext(path) == ".db" {
			shards++
			if bytes.HasPrefix(raw, []byte("SQLite format 3\x00")) {
				t.Fatalf("%s is a plaintext SQLite database", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if shards != len(tenants) {
		t.Fatalf("found %d shard files, want %d", shards, len(tenants))
	}

	// The same key reads every tenant's workflow back.
	again := embedFixture(t, dir, key)
	for _, p := range tenants {
		rows, err := again.View(p).ListWorkflows("default")
		if err != nil {
			t.Fatalf("ListWorkflows(%+v) after restart: %v", p, err)
		}
		if len(rows) != 1 || rows[0].Execution.WorkflowId != tenancySecret {
			t.Fatalf("%+v lost its workflow across restart: %+v", p, rows)
		}
	}
}

// A shard cannot be opened under a master key that is not its own.
func TestTenancy_WrongMasterRefused(t *testing.T) {
	dir := t.TempDir()
	emb := embedFixture(t, dir, bytes.Repeat([]byte{0x01}, 32))
	if _, err := emb.View(Org("acme")).en.StartWorkflow("default", "wf-a", "", TypeRef{Name: "Demo"}, "default", nil); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	if err := emb.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	wrong, err := Embed(context.Background(), EmbedConfig{DataDir: dir, MasterKey: bytes.Repeat([]byte{0x02}, 32)})
	if err != nil {
		// Refusing at boot is the strongest outcome: the root shard is
		// already unreadable.
		return
	}
	t.Cleanup(func() { _ = wrong.Stop(context.Background()) })
	if _, err := wrong.View(Org("acme")).ListWorkflows("default"); err == nil {
		t.Fatal("a shard read under the wrong master must fail, not return an empty result")
	}
}

// A malformed master key is refused where it enters.
func TestTenancy_MasterKeyLength(t *testing.T) {
	if _, err := Embed(context.Background(), EmbedConfig{DataDir: t.TempDir(), MasterKey: []byte("short")}); err == nil {
		t.Fatal("a short master key should be refused")
	}
}
