// Copyright © 2026 Hanzo AI. MIT License.

package frontend

import (
	"context"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/inproc"
)

// TestInproc_Dispatcher_RoundTrips_Health proves that a pkg/sdk client
// constructed via inproc.NewClient against the real *ZAPHandler reaches
// handleHealth and gets a structured response. The path exercised is:
//
//	client.Health → roundTrip → encodeEnvelope →
//	inproc.Transport.Call → ZAPHandler.Dispatch → handleHealth
//
// No ZAP node is started, no listener is bound, no port is reserved.
// This is the proof that pkg/sdk/inproc is wire-compatible with the
// network path: the same handler fires either way.
func TestInproc_Dispatcher_RoundTrips_Health(t *testing.T) {
	h, fh := newTestHandler()
	c, err := inproc.NewClient(h, client.Options{
		Namespace:   "default",
		Identity:    "inproc-test",
		CallTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("inproc.NewClient: %v", err)
	}
	defer c.Close()

	service, status, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if status != "SERVING" {
		t.Fatalf("status=%q want SERVING", status)
	}
	if service == "" {
		t.Fatalf("service must be non-empty")
	}
	_ = fh // health does not increment fakeHandler.calls
}

// TestInproc_Dispatcher_RoundTrips_StartWorkflow proves the full
// workflow-start opcode path. The fake handler records the call and
// returns a deterministic run_id; the inproc transport must surface
// that to the client unchanged.
func TestInproc_Dispatcher_RoundTrips_StartWorkflow(t *testing.T) {
	h, fh := newTestHandler()
	c, err := inproc.NewClient(h, client.Options{
		Namespace: "default",
		Identity:  "inproc-test",
	})
	if err != nil {
		t.Fatalf("inproc.NewClient: %v", err)
	}
	defer c.Close()

	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        "wf-inproc",
		TaskQueue: "tq",
	}, "Fake")
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	if got := run.GetRunID(); got != "run-wf-inproc" {
		t.Fatalf("run_id=%q want run-wf-inproc", got)
	}
	if got := fh.calls.Load(); got != 1 {
		t.Fatalf("fakeHandler observed %d calls, want 1", got)
	}
}

// TestInproc_Dispatcher_RoundTrips_Describe verifies a second opcode
// path (DescribeWorkflow), guarding against a single-opcode regression
// where only StartWorkflow happens to wire correctly.
func TestInproc_Dispatcher_RoundTrips_Describe(t *testing.T) {
	h, fh := newTestHandler()
	c, err := inproc.NewClient(h, client.Options{Namespace: "default"})
	if err != nil {
		t.Fatalf("inproc.NewClient: %v", err)
	}
	defer c.Close()

	info, err := c.DescribeWorkflow(context.Background(), "wf-desc", "")
	if err != nil {
		t.Fatalf("DescribeWorkflow: %v", err)
	}
	if info == nil {
		t.Fatal("nil WorkflowExecutionInfo")
	}
	if got := fh.calls.Load(); got != 1 {
		t.Fatalf("fakeHandler observed %d calls, want 1", got)
	}
}

// TestInproc_UnknownOpcode_SurfacesAsError ensures Dispatch returns
// the canonical error for an opcode without a registered handler. The
// transport must surface that as a Go error rather than a malformed
// frame; the client's roundTrip wraps it with an opcode label.
func TestInproc_UnknownOpcode_SurfacesAsError(t *testing.T) {
	h, _ := newTestHandler()
	tr := inproc.NewTransport(h)
	defer tr.Close()

	_, err := tr.Call(context.Background(), 0xFFFF, []byte("{}"))
	if err == nil {
		t.Fatal("expected error for unknown opcode")
	}
}
