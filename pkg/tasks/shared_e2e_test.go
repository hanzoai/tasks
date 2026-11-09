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
	"github.com/hanzoai/tasks/pkg/tasks/internal/dbtest"
	luxlog "github.com/luxfi/log"
)

// Two replicas of one engine, each with real workers, on one database. The tests
// kill one in the middle of a step and read the answer from the other.

// replica is one pod: an embedded engine on its own pool, and the worker and client
// that pod's apps hold.
type replica struct {
	emb  *tasks.Embedded
	cli  client.Client
	w    worker.Worker
	port int
}

// sharedReplica starts a replica on the test's database, with a pool of its own.
// register adds the workflows and activities this replica's worker serves.
func sharedReplica(t *testing.T, open dbtest.Open, name string, register func(worker.Worker)) *replica {
	t.Helper()
	db := open()
	port := freePort(t)
	emb, err := tasks.Embed(context.Background(), tasks.WithTiming(tasks.EmbedConfig{Address: fmt.Sprintf(":%d", port), Database: db, NodeID: name},
		200*time.Millisecond, 200*time.Millisecond, time.Second))
	if err != nil {
		t.Fatalf("embed %s: %v", name, err)
	}
	cli := dialLocal(t, port)
	w := worker.New(cli, "default", worker.Options{Logger: luxlog.New()})
	register(w)
	if err := w.Start(); err != nil {
		t.Fatalf("worker %s: %v", name, err)
	}
	t.Cleanup(func() { w.Stop(); cli.Close(); _ = emb.Stop(context.Background()) })
	return &replica{emb: emb, cli: cli, w: w, port: port}
}

var (
	handoverStarts atomic.Int32
	handoverDone   atomic.Int32
	handoverIn     = make(chan struct{}, 8)
	handoverBlock  chan struct{} // the doomed replica's activity never answers; the test frees it when it ends
)

// HandoverActivity blocks the first time it is run, which is on the replica that
// dies, and answers every time after.
func HandoverActivity(ctx context.Context) (string, error) {
	if handoverStarts.Add(1) == 1 {
		handoverIn <- struct{}{}
		<-handoverBlock
		return "", context.Canceled
	}
	handoverDone.Add(1)
	return "taken-over", nil
}

func HandoverWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var out string
	if err := workflow.ExecuteActivity(ctx, HandoverActivity).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

// resetHandover readies the doomed activity for a test: counts at zero, nothing
// waiting to be read, a block of its own.
func resetHandover() {
	handoverStarts.Store(0)
	handoverDone.Store(0)
	for len(handoverIn) > 0 {
		<-handoverIn
	}
	handoverBlock = make(chan struct{})
}

func registerHandover(w worker.Worker) {
	w.RegisterWorkflowWithOptions(HandoverWorkflow, worker.RegisterWorkflowOptions{Name: "HandoverWorkflow"})
	w.RegisterActivity(HandoverActivity)
}

// handover starts a workflow on a, waits until its activity is running on a, ends a
// with kill, and reads the workflow's end from b.
func handover(t *testing.T, open dbtest.Open, kill func(a *replica)) {
	t.Helper()
	resetHandover()
	a := sharedReplica(t, open, "a", registerHandover)
	b := sharedReplica(t, open, "b", registerHandover)
	t.Cleanup(func() { close(handoverBlock) }) // before the workers stop: Stop waits for the activity
	time.Sleep(100 * time.Millisecond)

	ctx := context.Background()
	run, err := a.cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "handover-wf", TaskQueue: "default"}, "HandoverWorkflow")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	select {
	case <-handoverIn:
	case <-time.After(10 * time.Second):
		t.Fatal("the activity never started on a")
	}
	kill(a)

	if got := waitStatus(t, ctx, b.cli, run.GetID(), run.GetRunID(), client.WorkflowStatusCompleted, 20*time.Second); got != client.WorkflowStatusCompleted {
		t.Fatalf("the workflow did not complete on b after a died; status=%d", got)
	}
	if n := handoverDone.Load(); n != 1 {
		t.Fatalf("the activity was answered %d times, want exactly 1", n)
	}
	if n := handoverStarts.Load(); n != 2 {
		t.Fatalf("the activity started %d times: the dead replica's attempt and the one that took over, want 2", n)
	}
	// b's own view of the run, from b's pool.
	info, err := b.cli.DescribeWorkflow(ctx, run.GetID(), run.GetRunID())
	if err != nil || info.Status != client.WorkflowStatusCompleted {
		t.Fatalf("describe from b: %v %v", info, err)
	}
}

// A replica is killed in the middle of a step: its membership lapses, and the other
// replica takes up the activity and carries the workflow to its end.
func TestShared_WorkflowSurvivesAKilledReplica(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		handover(t, open, func(a *replica) { a.emb.Crash() })
	})
}

// A replica leaves, as in a rolling update: the same, and sooner.
func TestShared_WorkflowSurvivesARollingUpdate(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		handover(t, open, func(a *replica) {
			if err := a.emb.Stop(context.Background()); err != nil {
				t.Fatalf("stop: %v", err)
			}
		})
	})
}

var quickRan sync.Map // workflow id -> number of times its activity ran

func QuickActivity(ctx context.Context, id string) (string, error) {
	n, _ := quickRan.LoadOrStore(id, new(atomic.Int32))
	n.(*atomic.Int32).Add(1)
	return id, nil
}

func QuickWorkflow(ctx workflow.Context, id string) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 30 * time.Second})
	var out string
	if err := workflow.ExecuteActivity(ctx, QuickActivity, id).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

func registerQuick(w worker.Worker) {
	w.RegisterWorkflowWithOptions(QuickWorkflow, worker.RegisterWorkflowOptions{Name: "QuickWorkflow"})
	w.RegisterActivity(QuickActivity)
}

// Workflows started on both replicas at once, each with an activity, and signalled
// from the other replica while they run, run every activity once: contention between
// replicas does not run anything twice.
func TestShared_NoActivityRunsTwiceUnderContention(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		noActivityRunsTwice(t, open)
	})
}

func noActivityRunsTwice(t *testing.T, open dbtest.Open) {
	reps := []*replica{
		sharedReplica(t, open, "a", registerQuick),
		sharedReplica(t, open, "b", registerQuick),
	}
	time.Sleep(100 * time.Millisecond)

	ctx := context.Background()
	const n = 40
	prefix := t.Name() + "/"
	type started struct {
		id  string
		run client.WorkflowRun
	}
	var (
		mu  sync.Mutex
		all []started
		wg  sync.WaitGroup
	)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%squick-%03d", prefix, i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, err := reps[i%2].cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: "default"}, "QuickWorkflow", id)
			if err != nil {
				t.Errorf("execute %s: %v", id, err)
				return
			}
			mu.Lock()
			all = append(all, started{id, run})
			mu.Unlock()
			// The other replica pokes the run while its owner works it: each signal
			// takes the run's lock and asks for a decision of its own.
			for k := 0; k < 3; k++ {
				_ = reps[(i+1)%2].cli.SignalWorkflow(ctx, id, run.GetRunID(), "poke", nil)
			}
		}()
	}
	wg.Wait()
	for _, s := range all {
		// Read each from the replica that did not start it.
		other := reps[1]
		if s.id[len(s.id)-1]%2 == 1 {
			other = reps[0]
		}
		if got := waitStatus(t, ctx, other.cli, s.id, s.run.GetRunID(), client.WorkflowStatusCompleted, 30*time.Second); got != client.WorkflowStatusCompleted {
			t.Fatalf("%s did not complete: status=%d", s.id, got)
		}
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%squick-%03d", prefix, i)
		c, _ := quickRan.Load(id)
		if c == nil || c.(*atomic.Int32).Load() != 1 {
			t.Errorf("%s: its activity ran %v times, want exactly once", id, c)
		}
	}
}

// Both replicas run the scheduler, and a schedule's tick is fired once, not once
// per replica.
func TestShared_ScheduleFiresOncePerTick(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		scheduleFiresOncePerTick(t, open)
	})
}

func scheduleFiresOncePerTick(t *testing.T, open dbtest.Open) {
	a := sharedReplica(t, open, "a", registerQuick)
	b := sharedReplica(t, open, "b", registerQuick)
	time.Sleep(100 * time.Millisecond)

	root := tasks.Principal{}
	if err := a.emb.View(root).CreateSchedule(tasks.Schedule{
		ScheduleId: "every-second", Namespace: "default",
		Spec:   tasks.ScheduleSpec{CronString: []string{"@every 1s"}},
		Action: tasks.ScheduleAction{WorkflowType: tasks.TypeRef{Name: "QuickWorkflow"}, TaskQueue: "default", Input: []any{"scheduled"}},
	}); err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	time.Sleep(6 * time.Second)

	runs, err := b.emb.View(root).ListWorkflows("default")
	if err != nil {
		t.Fatal(err)
	}
	ticks := map[string]int{}
	for _, r := range runs {
		if s, _ := r.SearchAttrs["ScheduleId"].(string); s == "every-second" {
			start, _ := r.SearchAttrs["TemporalScheduledStartTime"].(string)
			ticks[start]++
		}
	}
	if len(ticks) < 3 {
		t.Fatalf("the schedule fired %d ticks in 6s, want several: %v", len(ticks), ticks)
	}
	for tick, n := range ticks {
		if n != 1 {
			t.Errorf("tick %s fired %d times across two replicas, want 1", tick, n)
		}
	}
}
