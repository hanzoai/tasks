// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/worker"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	"github.com/hanzoai/tasks/pkg/tasks"
	luxlog "github.com/luxfi/log"
)

// freePort picks an OS-assigned port and closes the listener, returning
// the port number. There is a race between close and ZAP bind; for a
// test that's acceptable.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

var e2eActivityCalls int32

func E2EGreet(ctx context.Context, who string) (string, error) {
	atomic.AddInt32(&e2eActivityCalls, 1)
	return "hello, " + who, nil
}

func E2EHello(ctx workflow.Context, name string) (string, error) {
	ao := workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Second}
	ctx = workflow.WithActivityOptions(ctx, ao)
	var out string
	if err := workflow.ExecuteActivity(ctx, E2EGreet, name).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

// TestE2E_WorkflowExecutesActivityAndCompletes proves the native ZAP
// engine actually runs workflows: client starts a workflow, server
// pushes the task to a registered worker, the workflow body runs and
// schedules an activity, server pushes the activity, the activity
// returns a result, the result is pushed back to the workflow, the
// workflow returns, and DescribeWorkflow reports COMPLETED.
//
// Anything weaker than this (control-plane round-trips, fake
// workflows) is not what shipped — this is the end-to-end path that
// drives the wind-down workflow tomorrow.
func TestE2E_WorkflowExecutesActivityAndCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	port := freePort(t)
	emb, err := tasks.Embed(ctx, tasks.EmbedConfig{ZAPPort: port})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(ctx)

	cli, err := client.Dial(client.Options{
		HostPort:    fmt.Sprintf("127.0.0.1:%d", port),
		Namespace:   "default",
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	atomic.StoreInt32(&e2eActivityCalls, 0)

	w := worker.New(cli, "default", worker.Options{
		MaxConcurrentWorkflowTaskPollers:       1,
		MaxConcurrentActivityExecutionSize:     1,
		MaxConcurrentWorkflowTaskExecutionSize: 1,
		Logger:                                 luxlog.New(),
	})
	w.RegisterWorkflowWithOptions(E2EHello, worker.RegisterWorkflowOptions{Name: "Hello"})
	w.RegisterActivity(E2EGreet)

	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()

	// Give Subscribe round-trips a moment to land. Without this the
	// EnqueueWorkflowTask call below races the Subscribe; the
	// dispatcher correctly queues, but the test wants delivery to be
	// observed on the wire path.
	time.Sleep(100 * time.Millisecond)

	run, err := cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "e2e-hello-" + time.Now().UTC().Format("150405.000000"),
		TaskQueue: "default",
	}, "Hello", "world")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Poll until terminal — the wire is async; the worker dispatches
	// the workflow task on a goroutine and the activity completion
	// flows back through the dispatcher push path.
	deadline := time.Now().Add(15 * time.Second)
	var lastStatus client.WorkflowStatus
	for time.Now().Before(deadline) {
		info, err := cli.DescribeWorkflow(ctx, run.GetID(), run.GetRunID())
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		lastStatus = info.Status
		if info.Status == client.WorkflowStatusCompleted {
			break
		}
		if info.Status == client.WorkflowStatusFailed ||
			info.Status == client.WorkflowStatusTimedOut ||
			info.Status == client.WorkflowStatusTerminated {
			t.Fatalf("workflow ended in non-success terminal state: %d (info=%+v)", info.Status, info)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if lastStatus != client.WorkflowStatusCompleted {
		t.Fatalf("workflow did not complete; last status=%d", lastStatus)
	}
	if got := atomic.LoadInt32(&e2eActivityCalls); got != 1 {
		t.Fatalf("expected exactly 1 activity invocation, got %d", got)
	}
}
