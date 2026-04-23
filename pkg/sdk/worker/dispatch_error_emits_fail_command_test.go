// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// Red §5.7: workflow return errors were previously silently
// dropped by dispatch.go, and the server responded with an empty
// commands list — so errored workflows appeared to complete
// successfully, no retries, no visibility.
//
// PR-B fixes that: a non-nil fn return error is encoded via
// temporal.Encode and emitted as a single FailWorkflow command
// (kind=1) in the CommandsEnvelope. This test asserts that shape.

func erroringWorkflow(ctx workflow.Context) error {
	return errors.New("boom")
}

func TestDispatch_WorkflowError_EmitsFailCommand(t *testing.T) {
	t.Parallel()

	ft := &fakeTransport{}
	w := newTestWorker(t, ft)
	w.RegisterWorkflow(erroringWorkflow)

	w.dispatchWorkflowTask(context.Background(), &client.WorkflowTask{
		TaskToken:        []byte{0x99},
		WorkflowID:       "wf-err",
		RunID:            "run-err",
		WorkflowTypeName: "erroringWorkflow",
		// No inputs.
	})

	if ft.workflowCompleted.Load() != 1 {
		t.Fatalf("RespondWorkflowTaskCompleted called %d times, want 1",
			ft.workflowCompleted.Load())
	}
	if ft.lastWorkflowResp == nil || len(ft.lastWorkflowResp.Commands) == 0 {
		t.Fatal("expected a non-empty commands envelope on failure")
	}

	env := decodeCommandsEnvelope(t, ft.lastWorkflowResp.Commands)
	if env.Version != 1 {
		t.Errorf("envelope version = %d, want 1", env.Version)
	}
	if len(env.Commands) != 1 {
		t.Fatalf("envelope commands = %d, want 1; got %+v",
			len(env.Commands), env.Commands)
	}
	cmd := env.Commands[0]
	if cmd.Kind != commandKindFailWorkflow {
		t.Fatalf("command kind = %d, want %d (failWorkflow)",
			cmd.Kind, commandKindFailWorkflow)
	}
	if len(cmd.Failure) == 0 {
		t.Fatal("expected failure bytes; got empty")
	}
	if len(cmd.Result) != 0 {
		t.Errorf("result bytes should be empty on failWorkflow; got %q",
			cmd.Result)
	}
	if cmd.ActivityTaskID != "" {
		t.Errorf("activityTaskId should be empty on failWorkflow; got %q",
			cmd.ActivityTaskID)
	}

	// The failure bytes must round-trip via temporal.Decode into an
	// error whose Error() text surfaces the workflow's return.
	decoded := temporal.Decode(cmd.Failure)
	if decoded == nil {
		t.Fatal("temporal.Decode returned nil for non-empty failure")
	}
	if got := decoded.Error(); got == "" {
		t.Error("decoded error has empty message")
	}
	// The message shouldn't swallow "boom" — temporal.Encode
	// preserves the wrapped cause text.
	if !contains(decoded.Error(), "boom") {
		t.Errorf("decoded error = %q, want contains %q", decoded.Error(), "boom")
	}
}

// contains is a tiny substring check; avoids importing strings
// for a single call.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
