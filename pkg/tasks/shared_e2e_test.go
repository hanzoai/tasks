// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	luxlog "github.com/luxfi/log"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/worker"
	"github.com/hanzoai/tasks/pkg/tasks"
)

// A crash recovered from a shared SQL store: the engine that takes over has a fresh,
// empty data directory, so everything it recovers came from the server. Runs where
// TASKS_SQL_ADDR names a test server.
func TestDurable_CrashRecoveryFromSharedSQL(t *testing.T) {
	addr := os.Getenv("TASKS_SQL_ADDR")
	if addr == "" {
		t.Skip("TASKS_SQL_ADDR unset")
	}
	t.Setenv("TASKSD_STORE", "sql")
	t.Setenv("TASKSD_SQL_ADDR", addr)
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	ctx := context.Background()
	recovered.Store(false)
	recoverDone.Store(0)

	portA := freePort(t)
	embA, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: fmt.Sprintf(":%d", portA), DataDir: t.TempDir(), MasterKey: master})
	if err != nil {
		t.Fatalf("embed A: %v", err)
	}
	cliA := dialLocal(t, portA)
	wA := worker.New(cliA, "default", worker.Options{Logger: luxlog.New()})
	wA.RegisterWorkflowWithOptions(RecoverWorkflow, worker.RegisterWorkflowOptions{Name: "RecoverWorkflow"})
	wA.RegisterActivity(RecoverGreetActivity)
	if err := wA.Start(); err != nil {
		t.Fatalf("worker A start: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	run, err := cliA.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "recover-shared-wf", TaskQueue: "default"}, "RecoverWorkflow")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	select {
	case <-recoverStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("activity never started on worker A")
	}
	cliA.Close()
	if err := embA.Stop(ctx); err != nil {
		t.Fatalf("stop A: %v", err)
	}

	recovered.Store(true)
	portB := freePort(t)
	embB, err := tasks.Embed(ctx, tasks.EmbedConfig{Address: fmt.Sprintf(":%d", portB), DataDir: t.TempDir(), MasterKey: master})
	if err != nil {
		t.Fatalf("embed B: %v", err)
	}
	defer embB.Stop(ctx)
	cliB := dialLocal(t, portB)
	defer cliB.Close()
	wB := worker.New(cliB, "default", worker.Options{Logger: luxlog.New()})
	wB.RegisterWorkflowWithOptions(RecoverWorkflow, worker.RegisterWorkflowOptions{Name: "RecoverWorkflow"})
	wB.RegisterActivity(RecoverGreetActivity)
	if err := wB.Start(); err != nil {
		t.Fatalf("worker B start: %v", err)
	}
	defer wB.Stop()
	if got := waitStatus(t, ctx, cliB, run.GetID(), run.GetRunID(), client.WorkflowStatusCompleted, 20*time.Second); got != client.WorkflowStatusCompleted {
		t.Fatalf("workflow did not complete after recovery from the shared store; status=%d", got)
	}
	if n := recoverDone.Load(); n != 1 {
		t.Fatalf("activity completed %d times, want exactly 1", n)
	}
}
