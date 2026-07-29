// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// These tests pin the ACTIVITY/WORKFLOW half of failure reporting.
// schedule_report_test.go covers the sweeper — the layer that was already
// healthy during the incident.
//
// The incident: cloud's clients/cron schedules fire a JobWorkflow whose
// RunJobActivity re-reads the entry's ConfigMap at fire time. The cloud
// ServiceAccount had no RBAC to read ConfigMaps, so the ACTIVITY failed on
// every single fire for 11 days. StartWorkflow SUCCEEDED every time —
// actionCount reached 4489 — so the scheduler was correct AND silent, and
// the fault lived one layer down where the only output was
// e.emit(Event{Kind:"activity.failed"}) into an SSE broker with nobody
// subscribed. Six nightly backups produced nothing; every dashboard read
// green; it was found by hand-reading SQLite.
//
// Every test here therefore attaches NO SSE subscriber, exactly as
// production had none. Without the fix each one reports the empty evidence
// that is the bug itself.

// failNS registers a namespace and returns a running workflow to hang
// activities off.
func failNS(t *testing.T, en *engine, ns, wfID, wfType, queue string) string {
	t.Helper()
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: ns}}); err != nil {
		t.Fatalf("RegisterNamespace(%s): %v", ns, err)
	}
	wf, err := en.StartWorkflow(ns, wfID, "", TypeRef{Name: wfType}, queue, []any{})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	return wf.Execution.RunId
}

// schedActivity schedules (ns, run, seq) so it can then be failed.
func schedActivity(t *testing.T, en *engine, ns, wfID, run string, seq int, actType, queue string, rp *client.RetryPolicyJSON) {
	t.Helper()
	unlock := en.lockRun(ns, wfID, run)
	defer unlock()
	if err := en.applyScheduleActivity(ns, wfID, run, seq, actType, []byte(`[]`), queue, 0, 0, rp); err != nil {
		t.Fatalf("applyScheduleActivity seq=%d: %v", seq, err)
	}
}

// rbacDenied is the incident's error verbatim in shape: a permanent,
// non-retryable fault that no amount of retrying can fix.
func rbacDenied(t *testing.T) []byte {
	t.Helper()
	b, err := temporal.Encode(temporal.NewError(
		`configmaps "cron-reconcile" is forbidden: User "system:serviceaccount:hanzo:cloud" cannot get resource "configmaps"`,
		temporal.CodeApplication, true))
	if err != nil {
		t.Fatalf("encode failure: %v", err)
	}
	return b
}

// onlyStreak returns the single failure streak in ns, failing the test with
// the exact silence the incident produced when there is none.
func onlyStreak(t *testing.T, en *engine, ns string) FailureStreak {
	t.Helper()
	rows, err := en.FailureStreaks(ns)
	if err != nil {
		t.Fatalf("FailureStreaks(%s): %v", ns, err)
	}
	if len(rows) != 1 {
		t.Fatalf("durable failure streaks = %d, want 1; rows: %+v", len(rows), rows)
	}
	return rows[0]
}

// TestActivityFailureIsDurableWithNoSubscriber is the core regression test.
// An activity that fails with nobody watching must leave evidence in the
// store — the guarantee the SSE-only path never provided.
func TestActivityFailureIsDurableWithNoSubscriber(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	logs := captureLogs(en)
	run := failNS(t, en, "default", "job-1", "JobWorkflow", "cron-q")
	schedActivity(t, en, "default", "job-1", run, 0, "RunJobActivity", "cron-q", nil)

	if err := en.completeWorkflowActivity("default", "job-1", run, 0, nil, rbacDenied(t)); err != nil {
		t.Fatalf("completeWorkflowActivity: %v", err)
	}

	rec := onlyStreak(t, en, "default")
	if rec.Namespace != "default" || rec.WorkflowType != "JobWorkflow" ||
		rec.ActivityType != "RunJobActivity" || rec.TaskQueue != "cron-q" {
		t.Fatalf("streak identity = %+v, want ns=default JobWorkflow/RunJobActivity on cron-q", rec)
	}
	if rec.ConsecutiveFailures != 1 {
		t.Fatalf("consecutiveFailures = %d, want 1", rec.ConsecutiveFailures)
	}
	if rec.Retrying {
		t.Fatal("retrying = true, want false: a non-retryable failure is terminal for this run")
	}
	if rec.LastRunId != run || rec.LastWorkflowId != "job-1" {
		t.Fatalf("streak points at %s/%s, want job-1/%s — a row you cannot trace to a run is not evidence",
			rec.LastWorkflowId, rec.LastRunId, run)
	}
	if rec.LastError == "" || rec.FirstFailureTime == "" {
		t.Fatalf("streak carries no error/time: %+v", rec)
	}

	// The log floor: an operator who only ever sees stdout still gets it.
	fails := withMsg(logs(), msgActivityFailed)
	if len(fails) != 1 {
		t.Fatalf("log lines = %d, want 1; records: %v", len(fails), logs())
	}
	wantFields(t, fails[0], map[string]any{
		"namespace":           "default",
		"workflowType":        "JobWorkflow",
		"activityType":        "RunJobActivity",
		"taskQueue":           "cron-q",
		"consecutiveFailures": 1,
		"retrying":            false,
	})
}

// TestActivityFailureStreakOutlivesProcessRestart is the "outlives process
// restart" half of the guarantee. An 11-day outage spans deploys; a counter
// that resets on boot can never say "this has been dead for 11 days", which
// is the one number that would have ended the incident on day one.
func TestActivityFailureStreakOutlivesProcessRestart(t *testing.T) {
	s := newStore()
	defer s.close()

	// Engine A: three failed fires, each a fresh run — the incident's shape,
	// where a run-keyed counter would read "1" three times.
	enA := newEngine(s)
	captureLogs(enA)
	for i, wfID := range []string{"fire-1", "fire-2", "fire-3"} {
		if i == 0 {
			failNS(t, enA, "default", wfID, "JobWorkflow", "cron-q")
		} else if _, err := enA.StartWorkflow("default", wfID, "", TypeRef{Name: "JobWorkflow"}, "cron-q", []any{}); err != nil {
			t.Fatalf("StartWorkflow(%s): %v", wfID, err)
		}
		wf, _, _ := enA.DescribeWorkflow("default", wfID, "")
		schedActivity(t, enA, "default", wfID, wf.Execution.RunId, 0, "RunJobActivity", "cron-q", nil)
		if err := enA.completeWorkflowActivity("default", wfID, wf.Execution.RunId, 0, nil, rbacDenied(t)); err != nil {
			t.Fatalf("fail %s: %v", wfID, err)
		}
	}
	if got := onlyStreak(t, enA, "default").ConsecutiveFailures; got != 3 {
		t.Fatalf("consecutiveFailures before restart = %d, want 3 — the count must span RUNS, not reset per run", got)
	}

	// Crash: a fresh engine (empty in-memory state) over the SAME store.
	enB := newEngine(s)
	captureLogs(enB)
	rec := onlyStreak(t, enB, "default")
	if rec.ConsecutiveFailures != 3 {
		t.Fatalf("consecutiveFailures after restart = %d, want 3", rec.ConsecutiveFailures)
	}
	if rec.ActivityType != "RunJobActivity" {
		t.Fatalf("streak identity lost across restart: %+v", rec)
	}

	// And it keeps counting from where it was, rather than starting over.
	if _, err := enB.StartWorkflow("default", "fire-4", "", TypeRef{Name: "JobWorkflow"}, "cron-q", []any{}); err != nil {
		t.Fatalf("StartWorkflow(fire-4): %v", err)
	}
	wf, _, _ := enB.DescribeWorkflow("default", "fire-4", "")
	schedActivity(t, enB, "default", "fire-4", wf.Execution.RunId, 0, "RunJobActivity", "cron-q", nil)
	if err := enB.completeWorkflowActivity("default", "fire-4", wf.Execution.RunId, 0, nil, rbacDenied(t)); err != nil {
		t.Fatalf("fail fire-4: %v", err)
	}
	if got := onlyStreak(t, enB, "default").ConsecutiveFailures; got != 4 {
		t.Fatalf("consecutiveFailures after restart+failure = %d, want 4", got)
	}
}

// TestScheduleFiredActivityFailureNamesTheSchedule is the incident end to
// end: a cron schedule fires, its activity fails, and the evidence must name
// the SCHEDULE. Without it a report can only say "some JobWorkflow is
// broken" — never "the nightly backup has not run", which is the question
// eleven days of missing backups actually asked.
func TestScheduleFiredActivityFailureNamesTheSchedule(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	captureLogs(en)
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	if err := en.CreateSchedule(Schedule{
		ScheduleId: "nightly-backup",
		Namespace:  "default",
		Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: "JobWorkflow"}, TaskQueue: "cron-q"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	backdate(t, en, "default", "nightly-backup", 5*time.Minute)
	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}

	// The fire succeeded — exactly as it did 4489 times in production.
	runs, err := en.ListWorkflows("default")
	if err != nil || len(runs) != 1 {
		t.Fatalf("workflows after sweep = %d (err=%v), want 1", len(runs), err)
	}
	wfID, run := runs[0].Execution.WorkflowId, runs[0].Execution.RunId

	// Now break the layer below, where the real fault lived.
	schedActivity(t, en, "default", wfID, run, 0, "RunJobActivity", "cron-q", nil)
	if err := en.completeWorkflowActivity("default", wfID, run, 0, nil, rbacDenied(t)); err != nil {
		t.Fatalf("completeWorkflowActivity: %v", err)
	}

	if got := onlyStreak(t, en, "default").ScheduleId; got != "nightly-backup" {
		t.Fatalf("streak scheduleId = %q, want %q — the failure must name the cron entry that is dead", got, "nightly-backup")
	}
}

// TestActivityFailureIsThrottled proves the report does not become the new
// flood. A broken cron re-fires forever; unthrottled that is thousands of
// identical lines a day, which buries the signal as thoroughly as silence
// did. The DURABLE count stays exact — throttling is about lines, never
// about evidence.
func TestActivityFailureIsThrottled(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	logs := captureLogs(en)
	run := failNS(t, en, "default", "job-loud", "JobWorkflow", "cron-q")

	const fires = 3 * failReportThrottle
	for i := 0; i < fires; i++ {
		schedActivity(t, en, "default", "job-loud", run, i, "RunJobActivity", "cron-q", nil)
		if err := en.completeWorkflowActivity("default", "job-loud", run, i, nil, rbacDenied(t)); err != nil {
			t.Fatalf("fail #%d: %v", i+1, err)
		}
	}

	lines := len(withMsg(logs(), msgActivityFailed)) + len(withMsg(logs(), msgActivityPersistent))
	if lines > fires/10 {
		t.Fatalf("log lines after %d failures = %d, want the throttle to hold it near %d",
			fires, lines, fires/failReportThrottle)
	}
	if lines == 0 {
		t.Fatal("log lines = 0: throttling must not mean silence")
	}
	if got := onlyStreak(t, en, "default").ConsecutiveFailures; got != fires {
		t.Fatalf("durable consecutiveFailures = %d, want %d — the throttle bounds LINES, not evidence", got, fires)
	}
}

// TestActivityFailurePersistentIsAlertable is the distinction the incident
// turned on: "failed once and will retry" (normal, WARN) versus "has failed
// every attempt for a long time" (the outage, ERROR). Both were equally
// invisible before; reporting them at the same volume would be almost as
// useless.
func TestActivityFailurePersistentIsAlertable(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	logs := captureLogs(en)
	run := failNS(t, en, "default", "job-p", "JobWorkflow", "cron-q")

	// A retryable failure with attempts left: the benign case.
	schedActivity(t, en, "default", "job-p", run, 0, "RunJobActivity", "cron-q",
		&client.RetryPolicyJSON{InitialIntervalMs: 1, MaximumAttempts: 5})
	transient, err := temporal.Encode(temporal.NewError("connection reset", temporal.CodeApplication, false))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := en.completeWorkflowActivity("default", "job-p", run, 0, nil, transient); err != nil {
		t.Fatalf("transient failure: %v", err)
	}
	warns := withMsg(logs(), msgActivityFailed)
	if len(warns) != 1 {
		t.Fatalf("lines for a first, retryable failure = %d, want 1; records: %v", len(warns), logs())
	}
	wantFields(t, warns[0], map[string]any{"level": "WARN", "retrying": true, "attempt": 1})
	if n := len(withMsg(logs(), msgActivityPersistent)); n != 0 {
		t.Fatalf("persistent lines after ONE failure = %d, want 0 — a blip must not page anyone", n)
	}

	// Age the streak into the incident's shape: still failing, long enough
	// that no single run's retry budget explains it.
	id := failureIdentity{Namespace: "default", WorkflowType: "JobWorkflow", ActivityType: "RunJobActivity", TaskQueue: "cron-q"}
	key := failKey("default", id.fingerprint())
	var rec FailureStreak
	if ok, _ := en.store.get(key, &rec); !ok {
		t.Fatal("no streak row to age; the test's premise is gone")
	}
	rec.FirstFailureTime = time.Now().UTC().Add(-11 * 24 * time.Hour).Format(time.RFC3339)
	if err := en.store.put(key, rec); err != nil {
		t.Fatalf("age streak: %v", err)
	}

	schedActivity(t, en, "default", "job-p", run, 1, "RunJobActivity", "cron-q", nil)
	if err := en.completeWorkflowActivity("default", "job-p", run, 1, nil, rbacDenied(t)); err != nil {
		t.Fatalf("persistent failure: %v", err)
	}

	// Reports on the TRANSITION, not on the throttle's next tick: a nightly
	// cron reaches only its second failure in two days, and waiting for #60
	// would be the original bug wearing a different hat.
	persistent := withMsg(logs(), msgActivityPersistent)
	if len(persistent) != 1 {
		t.Fatalf("persistent lines = %d, want 1 (reported at the crossing); records: %v", len(persistent), logs())
	}
	wantFields(t, persistent[0], map[string]any{
		"level":               "ERROR",
		"activityType":        "RunJobActivity",
		"consecutiveFailures": 2,
	})
	if !onlyStreak(t, en, "default").Persistent {
		t.Fatal("durable streak not marked persistent")
	}
}

// TestActivityRecoveryClearsStreak closes the loop the throttle opens: with
// no recovery line an operator cannot tell a fixed activity from one whose
// next throttled line has not come round yet. The durable row must go too —
// the keyspace answers "what is broken NOW".
func TestActivityRecoveryClearsStreak(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	logs := captureLogs(en)
	run := failNS(t, en, "default", "job-r", "JobWorkflow", "cron-q")

	schedActivity(t, en, "default", "job-r", run, 0, "RunJobActivity", "cron-q", nil)
	if err := en.completeWorkflowActivity("default", "job-r", run, 0, nil, rbacDenied(t)); err != nil {
		t.Fatalf("fail: %v", err)
	}
	onlyStreak(t, en, "default") // must exist before we fix it

	schedActivity(t, en, "default", "job-r", run, 1, "RunJobActivity", "cron-q", nil)
	if err := en.completeWorkflowActivity("default", "job-r", run, 1, []byte(`"ok"`), nil); err != nil {
		t.Fatalf("succeed: %v", err)
	}

	rows, err := en.FailureStreaks("default")
	if err != nil {
		t.Fatalf("FailureStreaks: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("streaks after recovery = %d, want 0; rows: %+v", len(rows), rows)
	}
	recovered := withMsg(logs(), msgActivityRecovered)
	if len(recovered) != 1 {
		t.Fatalf("recovery lines = %d, want 1; records: %v", len(recovered), logs())
	}
	wantFields(t, recovered[0], map[string]any{
		"level":                    "INFO",
		"activityType":             "RunJobActivity",
		"afterConsecutiveFailures": 1,
	})
}

// TestStandaloneActivityFailureRecorded covers the other activity family —
// the fleet queue's fn.run / studio.render jobs, which never touch the
// workflow path and had exactly the same SSE-only output.
func TestStandaloneActivityFailureRecorded(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	logs := captureLogs(en)
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "gpu-jobs"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	start := func(id string) *StandaloneActivity {
		t.Helper()
		a, err := en.StartActivity("gpu-jobs", id, "", TypeRef{Name: "studio.render"}, "gpu-q",
			nil, nil, "", "", "", "", "worker-1", "")
		if err != nil {
			t.Fatalf("StartActivity(%s): %v", id, err)
		}
		return a
	}

	a := start("render-1")
	if err := en.FailActivity("gpu-jobs", "render-1", a.Execution.RunId, "CUDA out of memory", "worker-1"); err != nil {
		t.Fatalf("FailActivity: %v", err)
	}
	rec := onlyStreak(t, en, "gpu-jobs")
	if rec.ActivityType != "studio.render" || rec.TaskQueue != "gpu-q" || rec.LastError != "CUDA out of memory" {
		t.Fatalf("standalone streak = %+v, want studio.render on gpu-q with the cause", rec)
	}
	if rec.WorkflowType != "" {
		t.Fatalf("standalone streak claims workflowType %q; there is no enclosing workflow", rec.WorkflowType)
	}
	if n := len(withMsg(logs(), msgActivityFailed)); n != 1 {
		t.Fatalf("log lines = %d, want 1; records: %v", n, logs())
	}

	// One success proves the fault is not systematic, so the streak ends.
	b := start("render-2")
	if err := en.CompleteActivity("gpu-jobs", "render-2", b.Execution.RunId, "frame.png", "worker-1"); err != nil {
		t.Fatalf("CompleteActivity: %v", err)
	}
	rows, err := en.FailureStreaks("gpu-jobs")
	if err != nil {
		t.Fatalf("FailureStreaks: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("streaks after a successful render = %d, want 0; rows: %+v", len(rows), rows)
	}
}

// TestWorkflowFailureRecorded covers the hole an activity-only report would
// leave: a decider that faults before scheduling anything (bad input, a
// panic on episode 0) fails a workflow with no activity record to speak for
// it — the same class of bug with an even thinner trail.
func TestWorkflowFailureRecorded(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	logs := captureLogs(en)
	failNS(t, en, "default", "job-w", "JobWorkflow", "cron-q")

	boom, err := temporal.Encode(temporal.NewError("unmarshal job spec: unexpected end of JSON input", temporal.CodeApplication, true))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := en.terminalTransition("default", "job-w", "", "WORKFLOW_EXECUTION_STATUS_FAILED",
		"workflow.failed", "WORKFLOW_EXECUTION_FAILED", map[string]any{"failure": string(boom)}); err != nil {
		t.Fatalf("terminalTransition: %v", err)
	}

	rec := onlyStreak(t, en, "default")
	if rec.WorkflowType != "JobWorkflow" || rec.TaskQueue != "cron-q" {
		t.Fatalf("workflow streak = %+v, want JobWorkflow on cron-q", rec)
	}
	if rec.ActivityType != "" {
		t.Fatalf("workflow streak names activityType %q; nothing was scheduled", rec.ActivityType)
	}
	if rec.LastError == "" {
		t.Fatal("workflow streak carries no error: the decoded failure is the actionable part")
	}
	fails := withMsg(logs(), msgWorkflowFailed)
	if len(fails) != 1 {
		t.Fatalf("workflow failure lines = %d, want 1; records: %v", len(fails), logs())
	}
	wantFields(t, fails[0], map[string]any{"workflowType": "JobWorkflow", "consecutiveFailures": 1})
}

// TestFailureReportingPreservesRetrySemantics is the guard on the promise
// that this change only OBSERVES. The activity must still be retried exactly
// MaximumAttempts times, still end FAILED, and still leave the history it
// left before — reporting rides alongside, it does not steer.
func TestFailureReportingPreservesRetrySemantics(t *testing.T) {
	s := newStore()
	defer s.close()
	en := newEngine(s)
	captureLogs(en)
	cap := &captureSend{}
	en.disp.send = cap.fn
	run := failNS(t, en, "default", "job-s", "JobWorkflow", "tq")
	if _, err := en.disp.Subscribe("actPeer", "default", "tq", kindActivity); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	const maxAttempts = 3
	schedActivity(t, en, "default", "job-s", run, 0, "RunJobActivity", "tq",
		&client.RetryPolicyJSON{InitialIntervalMs: 1, BackoffCoefficient: 1, MaximumAttempts: maxAttempts})
	retryable, err := temporal.Encode(temporal.NewError("timeout", temporal.CodeApplication, false))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < maxAttempts; i++ {
		if err := en.completeWorkflowActivity("default", "job-s", run, 0, nil, retryable); err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		// The retry is armed on a timer; wait for the re-dispatch so the next
		// completion lands on the attempt this one scheduled.
		if i < maxAttempts-1 {
			waitFor(t, func() bool {
				rec, ok, _ := en.getWorkflowActivity("default", "job-s", run, 0)
				return ok && rec.Attempt == i+2 && rec.Status == wfActStarted
			}, "re-dispatch of attempt %d", i+2)
		}
	}

	rec, ok, _ := en.getWorkflowActivity("default", "job-s", run, 0)
	if !ok || rec.Status != wfActFailed {
		t.Fatalf("activity status = %v (ok=%v), want %s after exhausting retries", rec, ok, wfActFailed)
	}
	if rec.Attempt != maxAttempts {
		t.Fatalf("attempts = %d, want %d — reporting must not change the retry budget", rec.Attempt, maxAttempts)
	}
	if got := countEventType(t, en, "job-s", run, evtActivityFailed); got != 1 {
		t.Fatalf("ACTIVITY_TASK_FAILED events = %d, want 1 (terminal only)", got)
	}
	if got := countDeliveries(cap, OpcodeDeliverActivityTask); got != maxAttempts {
		t.Fatalf("activity dispatches = %d, want %d", got, maxAttempts)
	}
	// One streak spanning all three attempts, not three streaks: identity is
	// the shape of the work, not the attempt.
	if got := onlyStreak(t, en, "default").ConsecutiveFailures; got != maxAttempts {
		t.Fatalf("consecutiveFailures = %d, want %d", got, maxAttempts)
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool, what string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for "+what, args...)
}

// TestFailureStreaksAreOrgScoped: two orgs breaking the same activity type
// must not share a row or a count. Multi-tenancy is not observability's to
// blur — "whose nightly backup is dead" is the first question asked.
func TestFailureStreaksAreOrgScoped(t *testing.T) {
	s := newStore()
	defer s.close()
	root := newEngine(s)
	captureLogs(root)

	for _, org := range []string{"acme", "globex"} {
		en := root.As(Org(org))
		run := failNS(t, en, "default", "job-"+org, "JobWorkflow", "cron-q")
		schedActivity(t, en, "default", "job-"+org, run, 0, "RunJobActivity", "cron-q", nil)
		if err := en.completeWorkflowActivity("default", "job-"+org, run, 0, nil, rbacDenied(t)); err != nil {
			t.Fatalf("fail %s: %v", org, err)
		}
	}
	// Break acme a second time; globex must not move.
	acme := root.As(Org("acme"))
	if _, err := acme.StartWorkflow("default", "job-acme-2", "", TypeRef{Name: "JobWorkflow"}, "cron-q", []any{}); err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	wf, _, _ := acme.DescribeWorkflow("default", "job-acme-2", "")
	schedActivity(t, acme, "default", "job-acme-2", wf.Execution.RunId, 0, "RunJobActivity", "cron-q", nil)
	if err := acme.completeWorkflowActivity("default", "job-acme-2", wf.Execution.RunId, 0, nil, rbacDenied(t)); err != nil {
		t.Fatalf("fail acme again: %v", err)
	}

	acmeRec := onlyStreak(t, acme, "default")
	globexRec := onlyStreak(t, root.As(Org("globex")), "default")
	if acmeRec.Org != "acme" || globexRec.Org != "globex" {
		t.Fatalf("streak orgs = %q / %q, want acme / globex", acmeRec.Org, globexRec.Org)
	}
	if acmeRec.ConsecutiveFailures != 2 || globexRec.ConsecutiveFailures != 1 {
		t.Fatalf("counts = acme %d / globex %d, want 2 / 1 — one org's fault must not inflate another's",
			acmeRec.ConsecutiveFailures, globexRec.ConsecutiveFailures)
	}
}
