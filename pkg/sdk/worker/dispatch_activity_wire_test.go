// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// recordingTransport embeds fakeTransport and overrides the
// activity-wire methods so the test can assert they were hit
// and capture the requests for inspection.
//
// Red §5.1 (CRITICAL-1) coverage: prior to the server-push migration
// ExecuteActivity settled (nil, nil) without touching the wire. Here
// we assert that (a) ScheduleActivity was called with the correct
// activity type, (b) the worker is awaiting the activity result via
// the pending-result registry installed by Subscribe, (c) the activity
// fn registered locally was NOT invoked (the wire is source of truth),
// and (d) the workflow Future settles with the wire-returned bytes
// after the server push lands.
type recordingTransport struct {
	fakeTransport

	mu       sync.Mutex
	schedReq []client.ScheduleActivityRequest

	// wireResult is what the server pushes via OnActivityResult.
	wireResult []byte
}

func (r *recordingTransport) ScheduleActivity(ctx context.Context, req client.ScheduleActivityRequest) (*client.ScheduleActivityResponse, error) {
	r.mu.Lock()
	r.schedReq = append(r.schedReq, req)
	r.mu.Unlock()
	// Schedule the server push asynchronously so the workflow
	// goroutine has time to register the pending channel.
	go func() {
		time.Sleep(20 * time.Millisecond)
		r.PushActivityResult("wire-act-1", r.wireResult, nil)
	}()
	return &client.ScheduleActivityResponse{ActivityTaskID: "wire-act-1"}, nil
}

// activityLocalCallCount tracks whether the activity fn was
// invoked in-process. For a wire-backed dispatch this must stay
// at zero — the activity lives on a different worker.
var activityLocalCallCount atomic.Int32

func wireTargetActivity(ctx context.Context, n int) (int, error) {
	activityLocalCallCount.Add(1)
	return n * 2, nil
}

// wireCallingWorkflow calls workflow.ExecuteActivity exactly once
// and returns whatever the Future settles with.
func wireCallingWorkflow(ctx workflow.Context, n int) (int, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		TaskQueue:           "test-queue",
	}
	ctx = workflow.WithActivityOptions(ctx, opts)

	fut := workflow.ExecuteActivity(ctx, wireTargetActivity, n)
	var out int
	if err := fut.Get(ctx, &out); err != nil {
		return 0, err
	}
	return out, nil
}

func TestDispatch_WorkflowExecuteActivity_HitsWire(t *testing.T) {
	// Not parallel: mutates activityLocalCallCount.
	activityLocalCallCount.Store(0)

	rt := &recordingTransport{
		wireResult: mustJSON(t, 84), // the wire returns 84 (n=42, wire doubles it)
	}
	w := newTestWorker(t, &rt.fakeTransport)
	w.transport = rt // rebind to the recording transport
	// Wire the OnActivityResult callback to the worker's
	// completeActivity router so PushActivityResult drains into the
	// workflow's pending channel.
	rt.OnActivityResult(func(activityID string, result, failure []byte) {
		w.completeActivity(activityID, result, failure)
	})
	w.RegisterWorkflow(wireCallingWorkflow)
	w.RegisterActivity(wireTargetActivity)

	input := mustJSON(t, []any{42})
	w.dispatchWorkflowTask(context.Background(), &client.WorkflowTask{
		TaskToken:        []byte{0x01},
		WorkflowID:       "wf-wire",
		RunID:            "run-wire",
		WorkflowTypeName: "wireCallingWorkflow",
		History:          input,
	})

	// (a) ScheduleActivity was called with the correct activity type.
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.schedReq) != 1 {
		t.Fatalf("ScheduleActivity called %d times, want 1", len(rt.schedReq))
	}
	if rt.schedReq[0].ActivityType != "wireTargetActivity" {
		t.Errorf("ScheduleActivity.ActivityType = %q, want %q",
			rt.schedReq[0].ActivityType, "wireTargetActivity")
	}
	if rt.schedReq[0].WorkflowID != "wf-wire" {
		t.Errorf("ScheduleActivity.WorkflowID = %q, want %q",
			rt.schedReq[0].WorkflowID, "wf-wire")
	}

	// (b) The in-process activity fn MUST NOT have been called.
	// The wire is the source of truth.
	if got := activityLocalCallCount.Load(); got != 0 {
		t.Fatalf("activity fn invoked locally %d times; wire dispatch "+
			"must route through transport, not call the fn in-proc", got)
	}

	// (d) The workflow completed successfully with the wire result.
	// The dispatch.go path emits a completeWorkflow command on success
	// and a failWorkflow command on error; assert we got success.
	if rt.workflowCompleted.Load() != 1 {
		t.Fatalf("RespondWorkflowTaskCompleted called %d times, want 1",
			rt.workflowCompleted.Load())
	}
	if rt.lastWorkflowResp == nil {
		t.Fatal("no commands response captured")
	}
	env := decodeCommandsEnvelope(t, rt.lastWorkflowResp.Commands)
	if env.Version != 1 {
		t.Errorf("envelope version = %d, want 1", env.Version)
	}
	if len(env.Commands) != 1 {
		t.Fatalf("envelope commands = %d, want 1", len(env.Commands))
	}
	cmd := env.Commands[0]
	if cmd.Kind != commandKindCompleteWorkflow {
		t.Fatalf("command kind = %d, want %d (completeWorkflow); "+
			"failure=%q", cmd.Kind, commandKindCompleteWorkflow, cmd.Failure)
	}
	// result bytes should be JSON-encoded 84 (wire doubled).
	var got int
	if err := json.Unmarshal(cmd.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got != 84 {
		t.Errorf("workflow result = %d, want 84 (wire-returned)", got)
	}
}

// mustJSON is a test helper that json.Marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// decodeCommandsEnvelope parses the producer-side wire shape for
// assertion. Shared with the error-path test.
func decodeCommandsEnvelope(t *testing.T, b []byte) commandsEnvelope {
	t.Helper()
	var e commandsEnvelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("decode envelope: %v (raw=%q)", err, b)
	}
	return e
}
