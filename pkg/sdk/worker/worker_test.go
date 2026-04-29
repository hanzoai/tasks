// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/activity"
	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	luxlog "github.com/luxfi/log"
)

// -------- fake transport ---------------------------------------------------
//
// fakeTransport is an in-memory client.WorkerTransport for the
// server-push wire model. Tests inject tasks via PushWorkflowTask /
// PushActivityTask, which call the registered OnWorkflowTask /
// OnActivityTask handlers exactly as the real transport would on a
// server-pushed delivery. Respond* / Heartbeat invocations are
// captured atomically.
type fakeTransport struct {
	mu sync.Mutex

	onWFTask  func(*client.WorkflowTask)
	onActTask func(*client.ActivityTask)
	onActRes  func(activityID string, result, failure []byte)

	subWF, subAct atomic.Int32
	unsubCount    atomic.Int32

	workflowCompleted atomic.Int32
	activityCompleted atomic.Int32
	activityFailed    atomic.Int32
	heartbeatCount    atomic.Int32

	lastWorkflowResp *client.RespondWorkflowTaskCompletedRequest
	lastActivityResp *client.RespondActivityTaskCompletedRequest
	lastActivityFail *client.RespondActivityTaskFailedRequest
}

func (f *fakeTransport) Close() error { return nil }

func (f *fakeTransport) SubscribeWorkflowTasks(ctx context.Context, req client.PollWorkflowTaskRequest) (string, error) {
	f.subWF.Add(1)
	return "wf-sub", nil
}

func (f *fakeTransport) SubscribeActivityTasks(ctx context.Context, req client.PollActivityTaskRequest) (string, error) {
	f.subAct.Add(1)
	return "act-sub", nil
}

func (f *fakeTransport) Unsubscribe(ctx context.Context, subID string) error {
	f.unsubCount.Add(1)
	return nil
}

func (f *fakeTransport) OnWorkflowTask(fn func(*client.WorkflowTask)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onWFTask = fn
}

func (f *fakeTransport) OnActivityTask(fn func(*client.ActivityTask)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onActTask = fn
}

func (f *fakeTransport) OnActivityResult(fn func(activityID string, result, failure []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onActRes = fn
}

// PushWorkflowTask simulates a server-pushed workflow task delivery.
func (f *fakeTransport) PushWorkflowTask(t *client.WorkflowTask) {
	f.mu.Lock()
	cb := f.onWFTask
	f.mu.Unlock()
	if cb != nil {
		cb(t)
	}
}

// PushActivityTask simulates a server-pushed activity task delivery.
func (f *fakeTransport) PushActivityTask(t *client.ActivityTask) {
	f.mu.Lock()
	cb := f.onActTask
	f.mu.Unlock()
	if cb != nil {
		cb(t)
	}
}

// PushActivityResult simulates a server-pushed activity-result delivery.
func (f *fakeTransport) PushActivityResult(activityID string, result, failure []byte) {
	f.mu.Lock()
	cb := f.onActRes
	f.mu.Unlock()
	if cb != nil {
		cb(activityID, result, failure)
	}
}

func (f *fakeTransport) RespondWorkflowTaskCompleted(ctx context.Context, req client.RespondWorkflowTaskCompletedRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastWorkflowResp = &req
	f.workflowCompleted.Add(1)
	return nil
}

func (f *fakeTransport) RespondActivityTaskCompleted(ctx context.Context, req client.RespondActivityTaskCompletedRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastActivityResp = &req
	f.activityCompleted.Add(1)
	return nil
}

func (f *fakeTransport) RespondActivityTaskFailed(ctx context.Context, req client.RespondActivityTaskFailedRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastActivityFail = &req
	f.activityFailed.Add(1)
	return nil
}

func (f *fakeTransport) RecordActivityTaskHeartbeat(ctx context.Context, req client.RecordActivityTaskHeartbeatRequest) (bool, error) {
	f.heartbeatCount.Add(1)
	return false, nil
}

// ScheduleActivity satisfies WorkerTransport. The default
// implementation is a no-op; tests that exercise wire-backed
// activity dispatch embed fakeTransport and override.
func (f *fakeTransport) ScheduleActivity(ctx context.Context, req client.ScheduleActivityRequest) (*client.ScheduleActivityResponse, error) {
	return &client.ScheduleActivityResponse{ActivityTaskID: "stub-id"}, nil
}

// StartChildWorkflow satisfies WorkerTransport.
func (f *fakeTransport) StartChildWorkflow(ctx context.Context, req client.StartChildWorkflowRequest) (*client.StartChildWorkflowResponse, error) {
	return &client.StartChildWorkflowResponse{RunID: "stub-run-id"}, nil
}

// -------- test harness ----------------------------------------------------
//
// newTestWorker builds a workerImpl directly rather than going through
// New, since New requires a full client.Client. The worker's internals
// don't touch the Client beyond name/identity extraction, which we
// supply manually.
func newTestWorker(t *testing.T, ft *fakeTransport) *workerImpl {
	t.Helper()
	return &workerImpl{
		client:    nil,
		transport: ft,
		taskQueue: "test-queue",
		namespace: "default",
		identity:  "test-worker@1",
		opts: Options{
			MaxConcurrentActivityExecutionSize: 1,
			MaxConcurrentWorkflowTaskPollers:   1,
		},
		logger:   luxlog.Noop(),
		registry: newRegistry(),
		stopCh:   make(chan struct{}),
	}
}

// -------- registration tests ----------------------------------------------

func TestRegisterWorkflow_ByReflectedName(t *testing.T) {
	t.Parallel()
	r := newRegistry()
	r.registerWorkflow(sampleWorkflow, RegisterWorkflowOptions{})
	if _, ok := r.workflowFn("sampleWorkflow"); !ok {
		t.Fatalf("workflow not registered under reflected name; registry=%v", r.workflows)
	}
}

func TestRegisterWorkflow_ByExplicitName(t *testing.T) {
	t.Parallel()
	r := newRegistry()
	r.registerWorkflow(sampleWorkflow, RegisterWorkflowOptions{Name: "CustomName"})
	if _, ok := r.workflowFn("CustomName"); !ok {
		t.Fatalf("workflow not registered under explicit name")
	}
}

func TestRegisterWorkflow_PanicsOnDuplicate(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	r := newRegistry()
	r.registerWorkflow(sampleWorkflow, RegisterWorkflowOptions{Name: "X"})
	r.registerWorkflow(sampleWorkflow, RegisterWorkflowOptions{Name: "X"})
}

func TestRegisterWorkflow_DisableCheck(t *testing.T) {
	t.Parallel()
	r := newRegistry()
	r.registerWorkflow(sampleWorkflow, RegisterWorkflowOptions{Name: "X"})
	r.registerWorkflow(sampleWorkflow, RegisterWorkflowOptions{Name: "X", DisableAlreadyRegisteredCheck: true})
	// No panic: replaced successfully.
}

func TestRegisterActivity_ByReflectedName(t *testing.T) {
	t.Parallel()
	r := newRegistry()
	r.registerActivity(sampleActivity, RegisterActivityOptions{})
	if _, ok := r.activityFn("sampleActivity"); !ok {
		t.Fatalf("activity not registered; registry=%v", r.activities)
	}
}

// -------- dispatch tests --------------------------------------------------

var workflowRan atomic.Int32
var lastWorkflowArg atomic.Value // string

func sampleWorkflow(ctx workflow.Context, name string) (string, error) {
	workflowRan.Add(1)
	lastWorkflowArg.Store(name)
	return "ok-" + name, nil
}

var activityRan atomic.Int32
var lastActivityArg atomic.Value // string

func sampleActivity(ctx context.Context, greeting string) (string, error) {
	activityRan.Add(1)
	lastActivityArg.Store(greeting)
	_, _ = io.Discard.Write([]byte(activity.GetInfo(ctx).ActivityType))
	return "handled:" + greeting, nil
}

func TestWorker_DispatchWorkflowTask(t *testing.T) {
	// NOT parallel: asserts on global workflowRan counter.
	workflowRan.Store(0)
	lastWorkflowArg.Store("")

	ft := &fakeTransport{}
	w := newTestWorker(t, ft)
	w.RegisterWorkflow(sampleWorkflow)

	input, _ := json.Marshal([]any{"tester"})

	// One poll cycle: dispatch the task directly.
	w.dispatchWorkflowTask(context.Background(), &client.WorkflowTask{
		TaskToken:        []byte{0x01, 0x02, 0x03},
		WorkflowID:       "wf-1",
		RunID:            "run-1",
		WorkflowTypeName: "sampleWorkflow",
		History:          input,
	})

	if got := workflowRan.Load(); got != 1 {
		t.Fatalf("workflow ran %d times, want 1", got)
	}
	if arg := lastWorkflowArg.Load(); arg == nil || arg.(string) != "tester" {
		t.Fatalf("workflow arg = %v, want %q", arg, "tester")
	}
	if ft.workflowCompleted.Load() != 1 {
		t.Fatalf("RespondWorkflowTaskCompleted called %d times, want 1", ft.workflowCompleted.Load())
	}
	if ft.lastWorkflowResp == nil || len(ft.lastWorkflowResp.Commands) == 0 {
		t.Fatal("expected non-empty commands payload in respond request")
	}
}

func TestWorker_DispatchWorkflowTask_Unregistered(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{}
	w := newTestWorker(t, ft)

	w.dispatchWorkflowTask(context.Background(), &client.WorkflowTask{
		TaskToken:        []byte{0x10},
		WorkflowID:       "wf-x",
		WorkflowTypeName: "UnknownWorkflow",
	})

	if ft.workflowCompleted.Load() != 1 {
		t.Fatalf("expected empty commands response; got completed=%d", ft.workflowCompleted.Load())
	}
}

func TestWorker_DispatchActivityTask(t *testing.T) {
	// NOT parallel: asserts on global activityRan counter.
	activityRan.Store(0)
	lastActivityArg.Store("")

	ft := &fakeTransport{}
	w := newTestWorker(t, ft)
	w.RegisterActivity(sampleActivity)

	input, _ := json.Marshal([]any{"hello"})
	w.dispatchActivityTask(context.Background(), &client.ActivityTask{
		TaskToken:        []byte{0xaa},
		WorkflowID:       "wf-a",
		RunID:            "run-a",
		ActivityID:       "act-1",
		ActivityTypeName: "sampleActivity",
		Input:            input,
		ScheduledTimeMs:  time.Now().UnixMilli(),
	})

	if got := activityRan.Load(); got != 1 {
		if ft.activityFailed.Load() > 0 && ft.lastActivityFail != nil {
			t.Logf("last failure envelope: %s", string(ft.lastActivityFail.Failure))
		}
		t.Fatalf("activity ran %d times, want 1 (completed=%d failed=%d)",
			got, ft.activityCompleted.Load(), ft.activityFailed.Load())
	}
	if arg := lastActivityArg.Load(); arg == nil || arg.(string) != "hello" {
		t.Fatalf("activity arg = %v, want %q", arg, "hello")
	}
	if ft.activityCompleted.Load() != 1 {
		t.Fatalf("RespondActivityTaskCompleted called %d times, want 1", ft.activityCompleted.Load())
	}
	if ft.lastActivityResp == nil {
		t.Fatal("expected an activity completed response captured")
	}
	var got string
	if err := json.Unmarshal(ft.lastActivityResp.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got != "handled:hello" {
		t.Fatalf("activity result = %q, want %q", got, "handled:hello")
	}
}

func TestWorker_DispatchActivityTask_Unregistered(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{}
	w := newTestWorker(t, ft)

	w.dispatchActivityTask(context.Background(), &client.ActivityTask{
		TaskToken:        []byte{0xbb},
		ActivityID:       "act-x",
		ActivityTypeName: "UnknownActivity",
	})

	if ft.activityFailed.Load() != 1 {
		t.Fatalf("expected failure response for unregistered activity; got %d", ft.activityFailed.Load())
	}
}

// failingActivity deliberately returns a temporal.Error so we see the
// failure pipeline.
func failingActivity(ctx context.Context) (any, error) {
	return nil, errors.New("boom")
}

func TestWorker_DispatchActivityTask_UserError(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{}
	w := newTestWorker(t, ft)
	w.RegisterActivity(failingActivity)

	w.dispatchActivityTask(context.Background(), &client.ActivityTask{
		TaskToken:        []byte{0xcc},
		ActivityID:       "act-e",
		ActivityTypeName: "failingActivity",
		Input:            nil,
	})

	if ft.activityFailed.Load() != 1 {
		t.Fatalf("expected failure response; got failed=%d completed=%d",
			ft.activityFailed.Load(), ft.activityCompleted.Load())
	}
}

// -------- server-push smoke test ------------------------------------------
//
// Subscribe and push a workflow task, verify dispatch fires, Stop
// cleanly unsubscribes.
func TestWorker_WorkflowPush_OneCycle(t *testing.T) {
	t.Parallel()
	workflowRan.Store(0)

	ft := &fakeTransport{}
	w := newTestWorker(t, ft)
	w.RegisterWorkflow(sampleWorkflow)

	if err := w.startSubscriptions(); err != nil {
		t.Fatalf("startSubscriptions: %v", err)
	}
	if ft.subWF.Load() != 1 || ft.subAct.Load() != 1 {
		t.Fatalf("subscribe counts wf=%d act=%d, want 1/1", ft.subWF.Load(), ft.subAct.Load())
	}

	input, _ := json.Marshal([]any{"loop"})
	ft.PushWorkflowTask(&client.WorkflowTask{
		TaskToken:        []byte{0x01},
		WorkflowID:       "wf-loop",
		WorkflowTypeName: "sampleWorkflow",
		History:          input,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ft.workflowCompleted.Load() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Stop()

	if workflowRan.Load() != 1 {
		t.Fatalf("workflow ran %d times, want 1", workflowRan.Load())
	}
	if ft.workflowCompleted.Load() != 1 {
		t.Fatalf("RespondWorkflowTaskCompleted = %d, want 1", ft.workflowCompleted.Load())
	}
	if ft.unsubCount.Load() != 2 {
		t.Fatalf("Unsubscribe count = %d, want 2 (wf + act)", ft.unsubCount.Load())
	}
}

// -------- activity push smoke test ----------------------------------------

func TestWorker_ActivityPush_OneCycle(t *testing.T) {
	t.Parallel()
	activityRan.Store(0)

	ft := &fakeTransport{}
	w := newTestWorker(t, ft)
	w.RegisterActivity(sampleActivity)

	if err := w.startSubscriptions(); err != nil {
		t.Fatalf("startSubscriptions: %v", err)
	}

	input, _ := json.Marshal([]any{"loop"})
	ft.PushActivityTask(&client.ActivityTask{
		TaskToken:        []byte{0x02},
		ActivityID:       "act-loop",
		ActivityTypeName: "sampleActivity",
		Input:            input,
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ft.activityCompleted.Load() == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	w.Stop()

	if activityRan.Load() != 1 {
		t.Fatalf("activity ran %d times, want 1", activityRan.Load())
	}
	if ft.activityCompleted.Load() != 1 {
		t.Fatalf("RespondActivityTaskCompleted = %d, want 1", ft.activityCompleted.Load())
	}
}

// -------- Start / Stop / Run ---------------------------------------------

func TestWorker_Run_StopsOnInterrupt(t *testing.T) {
	t.Parallel()
	ft := &fakeTransport{}
	w := newTestWorker(t, ft)

	interrupt := make(chan any)
	done := make(chan error, 1)
	go func() {
		done <- w.Run(interrupt)
	}()

	close(interrupt)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned err=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after interrupt")
	}
}

func TestNew_DefaultsIdentity(t *testing.T) {
	t.Parallel()
	// New(nil client, ...) returns a worker with a transport=nil but
	// the options should be normalised.
	ws := New(nil, "tq", Options{}).(*workerImpl)
	if ws.opts.MaxConcurrentActivityExecutionSize != defaultActivityPollers {
		t.Errorf("activity pollers = %d, want default %d",
			ws.opts.MaxConcurrentActivityExecutionSize, defaultActivityPollers)
	}
	if ws.opts.MaxConcurrentWorkflowTaskPollers != defaultWorkflowPollers {
		t.Errorf("workflow pollers = %d, want default %d",
			ws.opts.MaxConcurrentWorkflowTaskPollers, defaultWorkflowPollers)
	}
	if ws.identity == "" {
		t.Error("identity should default to hostname@pid")
	}
	if ws.namespace != "default" {
		t.Errorf("namespace = %q, want %q", ws.namespace, "default")
	}
}

func TestNew_StartFailsWithoutTransport(t *testing.T) {
	t.Parallel()
	w := New(nil, "tq", Options{}).(*workerImpl)
	err := w.Start()
	if err == nil {
		t.Fatal("expected Start to fail without a transport")
	}
}
