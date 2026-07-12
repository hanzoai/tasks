// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"strings"
	"testing"
	"time"
)

// These tests pin the property every durable-cron consumer (cloud
// clients/cron, base tools/cron, automations enableSchedule) stands on:
// the engine's sweeper actually starts workflows for due schedules — in
// the root shard AND in org-scoped shards — without replaying missed
// backlog. They drive sweepSchedules directly rather than waiting on the
// 5s runScheduler tick.

// backdate rewrites a schedule's anchor so its next fire time is in the
// past, as if it had been created (and never fired) minutes ago.
func backdate(t *testing.T, en *engine, ns, id string, ago time.Duration) {
	t.Helper()
	s, ok, err := en.DescribeSchedule(ns, id)
	if err != nil || !ok {
		t.Fatalf("DescribeSchedule(%s/%s): ok=%v err=%v", ns, id, ok, err)
	}
	s.Info.CreateTime = time.Now().UTC().Add(-ago).Format(time.RFC3339)
	s.Info.UpdateTime = ""
	if err := en.store.put("sc/"+ns+"/"+id, *s); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

func countByType(t *testing.T, en *engine, ns, wfType string) int {
	t.Helper()
	execs, err := en.ListWorkflows(ns)
	if err != nil {
		t.Fatalf("ListWorkflows(%s): %v", ns, err)
	}
	n := 0
	for _, e := range execs {
		if e.Type.Name == wfType {
			n++
		}
	}
	return n
}

func TestScheduleSweepFires(t *testing.T) {
	en := newEngine(newStore())
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	if err := en.CreateSchedule(Schedule{
		ScheduleId: "sweep-fire",
		Namespace:  "default",
		Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: "SweepProbe"}, TaskQueue: "sweep-q"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	backdate(t, en, "default", "sweep-fire", 5*time.Minute)

	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}
	if got := countByType(t, en, "default", "SweepProbe"); got != 1 {
		t.Fatalf("SweepProbe executions = %d, want 1", got)
	}

	// The schedule re-anchored at the fire: an immediate second sweep must
	// NOT fire again (next tick is in the future).
	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got := countByType(t, en, "default", "SweepProbe"); got != 1 {
		t.Fatalf("after second sweep executions = %d, want still 1", got)
	}
}

// TestScheduleSweepFiresOrgScoped proves the ROOT sweeper sees and fires
// schedules living in an org's shard (created through the exported View),
// and that the started workflow lands in that same org shard — the
// mechanics cloud's platform cron and its console visibility stand on.
func TestScheduleSweepFiresOrgScoped(t *testing.T) {
	root := newEngine(newStore())
	org := root.WithOrg("acme")
	if err := org.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "acme"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	if err := org.CreateSchedule(Schedule{
		ScheduleId: "org-fire",
		Namespace:  "acme",
		Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: "OrgProbe"}, TaskQueue: "org-q"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	backdate(t, org, "acme", "org-fire", 5*time.Minute)

	if err := root.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}
	if got := countByType(t, org, "acme", "OrgProbe"); got != 1 {
		t.Fatalf("org-shard OrgProbe executions = %d, want 1", got)
	}
	// The run must NOT leak into the root shard.
	if got := countByType(t, root, "acme", "OrgProbe"); got != 0 {
		t.Fatalf("root-shard OrgProbe executions = %d, want 0", got)
	}
}

// TestScheduleSweepClampsBacklog proves a schedule that missed many ticks
// fires exactly ONCE per sweep (re-anchored at now) instead of storming
// its backlog — the recovery property that makes healing a long-dead
// sweeper safe in production.
func TestScheduleSweepClampsBacklog(t *testing.T) {
	en := newEngine(newStore())
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	if err := en.CreateSchedule(Schedule{
		ScheduleId: "backlog",
		Namespace:  "default",
		Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: "BacklogProbe"}, TaskQueue: "bl-q"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	backdate(t, en, "default", "backlog", 24*time.Hour) // ~1440 missed ticks

	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}
	if got := countByType(t, en, "default", "BacklogProbe"); got != 1 {
		t.Fatalf("executions after 24h backlog = %d, want exactly 1", got)
	}
}

// TestIntervalScheduleFires proves interval specs (what base's durable
// client writes) fire — nextScheduleFire must honor Spec.Interval, not
// only CronString.
func TestIntervalScheduleFires(t *testing.T) {
	en := newEngine(newStore())
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	s := Schedule{
		ScheduleId: "ivl",
		Namespace:  "default",
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: "IntervalProbe"}, TaskQueue: "ivl-q"},
	}
	s.Spec.Interval = append(s.Spec.Interval, struct {
		Interval string `json:"interval"`
		Phase    string `json:"phase,omitempty"`
	}{Interval: "1m"})
	if err := en.CreateSchedule(s); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	backdate(t, en, "default", "ivl", 10*time.Minute)

	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}
	if got := countByType(t, en, "default", "IntervalProbe"); got != 1 {
		t.Fatalf("IntervalProbe executions = %d, want 1", got)
	}
}

// TestScheduleSweepSkipsBrokenEntry proves one broken schedule (its
// namespace was never registered, so StartWorkflow refuses) cannot poison
// the sweep for everyone else.
func TestScheduleSweepSkipsBrokenEntry(t *testing.T) {
	en := newEngine(newStore())
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	// Broken: namespace "ghost" is never registered. The schedule id sorts
	// BEFORE the healthy one to prove iteration continues past the failure.
	for _, sc := range []Schedule{
		{
			ScheduleId: "a-broken",
			Namespace:  "ghost",
			Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
			Action:     ScheduleAction{WorkflowType: TypeRef{Name: "GhostProbe"}, TaskQueue: "q"},
		},
		{
			ScheduleId: "b-healthy",
			Namespace:  "default",
			Spec:       ScheduleSpec{CronString: []string{"* * * * *"}},
			Action:     ScheduleAction{WorkflowType: TypeRef{Name: "HealthyProbe"}, TaskQueue: "q"},
		},
	} {
		if err := en.CreateSchedule(sc); err != nil {
			t.Fatalf("CreateSchedule(%s): %v", sc.ScheduleId, err)
		}
	}
	backdate(t, en, "ghost", "a-broken", 5*time.Minute)
	backdate(t, en, "default", "b-healthy", 5*time.Minute)

	if err := en.sweepSchedules(); err != nil {
		t.Fatalf("sweepSchedules: %v", err)
	}
	if got := countByType(t, en, "default", "HealthyProbe"); got != 1 {
		t.Fatalf("HealthyProbe executions = %d, want 1 (broken sibling must not poison sweep)", got)
	}
}

// TestViewScheduleCRUD exercises the exported in-process org view end to
// end: register ns, create, list, describe, trigger, delete.
func TestViewScheduleCRUD(t *testing.T) {
	en := newEngine(newStore())
	emb := &Embedded{engine: en}
	v := emb.View("hanzo")

	if err := v.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "hanzo"}}); err != nil {
		t.Fatalf("RegisterNamespace: %v", err)
	}
	if err := v.CreateSchedule(Schedule{
		ScheduleId: "cron-probe",
		Namespace:  "hanzo",
		Spec:       ScheduleSpec{CronString: []string{"0 4 * * *"}},
		Action:     ScheduleAction{WorkflowType: TypeRef{Name: "ViewProbe"}, TaskQueue: "view-q"},
	}); err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	list, err := v.ListSchedules("hanzo")
	if err != nil || len(list) != 1 || list[0].ScheduleId != "cron-probe" {
		t.Fatalf("ListSchedules = %+v, err=%v; want the one cron-probe", list, err)
	}
	if _, ok, err := v.DescribeSchedule("hanzo", "cron-probe"); !ok || err != nil {
		t.Fatalf("DescribeSchedule: ok=%v err=%v", ok, err)
	}
	wf, err := v.TriggerSchedule("hanzo", "cron-probe", "req-1")
	if err != nil || wf == nil || !strings.HasPrefix(wf.Type.Name, "ViewProbe") {
		t.Fatalf("TriggerSchedule: wf=%+v err=%v", wf, err)
	}
	if err := v.DeleteSchedule("hanzo", "cron-probe"); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if list, _ := v.ListSchedules("hanzo"); len(list) != 0 {
		t.Fatalf("after delete, %d schedules remain", len(list))
	}
}
