// Copyright © 2026 Hanzo AI. MIT License.

package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// TestSignalWithStartWorkflowOpcode verifies the client stamps the
// correct opcode (0x0066) and wire shape on the outbound frame.
func TestSignalWithStartWorkflowOpcode(t *testing.T) {
	t.Parallel()
	const wantRunID = "sws-run-1"
	st := &stubTransport{
		dynamicReply: func(op uint16, _ []byte) (map[string]any, uint32, string, error) {
			if op != opSignalWithStartWorkflow {
				return nil, 0, "", errors.New("wrong opcode")
			}
			return map[string]any{"run_id": wantRunID}, 0, "", nil
		},
	}
	c, err := Dial(Options{Namespace: "billing", Transport: st})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	run, err := c.SignalWithStartWorkflow(
		context.Background(),
		"subscription-42", "ExtendTrial",
		map[string]any{"days": 7},
		StartWorkflowOptions{
			TaskQueue:                "billing-q",
			WorkflowExecutionTimeout: 5 * time.Minute,
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 3,
			},
		},
		"SubscriptionWorkflow",
		map[string]any{"org": "acme"},
	)
	if err != nil {
		t.Fatalf("SignalWithStartWorkflow: %v", err)
	}
	if st.gotOp != opSignalWithStartWorkflow {
		t.Fatalf("opcode = 0x%04x, want 0x%04x", st.gotOp, opSignalWithStartWorkflow)
	}
	if run.GetID() != "subscription-42" {
		t.Fatalf("GetID = %q", run.GetID())
	}
	if run.GetRunID() != wantRunID {
		t.Fatalf("GetRunID = %q", run.GetRunID())
	}
}

// TestSignalWithStartWorkflowEncodesFields verifies the JSON wire
// shape carries both Start + Signal fields per schema/tasks.zap.
func TestSignalWithStartWorkflowEncodesFields(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{"run_id": "r"}}
	c, _ := Dial(Options{Namespace: "billing", Transport: st})
	defer c.Close()
	_, err := c.SignalWithStartWorkflow(
		context.Background(),
		"wf-1", "Go",
		"signal-arg",
		StartWorkflowOptions{TaskQueue: "q"},
		"WF",
		"arg0",
	)
	if err != nil {
		t.Fatalf("SignalWithStartWorkflow: %v", err)
	}
	var req map[string]any
	if err := decodeStubRequestBody(st.gotBody, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req["workflow_id"] != "wf-1" {
		t.Fatalf("workflow_id = %v", req["workflow_id"])
	}
	if req["signal_name"] != "Go" {
		t.Fatalf("signal_name = %v", req["signal_name"])
	}
	if req["signal_input"] != "signal-arg" {
		t.Fatalf("signal_input = %v", req["signal_input"])
	}
	if req["task_queue"] != "q" {
		t.Fatalf("task_queue = %v", req["task_queue"])
	}
	if req["workflow_type"] != "WF" {
		t.Fatalf("workflow_type = %v", req["workflow_type"])
	}
	input, _ := req["input"].([]any)
	if len(input) != 1 || input[0] != "arg0" {
		t.Fatalf("input = %v", input)
	}
}

// TestSignalWithStartRequiresWorkflowID rejects empty workflowID.
func TestSignalWithStartRequiresWorkflowID(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{"run_id": "r"}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	_, err := c.SignalWithStartWorkflow(context.Background(), "", "sig", nil,
		StartWorkflowOptions{TaskQueue: "q"}, "WF")
	if err == nil {
		t.Fatal("expected error on empty workflowID")
	}
}

// TestSignalWithStartRequiresSignalName rejects empty signalName.
func TestSignalWithStartRequiresSignalName(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{"run_id": "r"}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	_, err := c.SignalWithStartWorkflow(context.Background(), "wf", "", nil,
		StartWorkflowOptions{TaskQueue: "q"}, "WF")
	if err == nil {
		t.Fatal("expected error on empty signalName")
	}
}
