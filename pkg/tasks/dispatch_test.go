// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"sync"
	"testing"
)

// captureSend records every (peerID, opcode, body) the dispatcher pushes.
type captureSend struct {
	mu    sync.Mutex
	calls []sendCall
}

type sendCall struct {
	peerID string
	opcode uint16
	body   []byte
}

func (c *captureSend) fn(peerID string, opcode uint16, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(body))
	copy(cp, body)
	c.calls = append(c.calls, sendCall{peerID: peerID, opcode: opcode, body: cp})
	return nil
}

func (c *captureSend) snapshot() []sendCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]sendCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// TestDispatcher_WorkflowAndActivityRoundTrip walks the full server-push
// path: subscribe → enqueue workflow task → complete workflow → schedule
// activity → complete activity → assert result was pushed back to the
// workflow's peer.
func TestDispatcher_WorkflowAndActivityRoundTrip(t *testing.T) {
	cap := &captureSend{}
	d := newDispatcher()
	d.send = cap.fn

	// peerA subscribes to workflow tasks; peerB to activity tasks.
	const (
		ns         = "default"
		queue      = "demo"
		workflowID = "wf-1"
		runID      = "run-1"
		wfType     = "DemoWorkflow"
		actType    = "DemoActivity"
	)
	if _, err := d.Subscribe("peerA", ns, queue, kindWorkflow); err != nil {
		t.Fatalf("subscribe peerA: %v", err)
	}
	if _, err := d.Subscribe("peerB", ns, queue, kindActivity); err != nil {
		t.Fatalf("subscribe peerB: %v", err)
	}

	// Enqueue a workflow task. The dispatcher should immediately push
	// OpcodeDeliverWorkflowTask to peerA.
	d.EnqueueWorkflowTask("", ns, queue, workflowID, runID, wfType, []byte(`{"hello":"world"}`))

	calls := cap.snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 push after EnqueueWorkflowTask, got %d", len(calls))
	}
	if calls[0].peerID != "peerA" {
		t.Fatalf("workflow task delivered to %q, want peerA", calls[0].peerID)
	}
	if calls[0].opcode != OpcodeDeliverWorkflowTask {
		t.Fatalf("opcode = 0x%04x, want OpcodeDeliverWorkflowTask (0x%04x)",
			calls[0].opcode, OpcodeDeliverWorkflowTask)
	}

	// Decode the delivery JSON, extract the task token.
	var wfDelivery workflowTaskDeliveryJSON
	if err := json.Unmarshal(calls[0].body, &wfDelivery); err != nil {
		t.Fatalf("decode workflow delivery: %v", err)
	}
	if wfDelivery.WorkflowID != workflowID || wfDelivery.RunID != runID {
		t.Fatalf("delivery wrong ids: %+v", wfDelivery)
	}
	if wfDelivery.WorkflowTypeName != wfType {
		t.Fatalf("delivery type = %q, want %q", wfDelivery.WorkflowTypeName, wfType)
	}
	if wfDelivery.TaskToken == "" {
		t.Fatalf("delivery task_token is empty")
	}

	// Dispatch an activity for (wf, run, seq=0). The dispatcher delivers it
	// to the subscribed activity peer (peerB) and returns a token bound to
	// (ns, wf, run, seq).
	const actID = "run-1-act-0"
	actToken := d.DispatchActivity("", ns, queue, workflowID, runID, 0, actID, actType,
		[]byte(`{"name":"activity-input"}`), 60_000, 5_000)
	if len(actToken) == 0 {
		t.Fatalf("DispatchActivity returned empty token")
	}

	// One additional push to peerB.
	calls = cap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 pushes after DispatchActivity, got %d", len(calls))
	}
	if calls[1].peerID != "peerB" {
		t.Fatalf("activity task delivered to %q, want peerB", calls[1].peerID)
	}
	if calls[1].opcode != OpcodeDeliverActivityTask {
		t.Fatalf("opcode = 0x%04x, want OpcodeDeliverActivityTask (0x%04x)",
			calls[1].opcode, OpcodeDeliverActivityTask)
	}
	var actDelivery activityTaskDeliveryJSON
	if err := json.Unmarshal(calls[1].body, &actDelivery); err != nil {
		t.Fatalf("decode activity delivery: %v", err)
	}
	if actDelivery.ActivityID != actID {
		t.Fatalf("activity id = %q, want %q", actDelivery.ActivityID, actID)
	}
	if actDelivery.ActivityTypeName != actType {
		t.Fatalf("activity type = %q, want %q", actDelivery.ActivityTypeName, actType)
	}

	// The worker on peerB responds; the token resolves back to the exact
	// (ns, wf, run, seq) the engine advances from. Phase-2a activities do
	// NOT push a result to a workflow peer — the run advances by replay.
	pt, ok := d.ResolveActivityToken(actToken)
	if !ok {
		t.Fatalf("ResolveActivityToken: token not found")
	}
	if pt.ns != ns || pt.workflowID != workflowID || pt.runID != runID || pt.seq != 0 {
		t.Fatalf("resolved token wrong: ns=%q wf=%q run=%q seq=%d", pt.ns, pt.workflowID, pt.runID, pt.seq)
	}
	// Single-use.
	if _, ok := d.ResolveActivityToken(actToken); ok {
		t.Fatalf("ResolveActivityToken should be single-use")
	}
	// No result push on the wire — still exactly 2 sends.
	if got := len(cap.snapshot()); got != 2 {
		t.Fatalf("unexpected extra push after resolve: %d (no result push in the replay model)", got)
	}

	// Workflow task completion drops the inflight record.
	if _, ok := d.CompleteWorkflowTask([]byte(wfDelivery.TaskToken)); !ok {
		t.Fatalf("CompleteWorkflowTask: token not found")
	}
	if _, ok := d.CompleteWorkflowTask([]byte(wfDelivery.TaskToken)); ok {
		t.Fatalf("CompleteWorkflowTask should be single-use")
	}
}

// TestDispatcher_QueuesUntilSubscribe asserts that a task enqueued
// before any subscriber arrives is delivered to the first subscriber.
func TestDispatcher_QueuesUntilSubscribe(t *testing.T) {
	cap := &captureSend{}
	d := newDispatcher()
	d.send = cap.fn

	d.EnqueueWorkflowTask("", "ns", "q", "wf", "run", "Type", nil)
	if got := len(cap.snapshot()); got != 0 {
		t.Fatalf("unexpected push before subscribe: %d", got)
	}
	if _, err := d.Subscribe("late-peer", "ns", "q", kindWorkflow); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	calls := cap.snapshot()
	if len(calls) != 1 || calls[0].peerID != "late-peer" {
		t.Fatalf("expected 1 push to late-peer, got %+v", calls)
	}
}
