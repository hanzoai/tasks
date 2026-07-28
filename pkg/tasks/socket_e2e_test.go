// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/tasks"
)

// The engine serves, and the SDK reaches it, over a unix socket — the
// shape internal service-to-service traffic should take when both ends
// share a host. No port is involved anywhere.
func TestE2E_UnixSocket(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sock := filepath.Join(t.TempDir(), "tasks.sock")
	emb, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: sock})
	if err != nil {
		t.Fatalf("Embed on %s: %v", sock, err)
	}
	defer emb.Stop(context.Background())

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("engine did not bind the socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket", sock)
	}
	if emb.Address() != sock {
		t.Fatalf("Address() = %q want %q", emb.Address(), sock)
	}

	c, err := client.Dial(client.Options{
		Address:     sock,
		Namespace:   "default",
		DialTimeout: 5 * time.Second,
		CallTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Dial %s: %v", sock, err)
	}
	defer c.Close()

	health, err := c.CheckHealth(ctx, &client.CheckHealthRequest{})
	if err != nil {
		t.Fatalf("CheckHealth over unix socket: %v", err)
	}
	if health.Status != "ok" {
		t.Fatalf("health = %+v", health)
	}

	// A real workflow, started and read back over the socket.
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "wf-over-socket",
		TaskQueue: "default",
	}, "Demo")
	if err != nil {
		t.Fatalf("ExecuteWorkflow over unix socket: %v", err)
	}
	if run.GetID() != "wf-over-socket" {
		t.Fatalf("run id = %q", run.GetID())
	}
	desc, err := c.DescribeWorkflow(ctx, "wf-over-socket", run.GetRunID())
	if err != nil {
		t.Fatalf("DescribeWorkflow over unix socket: %v", err)
	}
	if desc.WorkflowID != "wf-over-socket" {
		t.Fatalf("described %+v", desc)
	}
}

// The socket is unlinked when the engine stops, so a restart at the same
// path binds cleanly.
func TestE2E_UnixSocketRebind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sock := filepath.Join(dir, "tasks.sock")
	for i := 0; i < 3; i++ {
		emb, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: sock, DataDir: dir})
		if err != nil {
			t.Fatalf("bind %d on %s: %v", i, sock, err)
		}
		if err := emb.Stop(ctx); err != nil {
			t.Fatalf("stop %d: %v", i, err)
		}
	}
}
