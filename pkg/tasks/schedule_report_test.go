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
// sweeper which cannot fire says so.
//
// The motivating incident: cloud's clients/cron schedules fire a
// JobWorkflow whose activity re-reads the entry's ConfigMap at fire time.
// The cloud ServiceAccount had no RBAC to read ConfigMaps, so the activity
// failed on every fire — for 11 days, across six nightly backups, while the
// engine emitted NOTHING and every dashboard read healthy. The engine's own
// durable records were the only witness. Isolation (one broken schedule
// must not stop the sweep) is correct and stays; silence is the bug.

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

const (
	msgActionFailed    = "tasks: cron schedule action failed"
	msgActionRecovered = "tasks: cron schedule action recovered"
	msgSweepFailed     = "tasks: schedule sweep failed, no schedule fired this tick"
)

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

// brokenSchedule creates a due schedule in an UNREGISTERED namespace, so
// StartWorkflow refuses it on every sweep — the shape of a cron entry
// whose action is broken in a way the engine cannot fix by retrying.
func brokenSchedule(t *testing.T, en *engine, ns, id, wfType, queue string) {
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

// TestScheduleSweepReportsBrokenAction is the regression test for the
// silent-cron defect: a schedule whose action cannot start must produce a
// log line carrying everything an operator needs to act — and the sweep
// must still fire everyone else.
func TestScheduleSweepReportsBrokenAction(t *testing.T) {
	en := newEngine(newStore())
	logs := captureLogs(en)
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	// "ghost" is never registered. Sorts before the healthy entry so the
	// test also proves iteration continues past the failure.
	brokenSchedule(t, en, "ghost", "a-broken", "GhostProbe", "ghost-q")
	if err := en.CreateSchedule(Schedule{
		ScheduleId: "b-healthy",
		Namespace:  "default",
		Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: "HealthyProbe"}, TaskQueue: "ok-q"},
	}); err != nil {
		t.Fatalf("CreateSchedule(b-healthy): %v", err)
	}
	backdate(t, en, "default", "b-healthy", 5*time.Minute)

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
	if errText, _ := fails[0]["error"].(string); !strings.Contains(errText, "not registered") {
		t.Fatalf("error field = %q, want it to name the cause", errText)
	}

	// Isolation is the behaviour we are NOT changing: the healthy sibling
	// still fired despite the broken one being swept first.
	if got := countByType(t, en, "default", "HealthyProbe"); got != 1 {
		t.Fatalf("HealthyProbe executions = %d, want 1 (reporting must not break isolation)", got)
	}
}

// TestScheduleSweepReportsOrgOnBrokenAction proves the org reaches the log
// line. The root sweeper fires other orgs' schedules through a throwaway
// WithOrg view; without the org a cloud operator cannot tell WHOSE nightly
// backup is dead, which is exactly the question the 11-day incident asked.
func TestScheduleSweepReportsOrgOnBrokenAction(t *testing.T) {
	root := newEngine(newStore())
	logs := captureLogs(root)
	org := root.WithOrg("acme")
	brokenSchedule(t, org, "acme-ns", "org-broken", "OrgProbe", "org-q")

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
// flood. A failed fire is deliberately NOT re-anchored, so the entry stays
// due and is retried on every 5s tick — unthrottled that is a line every
// 5 seconds forever, which hides the signal just as well as silence.
// First failure alerts, then one line per failReportThrottle sweeps.
func TestScheduleSweepThrottlesRepeatFailures(t *testing.T) {
	en := newEngine(newStore())
	logs := captureLogs(en)
	brokenSchedule(t, en, "ghost", "loud", "GhostProbe", "q")

	for i := 0; i < failReportThrottle; i++ {
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
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	// Broken by an EMPTY workflow type: registered namespace, so the fault
	// can be introduced and repaired in place by rewriting the row.
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
