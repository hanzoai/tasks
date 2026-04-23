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
// This directly exercises Red §5.1 (CRITICAL-1): prior to PR-B
// ExecuteActivity settled (nil, nil) without touching the wire.
// Here we assert that (a) ScheduleActivity was called with the
// correct activity type, (b) WaitActivityResult was long-polled
// for the same activityTaskId, (c) the activity fn registered in
// the worker's registry was NOT invoked locally, and (d) the
// Future returned to the workflow settled with the wire-returned
// result bytes.
type recordingTransport struct {
	fakeTransport

	mu       sync.Mutex
	schedReq []client.ScheduleActivityRequest
	waitReq  []client.WaitActivityResultRequest

	// wireResult is what WaitActivityResult returns (Ready=true).
	wireResult []byte
	// waitCalls counts how many Wait calls we've fielded; the
	// first returns ready=false to exercise the long-poll loop,
	// the second returns ready=true with wireResult.
	waitCalls atomic.Int32
}

func (r *recordingTransport) ScheduleActivity(ctx context.Context, req client.ScheduleActivityRequest) (*client.ScheduleActivityResponse, error) {
	r.mu.Lock()
	r.schedReq = append(r.schedReq, req)
	r.mu.Unlock()
	return &client.ScheduleActivityResponse{ActivityTaskID: "wire-act-1"}, nil
}

func (r *recordingTransport) WaitActivityResult(ctx context.Context, req client.WaitActivityResultRequest) (*client.WaitActivityResultResponse, error) {
	r.mu.Lock()
	r.waitReq = append(r.waitReq, req)
	r.mu.Unlock()
	n := r.waitCalls.Add(1)
	if n == 1 {
		// Exercise the long-poll "not ready yet" path.
		return &client.WaitActivityResultResponse{Ready: false}, nil
	}
	return &client.WaitActivityResultResponse{
		Ready:  true,
		Result: r.wireResult,
	}, nil
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

	// (b) WaitActivityResult was long-polled for the same id.
	if len(rt.waitReq) < 1 {
		t.Fatalf("WaitActivityResult called %d times, want >=1", len(rt.waitReq))
	}
	for i, wr := range rt.waitReq {
		if wr.ActivityTaskID != "wire-act-1" {
			t.Errorf("WaitActivityResult[%d].ActivityTaskID = %q, want %q",
				i, wr.ActivityTaskID, "wire-act-1")
		}
	}

	// (c) The in-process activity fn MUST NOT have been called.
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
