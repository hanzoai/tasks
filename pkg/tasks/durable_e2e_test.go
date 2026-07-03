// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/hanzoai/tasks/pkg/sdk/worker"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	"github.com/hanzoai/tasks/pkg/tasks"
	luxlog "github.com/luxfi/log"
)

// Full-stack Phase-2a tests: server-side activity retry+backoff and
// single-replica crash-recovery, driven through the real Embed + client +
// worker loop.

func dialLocal(t *testing.T, port int) client.Client {
	t.Helper()
	cli, err := client.Dial(client.Options{
		HostPort:    fmt.Sprintf("127.0.0.1:%d", port),
		Namespace:   "default",
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return cli
}

func waitStatus(t *testing.T, ctx context.Context, cli client.Client, id, runID string, want client.WorkflowStatus, within time.Duration) client.WorkflowStatus {
	t.Helper()
	deadline := time.Now().Add(within)
	var last client.WorkflowStatus
	for time.Now().Before(deadline) {
		info, err := cli.DescribeWorkflow(ctx, id, runID)
		if err == nil {
			last = info.Status
			if last == want {
				return last
			}
			switch last {
			case client.WorkflowStatusCompleted, client.WorkflowStatusFailed,
				client.WorkflowStatusTerminated, client.WorkflowStatusTimedOut:
				if last != want {
					return last // reached a different terminal state
				}
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	return last
}

// ── retry + backoff ─────────────────────────────────────────────────────

var (
	flakyAttempts atomic.Int32
	flakyMu       sync.Mutex
	flakyTimes    []time.Time
)

// FlakyGreetActivity fails the first two attempts with a retryable error,
// then succeeds. It records the wall-clock time of each attempt so the test
// can assert the backoff delay grew.
func FlakyGreetActivity(ctx context.Context) (string, error) {
	n := flakyAttempts.Add(1)
	flakyMu.Lock()
	flakyTimes = append(flakyTimes, time.Now())
	flakyMu.Unlock()
	if n < 3 {
		return "", temporal.NewError("transient boom", "TransientError", false)
	}
	return "greeted", nil
}

func RetryWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    25 * time.Millisecond,
			BackoffCoefficient: 3.0,
			MaximumInterval:    2 * time.Second,
			MaximumAttempts:    5,
		},
	})
	var out string
	if err := workflow.ExecuteActivity(ctx, FlakyGreetActivity).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

func TestDurable_ActivityRetryBackoff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	flakyAttempts.Store(0)
	flakyMu.Lock()
	flakyTimes = nil
	flakyMu.Unlock()

	port := freePort(t)
	emb, err := tasks.Embed(ctx, tasks.EmbedConfig{ZAPPort: port})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(ctx)

	cli := dialLocal(t, port)
	defer cli.Close()

	w := worker.New(cli, "default", worker.Options{Logger: luxlog.New()})
	w.RegisterWorkflowWithOptions(RetryWorkflow, worker.RegisterWorkflowOptions{Name: "RetryWorkflow"})
	w.RegisterActivity(FlakyGreetActivity)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()
	time.Sleep(100 * time.Millisecond)

	run, err := cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "retry-" + time.Now().UTC().Format("150405.000000"), TaskQueue: "default",
	}, "RetryWorkflow")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got := waitStatus(t, ctx, cli, run.GetID(), run.GetRunID(), client.WorkflowStatusCompleted, 15*time.Second); got != client.WorkflowStatusCompleted {
		t.Fatalf("workflow did not complete; status=%d attempts=%d", got, flakyAttempts.Load())
	}

	if n := flakyAttempts.Load(); n != 3 {
		t.Fatalf("activity attempts = %d, want 3 (2 failures then success)", n)
	}
	flakyMu.Lock()
	times := append([]time.Time(nil), flakyTimes...)
	flakyMu.Unlock()
	if len(times) != 3 {
		t.Fatalf("recorded %d attempt times, want 3", len(times))
	}
	gap1 := times[1].Sub(times[0]) // ~InitialInterval (25ms)
	gap2 := times[2].Sub(times[1]) // ~InitialInterval * Backoff (75ms)
	// Lower bounds prove the backoff was applied; gap2 > gap1 proves it grew.
	if gap1 < 12*time.Millisecond {
		t.Errorf("first retry gap = %v, want >= ~InitialInterval", gap1)
	}
	if gap2 < 50*time.Millisecond {
		t.Errorf("second retry gap = %v, want >= ~InitialInterval*Backoff", gap2)
	}
	if gap2 <= gap1 {
		t.Errorf("backoff did not grow: gap1=%v gap2=%v", gap1, gap2)
	}
}

// ── retry exhaustion → workflow fails ───────────────────────────────────

var exhaustAttempts atomic.Int32

func AlwaysFailActivity(ctx context.Context) (string, error) {
	exhaustAttempts.Add(1)
	return "", temporal.NewError("always boom", "TransientError", false)
}

func ExhaustWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    10 * time.Millisecond,
			BackoffCoefficient: 2.0,
			MaximumInterval:    100 * time.Millisecond,
			MaximumAttempts:    3,
		},
	})
	var out string
	if err := workflow.ExecuteActivity(ctx, AlwaysFailActivity).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

func TestDurable_ActivityRetryExhaustion_FailsWorkflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	exhaustAttempts.Store(0)

	port := freePort(t)
	emb, err := tasks.Embed(ctx, tasks.EmbedConfig{ZAPPort: port})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(ctx)

	cli := dialLocal(t, port)
	defer cli.Close()

	w := worker.New(cli, "default", worker.Options{Logger: luxlog.New()})
	w.RegisterWorkflowWithOptions(ExhaustWorkflow, worker.RegisterWorkflowOptions{Name: "ExhaustWorkflow"})
	w.RegisterActivity(AlwaysFailActivity)
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()
	time.Sleep(100 * time.Millisecond)

	run, err := cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "exhaust-" + time.Now().UTC().Format("150405.000000"), TaskQueue: "default",
	}, "ExhaustWorkflow")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got := waitStatus(t, ctx, cli, run.GetID(), run.GetRunID(), client.WorkflowStatusFailed, 15*time.Second); got != client.WorkflowStatusFailed {
		t.Fatalf("workflow status = %d, want FAILED (attempts=%d)", got, exhaustAttempts.Load())
	}
	if n := exhaustAttempts.Load(); n != 3 {
		t.Fatalf("activity attempts = %d, want 3 (MaximumAttempts)", n)
	}
}

// ── crash-recovery ──────────────────────────────────────────────────────

var (
	recovered        atomic.Bool
	recoverDone      atomic.Int32
	recoverStarted   = make(chan struct{})
	recoverGate      = make(chan struct{}) // never closed — models the crashed worker's lost goroutine
	recoverStartOnce sync.Once
)

// RecoverGreetActivity blocks its first (pre-crash) attempt forever so it
// never completes on worker A — modelling a worker whose goroutine is lost
// when the process crashes. After recovery flips `recovered`, worker B's
// attempt completes and counts exactly once.
func RecoverGreetActivity(ctx context.Context) (string, error) {
	if recovered.Load() {
		recoverDone.Add(1)
		return "recovered-ok", nil
	}
	recoverStartOnce.Do(func() { close(recoverStarted) })
	<-recoverGate // parks forever (the "crashed" attempt never responds)
	return "", context.Canceled
}

func RecoverWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var out string
	if err := workflow.ExecuteActivity(ctx, RecoverGreetActivity).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

func TestDurable_CrashRecovery_ReachesCompleted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	recovered.Store(false)
	recoverDone.Store(0)

	// Embed A + worker A: drive the activity in-flight, then simulate a crash.
	portA := freePort(t)
	embA, err := tasks.Embed(ctx, tasks.EmbedConfig{ZAPPort: portA, DataDir: dir})
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

	run, err := cliA.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: "recover-wf", TaskQueue: "default",
	}, "RecoverWorkflow")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	select {
	case <-recoverStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("activity never started on worker A")
	}

	// Crash: worker A's activity is blocked (do not Stop it). Close its client
	// and stop the server; the store directory survives.
	cliA.Close()
	if err := embA.Stop(ctx); err != nil {
		t.Fatalf("stop A: %v", err)
	}

	// Restart: Embed B recovers in-flight state from the same store; worker B
	// runs the now-deterministic activity to completion.
	recovered.Store(true)
	portB := freePort(t)
	embB, err := tasks.Embed(ctx, tasks.EmbedConfig{ZAPPort: portB, DataDir: dir})
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
		t.Fatalf("workflow did not complete after recovery; status=%d", got)
	}
	if n := recoverDone.Load(); n != 1 {
		t.Fatalf("activity completed %d times, want exactly 1 (no double execution of a recovered activity)", n)
	}
}
