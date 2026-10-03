// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
)

// ── helpers ─────────────────────────────────────────────────────────────

func shardOf(t *testing.T, s *store, ns string) *storepkg.Shard {
	t.Helper()
	sh, err := s.mgr.Get(context.Background(), s.principal, ns)
	if err != nil {
		t.Fatalf("shard %s: %v", ns, err)
	}
	return sh
}

// keysIn returns every key in ns's shard with its value.
func keysIn(t *testing.T, s *store, ns string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := shardOf(t, s, ns).List(context.Background(), "", func(k string, v []byte) error {
		out[k] = string(v)
		return nil
	}); err != nil {
		t.Fatalf("list: %v", err)
	}
	return out
}

// startRun starts workflow wfID with an activity in flight and returns its
// runId.
func startRun(t *testing.T, en *engine, ns, wfID, requestID string) string {
	t.Helper()
	wf, err := en.StartWorkflowWithRequestID(ns, wfID, "", TypeRef{Name: "W"}, "tq", []any{"in"}, requestID)
	if err != nil {
		t.Fatalf("start %s: %v", wfID, err)
	}
	run := wf.Execution.RunId
	unlock := en.lockRun(ns, wfID, run)
	defer unlock()
	if err := en.applyScheduleActivity(ns, wfID, run, 0, "Act", []byte(`["in"]`), "tq", 0, 0, nil); err != nil {
		t.Fatalf("schedule %s: %v", wfID, err)
	}
	return run
}

// finish completes run's activity and then the run itself.
func finish(t *testing.T, en *engine, ns, wfID, run string) {
	t.Helper()
	if err := en.completeWorkflowActivity(ns, wfID, run, 0, []byte(`"done"`), nil); err != nil {
		t.Fatalf("complete activity %s: %v", wfID, err)
	}
	unlock := en.lockRun(ns, wfID, run)
	defer unlock()
	if _, err := en.terminalTransition(ns, wfID, run, "WORKFLOW_EXECUTION_STATUS_COMPLETED", "workflow.completed", "WORKFLOW_EXECUTION_COMPLETED", map[string]any{"result": "ok"}); err != nil {
		t.Fatalf("close %s: %v", wfID, err)
	}
}

// restarted is a fresh engine over s — an empty dispatcher, as after a
// restart — with workers subscribed so recovery's deliveries are captured.
func restarted(t *testing.T, s *store) (*engine, *captureSend) {
	t.Helper()
	en := newEngine(s)
	cap := &captureSend{}
	en.disp.send = cap.fn
	if _, err := en.disp.Subscribe("wfPeer", "default", "tq", kindWorkflow); err != nil {
		t.Fatalf("subscribe wf: %v", err)
	}
	if _, err := en.disp.Subscribe("actPeer", "default", "tq", kindActivity); err != nil {
		t.Fatalf("subscribe act: %v", err)
	}
	return en, cap
}

// delivered lists the runs recovery handed a worker: "wf:<id>/<run>" per
// workflow task, "act:<activityId>" per activity task, sorted.
func delivered(t *testing.T, cap *captureSend) []string {
	t.Helper()
	var out []string
	for _, c := range cap.snapshot() {
		switch c.opcode {
		case OpcodeDeliverWorkflowTask:
			var d workflowTaskDeliveryJSON
			if err := json.Unmarshal(c.body, &d); err != nil {
				t.Fatalf("decode workflow task: %v", err)
			}
			out = append(out, "wf:"+d.WorkflowID+"/"+d.RunID)
		case OpcodeDeliverActivityTask:
			var d activityTaskDeliveryJSON
			if err := json.Unmarshal(c.body, &d); err != nil {
				t.Fatalf("decode activity task: %v", err)
			}
			out = append(out, "act:"+d.ActivityID)
		}
	}
	sort.Strings(out)
	return out
}

func openIndex(t *testing.T, s *store, ns string) []string {
	t.Helper()
	var out []string
	for k := range keysIn(t, s, ns) {
		if strings.HasPrefix(k, openPrefix(ns)) {
			out = append(out, strings.TrimPrefix(k, openPrefix(ns)))
		}
	}
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

// ── recovery ────────────────────────────────────────────────────────────

// TestRecover_FreshShard: a shard written with the index from the start.
// Starting a run indexes it and closing it drops it, so the index is exactly
// the open runs; a restart re-drives those; and once the index is built,
// Recover never reads a finished run's row — one that no longer even decodes
// cannot stop it, where reading every run would have failed on it.
func TestRecover_FreshShard(t *testing.T) {
	s := newStore()
	defer s.close()
	enA := newEngine(s)
	regDefaultNS(t, enA)
	done := startRun(t, enA, "default", "wf-done", "")
	finish(t, enA, "default", "wf-done", done)
	live := startRun(t, enA, "default", "wf-live", "")

	if got, want := openIndex(t, s, "default"), []string{"wf-live/" + live}; !equal(got, want) {
		t.Fatalf("open index = %v, want %v", got, want)
	}
	want := []string{"act:" + wfActivityID(live, 0), "wf:wf-live/" + live}

	enB, capB := restarted(t, s)
	if err := enB.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := delivered(t, capB); !equal(got, want) {
		t.Fatalf("recovered %v, want %v", got, want)
	}

	if err := shardOf(t, s, "default").Put(context.Background(), runKey("default", "wf-done", done), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	enC, capC := restarted(t, s)
	if err := enC.Recover(); err != nil {
		t.Fatalf("recover read a finished run: %v", err)
	}
	if got := delivered(t, capC); !equal(got, want) {
		t.Fatalf("recovered %v, want %v", got, want)
	}
}

// TestRecover_BackfillsShardWithoutIndex: a shard whose rows predate the
// index. The first Recover walks the rows once, builds the index and
// re-drives the open run; the walk is recorded and never repeated.
func TestRecover_BackfillsShardWithoutIndex(t *testing.T) {
	s := newStore()
	defer s.close()
	enA := newEngine(s)
	regDefaultNS(t, enA)
	done := startRun(t, enA, "default", "wf-done", "")
	finish(t, enA, "default", "wf-done", done)
	live := startRun(t, enA, "default", "wf-live", "")
	sh := shardOf(t, s, "default")
	for _, k := range openIndex(t, s, "default") {
		if err := sh.Del(context.Background(), openPrefix("default")+k); err != nil {
			t.Fatal(err)
		}
	}

	enB, cap := restarted(t, s)
	if err := enB.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	want := []string{"act:" + wfActivityID(live, 0), "wf:wf-live/" + live}
	if got := delivered(t, cap); !equal(got, want) {
		t.Fatalf("recovered %v, want %v", got, want)
	}
	if got := openIndex(t, s, "default"); !equal(got, []string{"wf-live/" + live}) {
		t.Fatalf("backfilled index = %v", got)
	}
	if _, ok := keysIn(t, s, "default")[openIndexKey("default")]; !ok {
		t.Fatal("backfill left no marker")
	}

	// A RUNNING row written the way rows were before the index: the marker
	// says the walk is done, so it is not walked again.
	stray := WorkflowExecution{Execution: ExecutionRef{WorkflowId: "wf-stray", RunId: "r1"}, Type: TypeRef{Name: "W"}, Status: "WORKFLOW_EXECUTION_STATUS_RUNNING", TaskQueue: "tq", StartTime: nowRFC3339()}
	if err := s.put(runKey("default", "wf-stray", "r1"), stray); err != nil {
		t.Fatal(err)
	}
	enC, capC := restarted(t, s)
	if err := enC.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := delivered(t, capC); !equal(got, want) {
		t.Fatalf("second recover %v, want %v (the walk ran again)", got, want)
	}
}

// TestRecover_DropsEntriesItsRowsContradict: a process stopped between the
// writes that open or close a run leaves an entry with no row, or one whose
// row is terminal. Recover drives neither and deletes both.
func TestRecover_DropsEntriesItsRowsContradict(t *testing.T) {
	s := newStore()
	defer s.close()
	enA := newEngine(s)
	regDefaultNS(t, enA)
	done := startRun(t, enA, "default", "wf-done", "")
	finish(t, enA, "default", "wf-done", done)
	for _, ref := range []ExecutionRef{{WorkflowId: "wf-done", RunId: done}, {WorkflowId: "wf-gone", RunId: "r1"}} {
		if err := enA.openRun("default", ref); err != nil {
			t.Fatal(err)
		}
	}

	enB, cap := restarted(t, s)
	if err := enB.Recover(); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := delivered(t, cap); len(got) != 0 {
		t.Fatalf("recovered %v, want nothing", got)
	}
	if got := openIndex(t, s, "default"); len(got) != 0 {
		t.Fatalf("open index = %v, want empty", got)
	}
}
