// Copyright © 2026 Hanzo AI. MIT License.

package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// TestQueryWorkflowOpcode confirms 0x0067 is used on the outbound
// frame and the JSON body carries namespace + execution + query_type.
func TestQueryWorkflowOpcode(t *testing.T) {
	t.Parallel()
	st := &stubTransport{
		dynamicReply: func(op uint16, _ []byte) (map[string]any, uint32, string, error) {
			if op != opQueryWorkflow {
				return nil, 0, "", errors.New("wrong opcode")
			}
			result, _ := json.Marshal(map[string]any{"pending": 3})
			return map[string]any{"result": result}, 0, "", nil
		},
	}
	c, _ := Dial(Options{Namespace: "billing", Transport: st})
	defer c.Close()

	ev, err := c.QueryWorkflow(context.Background(), "wf-1", "run-1", "getBalance", 1)
	if err != nil {
		t.Fatalf("QueryWorkflow: %v", err)
	}
	if !ev.HasValue() {
		t.Fatal("expected HasValue=true")
	}
	var got struct {
		Pending int `json:"pending"`
	}
	if err := ev.Get(&got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Pending != 3 {
		t.Fatalf("decoded = %+v", got)
	}
	if st.gotOp != opQueryWorkflow {
		t.Fatalf("opcode = 0x%04x, want 0x%04x", st.gotOp, opQueryWorkflow)
	}

	var req map[string]any
	if err := decodeStubRequestBody(st.gotBody, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req["workflow_id"] != "wf-1" {
		t.Fatalf("workflow_id = %v", req["workflow_id"])
	}
	if req["run_id"] != "run-1" {
		t.Fatalf("run_id = %v", req["run_id"])
	}
	if req["query_type"] != "getBalance" {
		t.Fatalf("query_type = %v", req["query_type"])
	}
}

// TestQueryWorkflowEmptyResult returns an EncodedValue that reports
// HasValue=false when the server answers with no result payload.
func TestQueryWorkflowEmptyResult(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	ev, err := c.QueryWorkflow(context.Background(), "wf", "", "q")
	if err != nil {
		t.Fatalf("QueryWorkflow: %v", err)
	}
	if ev.HasValue() {
		t.Fatal("empty payload must report HasValue=false")
	}
}

// TestQueryWorkflowRequiresWorkflowID rejects empty workflowID.
func TestQueryWorkflowRequiresWorkflowID(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	if _, err := c.QueryWorkflow(context.Background(), "", "", "q"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := c.QueryWorkflow(context.Background(), "wf", "", ""); err == nil {
		t.Fatal("expected error on empty queryType")
	}
}
