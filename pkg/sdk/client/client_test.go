package client

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// stubTransport is a test double that captures the opcode and JSON
// request body the client emits, and replays a canned response as a
// full ZAP envelope frame. Mirrors the fake-server pattern used by
// pkg/tasks/client_test.go (httptest server) — swapped for an
// in-process Transport because the v1 wire is ZAP, not HTTP.
type stubTransport struct {
	respBody     map[string]any
	respStatus   uint32
	respDetail   string
	callErr      error
	gotOp        uint16
	gotBody      []byte
	callCount    int
	dynamicReply func(op uint16, body []byte) (map[string]any, uint32, string, error)
	closed       bool
}

func (s *stubTransport) Call(ctx context.Context, op uint16, body []byte) ([]byte, error) {
	s.callCount++
	s.gotOp = op
	s.gotBody = append([]byte(nil), body...)
	if s.callErr != nil {
		return nil, s.callErr
	}

	respBody := s.respBody
	status := s.respStatus
	detail := s.respDetail
	if s.dynamicReply != nil {
		rb, st, dt, err := s.dynamicReply(op, body)
		if err != nil {
			return nil, err
		}
		respBody = rb
		status = st
		detail = dt
	}

	payload, err := json.Marshal(respBody)
	if err != nil {
		return nil, err
	}
	return encodeTestResponseFrame(op, status, detail, payload), nil
}

func (s *stubTransport) Close() error { s.closed = true; return nil }

func TestDialRequiresHostPort(t *testing.T) {
	t.Parallel()
	if _, err := Dial(Options{}); err == nil {
		t.Fatal("expected Dial to reject empty Options")
	}
}

func TestDialAndCloseWithInjectedTransport(t *testing.T) {
	t.Parallel()
	st := &stubTransport{}
	c, err := Dial(Options{
		Namespace: "scoped",
		Identity:  "tester",
		Transport: st,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	c.Close()
	if !st.closed {
		t.Fatal("expected transport to be closed by Client.Close")
	}

	// Subsequent calls on a closed client must surface ErrClosed.
	if err := c.CancelWorkflow(context.Background(), "wf", ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestDialDefaultNamespace(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, err := Dial(Options{Transport: st})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	if err := c.CancelWorkflow(context.Background(), "wf-1", ""); err != nil {
		t.Fatalf("CancelWorkflow: %v", err)
	}
	var req map[string]any
	if err := decodeStubRequestBody(st.gotBody, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req["namespace"] != "default" {
		t.Fatalf("expected default namespace, got %v", req["namespace"])
	}
}

func TestExecuteWorkflowRoundTrip(t *testing.T) {
	t.Parallel()
	const wantNS = "commerce"
	const wantRunID = "run-42"

	st := &stubTransport{
		dynamicReply: func(op uint16, body []byte) (map[string]any, uint32, string, error) {
			if op != opStartWorkflow {
				return nil, 0, "", errors.New("unexpected opcode")
			}
			return map[string]any{"run_id": wantRunID}, 0, "", nil
		},
	}
	c, err := Dial(Options{
		Namespace: wantNS,
		Identity:  "unit-test",
		Transport: st,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	opts := StartWorkflowOptions{
		ID:                       "wf-1",
		TaskQueue:                "ats",
		WorkflowExecutionTimeout: 5 * time.Minute,
		WorkflowRunTimeout:       2 * time.Minute,
		WorkflowTaskTimeout:      30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    3,
		},
	}
	run, err := c.ExecuteWorkflow(context.Background(), opts, "settlement.run", map[string]any{"org_id": "o1"})
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	if got := run.GetID(); got != "wf-1" {
		t.Fatalf("GetID = %q, want wf-1", got)
	}
	if got := run.GetRunID(); got != wantRunID {
		t.Fatalf("GetRunID = %q, want %s", got, wantRunID)
	}
	if st.gotOp != opStartWorkflow {
		t.Fatalf("opcode = 0x%04x, want 0x%04x", st.gotOp, opStartWorkflow)
	}

	var req map[string]any
	if err := decodeStubRequestBody(st.gotBody, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req["namespace"] != wantNS {
		t.Fatalf("namespace = %v, want %s", req["namespace"], wantNS)
	}
	if req["workflow_id"] != "wf-1" {
		t.Fatalf("workflow_id = %v", req["workflow_id"])
	}
	if req["workflow_type"] != "settlement.run" {
		t.Fatalf("workflow_type = %v", req["workflow_type"])
	}
	if req["task_queue"] != "ats" {
		t.Fatalf("task_queue = %v", req["task_queue"])
	}
	if req["identity"] != "unit-test" {
		t.Fatalf("identity = %v", req["identity"])
	}

	rp, ok := req["retry_policy"].(map[string]any)
	if !ok {
		t.Fatalf("retry_policy missing: %v", req["retry_policy"])
	}
	if rp["maximum_attempts"].(float64) != 3 {
		t.Fatalf("maximum_attempts = %v", rp["maximum_attempts"])
	}

	to, ok := req["timeouts"].(map[string]any)
	if !ok {
		t.Fatalf("timeouts missing: %v", req["timeouts"])
	}
	if to["workflow_execution_ms"].(float64) != float64(opts.WorkflowExecutionTimeout.Milliseconds()) {
		t.Fatalf("timeouts.execution_ms = %v", to["workflow_execution_ms"])
	}

	input, _ := req["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", input)
	}
	arg0, _ := input[0].(map[string]any)
	if arg0["org_id"] != "o1" {
		t.Fatalf("input[0].org_id = %v", arg0["org_id"])
	}
}

func TestServerErrorSurfaces(t *testing.T) {
	t.Parallel()
	st := &stubTransport{callErr: errors.New("server status 500: boom")}
	c, err := Dial(Options{Transport: st})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()
	if err := c.CancelWorkflow(context.Background(), "wf", ""); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSignalWorkflowValidatesInput(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	if err := c.SignalWorkflow(context.Background(), "", "", "sig", nil); err == nil {
		t.Fatal("expected error on empty workflowID")
	}
	if err := c.SignalWorkflow(context.Background(), "wf", "", "", nil); err == nil {
		t.Fatal("expected error on empty signalName")
	}
}

func TestExecuteWorkflowRequiresTaskQueue(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{"run_id": "r"}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	if _, err := c.ExecuteWorkflow(context.Background(), StartWorkflowOptions{ID: "wf"}, "t"); err == nil {
		t.Fatal("expected error on missing task queue")
	}
}
