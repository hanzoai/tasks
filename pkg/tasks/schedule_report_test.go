// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// These tests pin the OBSERVABILITY half of the cron sweeper. The firing
// half is covered by schedule_fire_test.go; what is covered here is that a
// fire which does not reach a worker says so.
//
// The motivating incident: cloud's clients/cron schedules fire a
// JobWorkflow whose activity re-reads the entry's ConfigMap at fire time.
// The cloud ServiceAccount had no RBAC to read ConfigMaps, so the activity
// failed on every fire — for 11 days, across six nightly backups, while the
// engine emitted NOTHING and every dashboard read healthy. The engine's own
// durable records were the only witness. Isolation (one broken schedule
// must not stop the sweep) is correct and stays; silence is the bug.
//
// The property asserted is "the fire REACHED A WORKER", not "the namespace
// was registered". A namespace is an addressing fact created on first use,
// so its absence was never the fault — it was a proxy for one, and a proxy
// that misses the three shapes an operator actually hits: a cron naming a
// namespace nobody serves, a schedule whose worker died, a task queue that
// was renamed out from under it. All three start the run and then execute
// nothing, which is exactly the silence this file exists to break.

// captureLogs points the engine's logger at a buffer and returns a reader
// over the structured records it captured. JSON so assertions are on
// FIELDS, not on substrings of a formatted line.
func captureLogs(en *engine) func() []map[string]any {
	buf := &bytes.Buffer{}
	en.log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				continue
			}
			out = append(out, m)
		}
		return out
	}
}

// withMsg filters captured records down to one log message.
func withMsg(recs []map[string]any, msg string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if s, _ := r["msg"].(string); s == msg {
			out = append(out, r)
		}
	}
	return out
}

// wantFields asserts every key/value pair is present on the record.
// Numbers arrive as float64 through JSON, so ints are compared numerically.
func wantFields(t *testing.T, rec map[string]any, want map[string]any) {
	t.Helper()
	for k, v := range want {
		got, ok := rec[k]
		if !ok {
			t.Fatalf("log record missing field %q; got %v", k, rec)
		}
		if n, isInt := v.(int); isInt {
			f, isNum := got.(float64)
			if !isNum || int(f) != n {
				t.Fatalf("log field %q = %v, want %d", k, got, n)
			}
			continue
		}
		if got != v {
			t.Fatalf("log field %q = %v, want %v", k, got, v)
		}
	}
}

// dueSchedule creates a schedule and backdates it so the next sweep fires
// it. Whether that fire reaches anything is decided separately, by whether
// a worker is subscribed — the two are orthogonal, so the tests compose
// them instead of each minting its own flavour of broken.
func dueSchedule(t *testing.T, en *engine, ns, id, wfType, queue string) {
	t.Helper()
	if err := en.CreateSchedule(Schedule{
		ScheduleId: id,
		Namespace:  ns,
		Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: wfType}, TaskQueue: queue},
	}); err != nil {
		t.Fatalf("CreateSchedule(%s): %v", id, err)
	}
	backdate(t, en, ns, id, 5*time.Minute)
}

// subscribe puts a worker on (ns, queue) and returns its subscription id,
// so a test can take the worker away again. The dispatcher is shared across
// every tenant view, which is why an org's schedule is served by a worker
// subscribed through the root engine.
func subscribe(t *testing.T, en *engine, ns, queue string) string {
	t.Helper()
	id, err := en.disp.Subscribe(newRandID(), ns, queue, kindWorkflow)
	if err != nil {
		t.Fatalf("Subscribe(%s/%s): %v", ns, queue, err)
	}
	return id
}

// TestScheduleSweepReportsBrokenAction is the regression test for the
// silent-cron defect: a fire that reaches no worker must produce a log line
// carrying everything an operator needs to act — and the sweep must still
// fire everyone else.
//
// "ghost" is a namespace nobody serves. It is created by the fire, the run
// is recorded, and then NOTHING executes it — the exact shape of a cron
// entry with a typo in its namespace, and the reason "is the namespace
// registered?" is not the question worth asking.
func TestScheduleSweepReportsBrokenAction(t *testing.T) {
	en := newEngine(newStore())
	logs := captureLogs(en)
	// Sorts before the healthy entry so the test also proves iteration
	// continues past the failure.
	dueSchedule(t, en, "ghost", "a-broken", "GhostProbe", "ghost-q")
	dueSchedule(t, en, "default", "b-healthy", "HealthyProbe", "ok-q")
	subscribe(t, en, "default", "ok-q")

	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}

	fails := withMsg(logs(), msgActionFailed)
	if len(fails) != 1 {
		t.Fatalf("failure log lines = %d, want 1; records: %v", len(fails), logs())
	}
	wantFields(t, fails[0], map[string]any{
		"level":               "ERROR",
		"org":                 "",
		"namespace":           "ghost",
		"scheduleId":          "a-broken",
		"workflowType":        "GhostProbe",
		"taskQueue":           "ghost-q",
		"consecutiveFailures": 1,
	})
	if errText, _ := fails[0]["error"].(string); !strings.Contains(errText, ErrNoWorkersSubscribed.Error()) {
		t.Fatalf("error field = %q, want it to name the cause", errText)
	}

	// The run EXISTS. That is what makes the old proxy insufficient and
	// this assertion the point: a start that succeeded is not a fire that
	// ran, and only the report tells them apart.
	if got := countByType(t, en, "ghost", "GhostProbe"); got != 1 {
		t.Fatalf("GhostProbe executions = %d, want 1 — the run is started, just unreachable", got)
	}

	// Isolation is the behaviour we are NOT changing: the healthy sibling
	// still fired despite the broken one being swept first.
	if got := countByType(t, en, "default", "HealthyProbe"); got != 1 {
		t.Fatalf("HealthyProbe executions = %d, want 1 (reporting must not break isolation)", got)
	}
}

// TestScheduleSweepReportsOrgOnBrokenAction proves the org reaches the log
// line. The root sweeper fires other tenants' schedules through a throwaway
// view; without the org a cloud operator cannot tell WHOSE nightly backup
// is dead, which is exactly the question the 11-day incident asked.
func TestScheduleSweepReportsOrgOnBrokenAction(t *testing.T) {
	root := newEngine(newStore())
	logs := captureLogs(root)
	org := root.As(Org("acme"))
	dueSchedule(t, org, "acme-ns", "org-broken", "OrgProbe", "org-q")

	if err := root.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}

	fails := withMsg(logs(), msgActionFailed)
	if len(fails) != 1 {
		t.Fatalf("failure log lines = %d, want 1; records: %v", len(fails), logs())
	}
	wantFields(t, fails[0], map[string]any{
		"org":        "acme",
		"namespace":  "acme-ns",
		"scheduleId": "org-broken",
	})
}

// TestScheduleSweepThrottlesRepeatFailures proves the report does not
// flood. A schedule firing into a queue nobody serves keeps firing on its
// own cadence and keeps reaching nobody — unthrottled that is a line every
// tick forever, which hides the signal just as well as silence.
// First failure alerts, then one line per failReportThrottle sweeps.
func TestScheduleSweepThrottlesRepeatFailures(t *testing.T) {
	en := newEngine(newStore())
	logs := captureLogs(en)
	dueSchedule(t, en, "ghost", "loud", "GhostProbe", "q")

	for i := 0; i < failReportThrottle; i++ {
		// A fire that STARTED re-anchors, so make it due again rather than
		// leaning on a failed start leaving it due.
		backdate(t, en, "ghost", "loud", 5*time.Minute)
		if err := en.sweepSchedules(); err != nil {
			t.Fatalf("sweepSchedules #%d: %v", i+1, err)
		}
	}

	fails := withMsg(logs(), msgActionFailed)
	if len(fails) != 2 {
		t.Fatalf("log lines after %d failing sweeps = %d, want 2 (first + one throttled heartbeat)",
			failReportThrottle, len(fails))
	}
	wantFields(t, fails[0], map[string]any{"consecutiveFailures": 1})
	wantFields(t, fails[1], map[string]any{"consecutiveFailures": failReportThrottle})
}

// TestScheduleSweepReportsRecoveryAndRelapse walks the whole tracker
// lifecycle: fail (alert) → fail quietly (throttled) → succeed (recovery
// line naming how long it was broken) → fail again (alerts IMMEDIATELY,
// not on the old throttle cadence). Without the recovery line an operator
// cannot distinguish a fixed schedule from one whose next throttled line
// simply has not come round yet.
func TestScheduleSweepReportsRecoveryAndRelapse(t *testing.T) {
	en := newEngine(newStore())
	logs := captureLogs(en)
	// A worker IS subscribed, so recovery means what it says: the action
	// started AND reached somebody.
	subscribe(t, en, "default", "flappy-q")
	// Broken by an EMPTY workflow type, so the fault can be introduced and
	// repaired in place by rewriting the row.
	if err := en.CreateSchedule(Schedule{
		ScheduleId: "flappy",
		Namespace:  "default",
		Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
		Action:     ScheduleAction{TaskQueue: "flappy-q"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}

	// Three failing sweeps → exactly one line (first alert, rest throttled).
	for i := 0; i < 3; i++ {
		backdate(t, en, "default", "flappy", 5*time.Minute)
		if err := en.sweepSchedules(); err != nil {
			t.Fatalf("failing sweep #%d: %v", i+1, err)
		}
	}
	if n := len(withMsg(logs(), msgActionFailed)); n != 1 {
		t.Fatalf("failure lines after 3 failing sweeps = %d, want 1", n)
	}

	// Repair the action, then sweep: recovery must be reported, and must
	// carry how many consecutive failures preceded it.
	setWorkflowType(t, en, "default", "flappy", "FlappyProbe")
	backdate(t, en, "default", "flappy", 5*time.Minute)
	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("recovery sweep: %v", err)
	}
	recovered := withMsg(logs(), msgActionRecovered)
	if len(recovered) != 1 {
		t.Fatalf("recovery lines = %d, want 1; records: %v", len(recovered), logs())
	}
	wantFields(t, recovered[0], map[string]any{
		"level":                    "INFO",
		"namespace":                "default",
		"scheduleId":               "flappy",
		"afterConsecutiveFailures": 3,
	})
	if got := countByType(t, en, "default", "FlappyProbe"); got != 1 {
		t.Fatalf("FlappyProbe executions = %d, want 1", got)
	}

	// Break it again: the counter was cleared, so this alerts at once
	// instead of waiting out the throttle.
	setWorkflowType(t, en, "default", "flappy", "")
	backdate(t, en, "default", "flappy", 5*time.Minute)
	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("relapse sweep: %v", err)
	}
	fails := withMsg(logs(), msgActionFailed)
	if len(fails) != 2 {
		t.Fatalf("failure lines after relapse = %d, want 2", len(fails))
	}
	wantFields(t, fails[1], map[string]any{"consecutiveFailures": 1})
}

// TestScheduleSweepReportsWorkerGone covers what the old
// namespace-registration proxy could never see: a schedule that is
// perfectly configured, in a namespace that exists, whose WORKER went away.
// Nothing about the entry changed; it simply stopped running. The report
// must open when the worker leaves and close when one comes back.
func TestScheduleSweepReportsWorkerGone(t *testing.T) {
	en := newEngine(newStore())
	logs := captureLogs(en)
	sub := subscribe(t, en, "default", "nightly-q")
	dueSchedule(t, en, "default", "nightly", "BackupWorkflow", "nightly-q")

	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("healthy sweep: %v", err)
	}
	if n := len(withMsg(logs(), msgActionFailed)); n != 0 {
		t.Fatalf("failure lines while a worker is subscribed = %d, want 0; records: %v", n, logs())
	}

	// The worker goes away. The schedule is untouched.
	en.disp.Unsubscribe(sub)
	backdate(t, en, "default", "nightly", 5*time.Minute)
	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("orphaned sweep: %v", err)
	}
	fails := withMsg(logs(), msgActionFailed)
	if len(fails) != 1 {
		t.Fatalf("failure lines after the worker left = %d, want 1; records: %v", len(fails), logs())
	}
	wantFields(t, fails[0], map[string]any{
		"namespace":           "default",
		"scheduleId":          "nightly",
		"taskQueue":           "nightly-q",
		"consecutiveFailures": 1,
	})

	// A worker comes back: the streak closes.
	subscribe(t, en, "default", "nightly-q")
	backdate(t, en, "default", "nightly", 5*time.Minute)
	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("recovery sweep: %v", err)
	}
	recovered := withMsg(logs(), msgActionRecovered)
	if len(recovered) != 1 {
		t.Fatalf("recovery lines = %d, want 1; records: %v", len(recovered), logs())
	}
	wantFields(t, recovered[0], map[string]any{"afterConsecutiveFailures": 1})
}

// setWorkflowType rewrites a stored schedule's action type in place,
// breaking ("") or repairing it without touching anything else.
func setWorkflowType(t *testing.T, en *engine, ns, id, wfType string) {
	t.Helper()
	s, ok, err := en.DescribeSchedule(ns, id)
	if err != nil || !ok {
		t.Fatalf("DescribeSchedule(%s/%s): ok=%v err=%v", ns, id, ok, err)
	}
	s.Action.WorkflowType.Name = wfType
	if err := en.store.put("sc/"+ns+"/"+id, *s); err != nil {
		t.Fatalf("setWorkflowType: %v", err)
	}
}

// TestSweepOnceReportsSweepError covers the other half of the defect: the
// scheduler tick used to discard the sweep error outright (`_ =
// e.sweepSchedules()`). A sweep that fails WHOLESALE stops cron for every
// org at once, and used to do it without a word.
func TestSweepOnceReportsSweepError(t *testing.T) {
	en := newEngine(newStore())
	logs := captureLogs(en)
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	// Closing the store makes every shard read fail, so the cross-org scan
	// that opens the sweep cannot complete.
	if err := en.store.close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := en.sweepSchedules(); err == nil {
		t.Fatal("sweepSchedules on a closed store returned nil; the test's premise is gone")
	}

	// The same throttle applies: a permanently unhappy store must not log
	// on every one of the 5s ticks either.
	for i := 0; i < failReportThrottle; i++ {
		en.sweepOnce()
	}

	sweeps := withMsg(logs(), msgSweepFailed)
	if len(sweeps) != 2 {
		t.Fatalf("sweep-failure lines after %d failing ticks = %d, want 2 (first + one throttled heartbeat); records: %v",
			failReportThrottle, len(sweeps), logs())
	}
	wantFields(t, sweeps[0], map[string]any{"level": "ERROR", "consecutiveFailures": 1})
	if errText, _ := sweeps[0]["error"].(string); errText == "" {
		t.Fatal("sweep-failure line carries no error field")
	}
}
