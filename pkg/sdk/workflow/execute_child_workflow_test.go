// Copyright © 2026 Hanzo AI. MIT License.

package workflow

import (
	"errors"
	"testing"
)

// DemoChildWorkflow is a test-only workflow function value used as the
// child-workflow handle argument.
func DemoChildWorkflow(_ Context, _ string) (int, error) { return 0, nil }

// TestExecuteChildWorkflowStub exercises the stub env's queued-reply
// behaviour and confirms the ChildWorkflowFuture shape is consistent
// with a plain Future on result + a side-band execution future.
func TestExecuteChildWorkflowStub(t *testing.T) {
	t.Parallel()
	env := NewStubEnv()
	env.OnChildWorkflow(DemoChildWorkflow).Return(42, nil)
	ctx := NewContextFromEnv(env)
	cf := ExecuteChildWorkflow(ctx, DemoChildWorkflow, "hello")

	// Execution future settles first (stub settles both synchronously;
	// test the ordering contract by reading execution eagerly).
	var got WorkflowExecution
	if err := cf.GetChildWorkflowExecution().Get(ctx, &got); err != nil {
		t.Fatalf("GetChildWorkflowExecution.Get: %v", err)
	}
	if got.WorkflowID == "" {
		t.Fatal("expected non-empty child WorkflowID from stub")
	}

	// Result future decodes the queued return value.
	var result int
	if err := cf.Get(ctx, &result); err != nil {
		t.Fatalf("Get result: %v", err)
	}
	if result != 42 {
		t.Fatalf("result = %d, want 42", result)
	}
}

// TestExecuteChildWorkflowSurfacesError propagates child errors to the
// parent via the result future.
func TestExecuteChildWorkflowSurfacesError(t *testing.T) {
	t.Parallel()
	env := NewStubEnv()
	env.OnChildWorkflow(DemoChildWorkflow).Return(0, errors.New("boom"))
	ctx := NewContextFromEnv(env)
	cf := ExecuteChildWorkflow(ctx, DemoChildWorkflow)

	var out any
	if err := cf.Get(ctx, &out); err == nil {
		t.Fatal("expected error from child workflow")
	}
}

// TestExecuteChildWorkflowNilArgsAccepted confirms nil/empty args are
// not a validation error at the caller — matches upstream.
func TestExecuteChildWorkflowNilArgsAccepted(t *testing.T) {
	t.Parallel()
	env := NewStubEnv()
	env.OnChildWorkflow(DemoChildWorkflow).Return(nil, nil)
	ctx := NewContextFromEnv(env)
	cf := ExecuteChildWorkflow(ctx, DemoChildWorkflow)
	var out any
	if err := cf.Get(ctx, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

// TestExecuteChildWorkflowNilChild rejects a nil child handle with a
// typed error.
func TestExecuteChildWorkflowNilChild(t *testing.T) {
	t.Parallel()
	env := NewStubEnv()
	ctx := NewContextFromEnv(env)
	cf := ExecuteChildWorkflow(ctx, nil)
	var out any
	if err := cf.Get(ctx, &out); err == nil {
		t.Fatal("expected error from nil child handle")
	}
}

// TestChildWorkflowFutureShape exercises the ReadyCh/IsReady plumbing
// that the Selector depends on.
func TestChildWorkflowFutureShape(t *testing.T) {
	t.Parallel()
	env := NewStubEnv()
	env.OnChildWorkflow(DemoChildWorkflow).Return(1, nil)
	ctx := NewContextFromEnv(env)
	cf := ExecuteChildWorkflow(ctx, DemoChildWorkflow)

	select {
	case <-cf.ReadyCh():
		// settled
	default:
		t.Fatal("stub-env ChildWorkflowFuture must settle synchronously")
	}
	if !cf.IsReady() {
		t.Fatal("IsReady must be true after settle")
	}
}
