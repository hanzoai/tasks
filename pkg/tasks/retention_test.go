// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// ── retention ───────────────────────────────────────────────────────────

// purgeFixture fills "default" (retention 720h) with every kind of run the
// purge distinguishes, and returns the namespace row and the keys each run
// owns, read from the shard.
type purgeFixture struct {
	en      *engine
	s       *store
	ns      Namespace
	expired map[string]bool // keys of the terminal runs
}

func newPurgeFixture(t *testing.T) *purgeFixture {
	t.Helper()
	s := newStore()
	t.Cleanup(func() { _ = s.close() })
	en := newEngine(s)
	regDefaultNS(t, en)
	const ns = "default"

	// A finished workflow run started under a request id.
	w1 := startRun(t, en, ns, "wf-1", "req-1")
	finish(t, en, ns, "wf-1", w1)
	// A finished run a manual schedule trigger started under a request id.
	if err := en.CreateSchedule(Schedule{ScheduleId: "sch", Namespace: ns, Action: ScheduleAction{WorkflowType: TypeRef{Name: "W"}, TaskQueue: "tq"}}); err != nil {
		t.Fatal(err)
	}
	trig, err := en.TriggerSchedule(ns, "sch", "trig-1")
	if err != nil {
		t.Fatal(err)
	}
	w2 := trig.Execution
	unlock := en.lockRun(ns, w2.WorkflowId, w2.RunId)
	if _, err := en.terminalTransition(ns, w2.WorkflowId, w2.RunId, "WORKFLOW_EXECUTION_STATUS_FAILED", "workflow.failed", "WORKFLOW_EXECUTION_FAILED", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	unlock()
	// A run still open under the same workflow id as a finished one.
	_ = startRun(t, en, ns, "wf-1", "req-2")
	// A finished standalone activity started under a request id, and one
	// still scheduled.
	if _, err := en.StartActivity(ns, "act-1", "ar1", TypeRef{Name: "A"}, "tq", nil, nil, "", "", "", "", "me", "areq-1"); err != nil {
		t.Fatal(err)
	}
	if err := en.CompleteActivity(ns, "act-1", "ar1", "ok", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := en.StartActivity(ns, "act-2", "ar2", TypeRef{Name: "A"}, "tq", nil, nil, "", "", "", "", "me", "areq-2"); err != nil {
		t.Fatal(err)
	}

	owned := func(k string) bool {
		for _, p := range []string{
			runKey(ns, "wf-1", w1), "wfh/" + ns + "/wf-1/" + w1 + "/", "wfact/" + ns + "/wf-1/" + w1 + "/",
			runKey(ns, w2.WorkflowId, w2.RunId), "wfh/" + ns + "/" + w2.WorkflowId + "/" + w2.RunId + "/",
			actKey(ns, "act-1", "ar1"), "ahist/" + ns + "/act-1/ar1/",
		} {
			if strings.HasPrefix(k, p) {
				return true
			}
		}
		return k == "idem/"+ns+"/wf-1/req-1" || k == "sctrig/"+ns+"/sch/trig-1" || k == "aidem/"+ns+"/act-1/areq-1"
	}
	f := &purgeFixture{en: en, s: s, expired: map[string]bool{}}
	for k := range keysIn(t, s, ns) {
		if owned(k) {
			f.expired[k] = true
		}
	}
	for _, k := range []string{"idem/" + ns + "/wf-1/req-1", "sctrig/" + ns + "/sch/trig-1", "aidem/" + ns + "/act-1/areq-1"} {
		if !f.expired[k] {
			t.Fatalf("fixture lacks %s", k)
		}
	}
	n, ok, err := en.DescribeNamespace(ns)
	if err != nil || !ok {
		t.Fatalf("namespace: ok=%v err=%v", ok, err)
	}
	f.ns = *n
	return f
}

// diff returns the keys removed between before and after, and fails on any
// key added or changed.
func diff(t *testing.T, before, after map[string]string) map[string]bool {
	t.Helper()
	gone := map[string]bool{}
	for k, v := range before {
		w, ok := after[k]
		switch {
		case !ok:
			gone[k] = true
		case w != v:
			t.Errorf("key %s changed", k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			t.Errorf("key %s added", k)
		}
	}
	return gone
}

// TestPurge_RemovesExactlyTheExpiredRuns: before the retention passes the
// purge changes nothing. After it, every key of each terminal run goes — its
// row, history, activity records, and the idempotency entries that name it —
// and every other key, including all of the open runs', is untouched.
func TestPurge_RemovesExactlyTheExpiredRuns(t *testing.T) {
	f := newPurgeFixture(t)
	before := keysIn(t, f.s, "default")

	if n, err := f.en.purge(f.ns, time.Now().Add(719*time.Hour), purgeBatch); err != nil || n != 0 {
		t.Fatalf("purge before retention: n=%d err=%v", n, err)
	}
	if gone := diff(t, before, keysIn(t, f.s, "default")); len(gone) != 0 {
		t.Fatalf("purge before retention removed %v", gone)
	}

	n, err := f.en.purge(f.ns, time.Now().Add(721*time.Hour), purgeBatch)
	if err != nil || n != 3 {
		t.Fatalf("purge after retention: n=%d err=%v, want 3 runs", n, err)
	}
	gone := diff(t, before, keysIn(t, f.s, "default"))
	for k := range f.expired {
		if !gone[k] {
			t.Errorf("expired key kept: %s", k)
		}
	}
	for k := range gone {
		if !f.expired[k] {
			t.Errorf("key of a live run removed: %s", k)
		}
	}
}

// TestPurge_Idempotent: a second pass over a purged namespace changes
// nothing, and a run whose purge was cut short — some keys gone, its row
// still there — is finished by the next pass.
func TestPurge_Idempotent(t *testing.T) {
	f := newPurgeFixture(t)
	later := time.Now().Add(721 * time.Hour)
	if _, err := f.en.purge(f.ns, later, purgeBatch); err != nil {
		t.Fatal(err)
	}
	purged := keysIn(t, f.s, "default")
	if n, err := f.en.purge(f.ns, later, purgeBatch); err != nil || n != 0 {
		t.Fatalf("second pass: n=%d err=%v", n, err)
	}
	if gone := diff(t, purged, keysIn(t, f.s, "default")); len(gone) != 0 {
		t.Fatalf("second pass removed %v", gone)
	}

	run := startRun(t, f.en, "default", "wf-cut", "req-cut")
	finish(t, f.en, "default", "wf-cut", run)
	sh := shardOf(t, f.s, "default")
	for k := range keysIn(t, f.s, "default") {
		if strings.HasPrefix(k, "wfh/default/wf-cut/") && !strings.HasSuffix(k, "1") {
			if err := sh.Del(context.Background(), k); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := f.en.purge(f.ns, later, purgeBatch); err != nil || n != 1 {
		t.Fatalf("finishing pass: n=%d err=%v", n, err)
	}
	for k := range keysIn(t, f.s, "default") {
		if strings.Contains(k, "wf-cut") {
			t.Errorf("cut-short run left %s", k)
		}
	}
}

// TestPurge_BoundedPassesReachEveryRun: each pass reads at most limit rows,
// the walk resumes where the last pass stopped and wraps at the end, and
// across passes every expired run goes exactly once while the open one stays.
func TestPurge_BoundedPassesReachEveryRun(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	regDefaultNS(t, en)
	var runs []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("wf-%d", i)
		run := startRun(t, en, "default", id, "")
		finish(t, en, "default", id, run)
		runs = append(runs, runKey("default", id, run))
	}
	open := startRun(t, en, "default", "wf-2-open", "")
	n, _, _ := en.DescribeNamespace("default")

	later := time.Now().Add(721 * time.Hour)
	total := 0
	for pass := 0; pass < 10 && total < len(runs); pass++ {
		got, err := en.purge(*n, later, 2)
		if err != nil {
			t.Fatal(err)
		}
		if got > 2 {
			t.Fatalf("pass %d purged %d runs, over its bound of 2 rows", pass, got)
		}
		total += got
	}
	if total != len(runs) {
		t.Fatalf("purged %d runs, want %d", total, len(runs))
	}
	keys := keysIn(t, s, "default")
	for _, k := range runs {
		if _, ok := keys[k]; ok {
			t.Errorf("expired run kept: %s", k)
		}
	}
	if _, ok := keys[runKey("default", "wf-2-open", open)]; !ok {
		t.Fatal("the open run was purged")
	}
	if _, ok := keys[openKey("default", "wf-2-open", open)]; !ok {
		t.Fatal("the open run lost its index entry")
	}
}

// TestPurge_RefusesUnreadableRetention: a retention that does not parse
// purges nothing.
func TestPurge_RefusesUnreadableRetention(t *testing.T) {
	f := newPurgeFixture(t)
	before := keysIn(t, f.s, "default")
	ns := f.ns
	ns.Config.WorkflowExecutionRetentionTtl = "thirty days"
	if _, err := f.en.purge(ns, time.Now().Add(100000*time.Hour), purgeBatch); err == nil {
		t.Fatal("purge accepted an unreadable retention")
	}
	if gone := diff(t, before, keysIn(t, f.s, "default")); len(gone) != 0 {
		t.Fatalf("purge under an unreadable retention removed %v", gone)
	}
}

// TestPurge_ShrinksTheShard: once a walk has removed a namespace's expired
// runs, the shard file on disk shrinks to what is left.
func TestPurge_ShrinksTheShard(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	regDefaultNS(t, en)
	pad := strings.Repeat("x", 4096)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("wf-%03d", i)
		wf, err := en.StartWorkflow("default", id, "", TypeRef{Name: "W"}, "tq", []any{pad})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.TerminateWorkflow("default", id, wf.Execution.RunId); err != nil {
			t.Fatal(err)
		}
	}
	sh := shardOf(t, s, "default")
	size := func() int64 {
		t.Helper()
		if err := sh.Checkpoint(); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(sh.Path())
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	full := size()
	n, _, _ := en.DescribeNamespace("default")
	if got, err := en.purge(*n, time.Now().Add(721*time.Hour), purgeBatch); err != nil || got != 200 {
		t.Fatalf("purge: n=%d err=%v", got, err)
	}
	if after := size(); after*4 > full {
		t.Fatalf("shard is %d bytes after purging every run, was %d", after, full)
	}
}
