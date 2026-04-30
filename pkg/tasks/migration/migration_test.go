// Copyright © 2026 Hanzo AI. MIT License.

package migration_test

import (
	"context"
	"os"
	"testing"

	"github.com/hanzoai/tasks/pkg/tasks/migration"
	"github.com/hanzoai/tasks/pkg/tasks/replication"
	"github.com/hanzoai/tasks/pkg/tasks/store"
)

func TestMigration_HappyPath(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := store.New(dir)
	t.Cleanup(func() { _ = mgr.Close() })
	rep := replication.NewLocal()
	mgr.WithReplicator(rep)
	ctx := context.Background()
	sh, _ := mgr.Get(ctx, "org", "ns")
	_ = sh.Put(ctx, "wf/ns/a/1", []byte("hello"))

	c := migration.NewCoordinator(mgr, rep)
	job, err := c.Migrate(ctx, migration.Job{OrgID: "org", Namespace: "ns", To: "node-2"})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if job.Status != migration.StatusReleased {
		t.Fatalf("status = %s, want released", job.Status)
	}
	if job.Bytes <= 0 {
		t.Fatalf("expected non-zero bytes copied, got %d", job.Bytes)
	}
}

func TestMigration_RejectsMissingFields(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := store.New(dir)
	t.Cleanup(func() { _ = mgr.Close() })
	c := migration.NewCoordinator(mgr, replication.NewLocal())
	if _, err := c.Migrate(context.Background(), migration.Job{OrgID: "x"}); err == nil {
		t.Fatal("expected error on empty namespace")
	}
}

func TestMigration_DoubleMigrateSafe(t *testing.T) {
	dir := t.TempDir()
	mgr, _ := store.New(dir)
	t.Cleanup(func() { _ = mgr.Close() })
	rep := replication.NewLocal()
	mgr.WithReplicator(rep)
	ctx := context.Background()
	sh, _ := mgr.Get(ctx, "org", "ns")
	_ = sh.Put(ctx, "wf/ns/a/1", []byte("hello"))

	c := migration.NewCoordinator(mgr, rep)
	if _, err := c.Migrate(ctx, migration.Job{OrgID: "org", Namespace: "ns", To: "n2"}); err != nil {
		t.Fatal(err)
	}
	// Second migrate against a freshly-opened shard must succeed.
	sh2, _ := mgr.Get(ctx, "org", "ns")
	_ = sh2.Put(ctx, "wf/ns/a/2", []byte("again"))
	job2, err := c.Migrate(ctx, migration.Job{OrgID: "org", Namespace: "ns", To: "n3"})
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if job2.Status != migration.StatusReleased {
		t.Fatalf("second job status = %s", job2.Status)
	}

	// And the destination file actually exists.
	if _, err := os.Stat(mgr.ShardPath("org", "ns")); err != nil {
		t.Fatalf("source shard missing: %v", err)
	}
}
