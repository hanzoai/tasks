// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"sync"
	"testing"
)

// White-box tests for the Phase-2a durable activity core: exactly-once
// dispatch keyed by (ns, run, seq) and the crash-recovery invariants.

func regDefaultNS(t *testing.T, en *engine) {
	t.Helper()
	if err := en.RegisterNamespace(Namespace{
		NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"},
		Config:        NamespaceCfg{WorkflowExecutionRetentionTtl: "720h", APSLimit: 400},
	}); err != nil {
		t.Fatalf("register ns: %v", err)
	}
}

func countEventType(t *testing.T, en *engine, wf, run, typ string) int {
	t.Helper()
	evs, _, err := en.GetWorkflowHistory("default", wf, run, 0, 10_000, false)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	n := 0
	for _, e := range evs {
		if e.EventType == typ {
			n++
		}
	}
	return n
}

func countDeliveries(cap *captureSend, opcode uint16) int {
	n := 0
	for _, c := range cap.snapshot() {
		if c.opcode == opcode {
			n++
		}
	}
	return n
}

// TestDurable_ExactlyOnceActivityDispatch: scheduling the same (run, seq)
// twice yields exactly one activity record, one ACTIVITY_TASK_SCHEDULED
// event, and one dispatch on the wire — the user activity never runs twice.
func TestDurable_ExactlyOnceActivityDispatch(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	regDefaultNS(t, en)

	cap := &captureSend{}
	en.disp.send = cap.fn
	if _, err := en.disp.Subscribe("actPeer", "default", "tq", kindActivity); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	wf, err := en.StartWorkflow("default", "wf-1", "", TypeRef{Name: "W"}, "tq", []any{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run := wf.Execution.RunId

	sched := func() {
		unlock := en.lockRun("default", "wf-1", run)
		defer unlock()
		if err := en.applyScheduleActivity("default", "wf-1", run, 0, "Act", []byte(`[]`), "tq", 0, 0, nil); err != nil {
			t.Errorf("applyScheduleActivity: %v", err)
		}
	}
	sched()
	sched() // second call must be a no-op (record already exists)

	if recs, _ := en.listWorkflowActivities("default", "wf-1", run); len(recs) != 1 {
		t.Fatalf("activity records = %d, want 1", len(recs))
	}
	if got := countEventType(t, en, "wf-1", run, evtActivityScheduled); got != 1 {
		t.Fatalf("ACTIVITY_TASK_SCHEDULED events = %d, want 1", got)
	}
	if got := countDeliveries(cap, OpcodeDeliverActivityTask); got != 1 {
		t.Fatalf("activity dispatches = %d, want 1 (exactly-once)", got)
	}
}

// TestDurable_ExactlyOnceActivityDispatch_Concurrent races many schedulers
// of the same (run, seq); the per-run lock makes the check-and-create atomic
// so exactly one record/event/dispatch results. Run with -race.
func TestDurable_ExactlyOnceActivityDispatch_Concurrent(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	regDefaultNS(t, en)

	cap := &captureSend{}
	en.disp.send = cap.fn
	if _, err := en.disp.Subscribe("actPeer", "default", "tq", kindActivity); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	wf, err := en.StartWorkflow("default", "wf-c", "", TypeRef{Name: "W"}, "tq", []any{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run := wf.Execution.RunId

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := en.lockRun("default", "wf-c", run)
			defer unlock()
			_ = en.applyScheduleActivity("default", "wf-c", run, 0, "Act", []byte(`[]`), "tq", 0, 0, nil)
		}()
	}
	wg.Wait()

	if recs, _ := en.listWorkflowActivities("default", "wf-c", run); len(recs) != 1 {
		t.Fatalf("activity records = %d, want 1 under concurrency", len(recs))
	}
	if got := countEventType(t, en, "wf-c", run, evtActivityScheduled); got != 1 {
		t.Fatalf("ACTIVITY_TASK_SCHEDULED events = %d, want 1 under concurrency", got)
	}
	if got := countDeliveries(cap, OpcodeDeliverActivityTask); got != 1 {
		t.Fatalf("activity dispatches = %d, want 1 under concurrency", got)
	}
}

// TestDurable_Recover_SkipsTerminal_RedispatchesInflight: a fresh engine over
// the same store re-dispatches only the non-terminal activity (never the
// already-completed one — no double execution) and re-enqueues a workflow
// task so the stateless worker replays and advances.
func TestDurable_Recover_SkipsTerminal_RedispatchesInflight(t *testing.T) {
	s := newStore()
	defer s.close()

	// Engine A drives a run to: seq0 COMPLETED (terminal), seq1 in-flight.
	enA := newEngine(s)
	regDefaultNS(t, enA)
	wf, err := enA.StartWorkflow("default", "wf-1", "", TypeRef{Name: "W"}, "tq", []any{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run := wf.Execution.RunId
	schedA := func(seq int) {
		unlock := enA.lockRun("default", "wf-1", run)
		defer unlock()
		if err := enA.applyScheduleActivity("default", "wf-1", run, seq, "Act", []byte(`[]`), "tq", 0, 0, nil); err != nil {
			t.Fatalf("applyScheduleActivity seq=%d: %v", seq, err)
		}
	}
	schedA(0)
	schedA(1)
	if err := enA.completeWorkflowActivity("default", "wf-1", run, 0, []byte(`"done"`), nil); err != nil {
		t.Fatalf("complete seq0: %v", err)
	}

	// Crash: fresh engine (empty dispatcher) over the SAME store.
	enB := newEngine(s)
	cap := &captureSend{}
	enB.disp.send = cap.fn
	if _, err := enB.disp.Subscribe("wfPeer", "default", "tq", kindWorkflow); err != nil {
		t.Fatalf("subscribe wf: %v", err)
	}
	if _, err := enB.disp.Subscribe("actPeer", "default", "tq", kindActivity); err != nil {
		t.Fatalf("subscribe act: %v", err)
	}
	if err := enB.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	var actIDs []string
	wfDeliveries := 0
	for _, c := range cap.snapshot() {
		switch c.opcode {
		case OpcodeDeliverActivityTask:
			var d activityTaskDeliveryJSON
			if err := json.Unmarshal(c.body, &d); err != nil {
				t.Fatalf("decode activity delivery: %v", err)
			}
			actIDs = append(actIDs, d.ActivityID)
		case OpcodeDeliverWorkflowTask:
			wfDeliveries++
		}
	}
	if len(actIDs) != 1 {
		t.Fatalf("re-dispatched %d activities, want 1 (only non-terminal seq1); ids=%v", len(actIDs), actIDs)
	}
	if actIDs[0] != wfActivityID(run, 1) {
		t.Fatalf("re-dispatched %q, want %q — the already-COMPLETED seq0 must NOT re-dispatch", actIDs[0], wfActivityID(run, 1))
	}
	if wfDeliveries != 1 {
		t.Fatalf("workflow tasks re-enqueued = %d, want 1", wfDeliveries)
	}
}
