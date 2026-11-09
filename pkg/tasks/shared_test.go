// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/tasks/internal/dbtest"
)

// Two engines on one database are two replicas of ONE engine. Each test here holds
// two of them, each with a handle (a pool) of its own on the same SQLite file, as two
// pods each have a pool onto one hanzo-sql, and asks what an engine that was alone
// would have got by being the only one.

// pod is one replica: its engine, and the worker it has connected, whose deliveries
// are captured.
type pod struct {
	en  *engine
	cap *captureSend
	m   *replica
}

func openShared(t *testing.T, open dbtest.Open) *store {
	t.Helper()
	st, err := newSharedStore(context.Background(), open())
	if err != nil {
		t.Fatalf("shared store: %v", err)
	}
	return st
}

func newPod(t *testing.T, open dbtest.Open, name string) *pod {
	t.Helper()
	st := openShared(t, open)
	en := newEngine(st)
	m, err := joinCluster(context.Background(), st.shared, name, slog.Default(), defaultTiming)
	if err != nil {
		t.Fatalf("join %s: %v", name, err)
	}
	t.Cleanup(m.halt)
	en.cl = m
	cap := &captureSend{}
	en.disp.send = cap.fn
	for peer, kind := range map[string]taskKind{name + "-wf": kindWorkflow, name + "-act": kindActivity} {
		if _, err := en.disp.Subscribe(peer, "default", "tq", kind); err != nil {
			t.Fatalf("subscribe %s: %v", peer, err)
		}
	}
	return &pod{en: en, cap: cap, m: m}
}

func podPair(t *testing.T, open dbtest.Open) (*pod, *pod) {
	t.Helper()
	a, b := newPod(t, open, "a"), newPod(t, open, "b")
	regDefaultNS(t, a.en)
	return a, b
}

// die ends p the way a killed process ends: nothing it owed is answered, and its
// membership is over.
func (p *pod) die(t *testing.T) {
	t.Helper()
	p.m.halt()
	if err := p.m.sh.Expire(context.Background(), memberPrefix+p.m.id, p.m.id); err != nil {
		t.Fatal(err)
	}
}

func deliveries(ps ...*pod) (wf, act int) {
	for _, p := range ps {
		wf += countDeliveries(p.cap, OpcodeDeliverWorkflowTask)
		act += countDeliveries(p.cap, OpcodeDeliverActivityTask)
	}
	return
}

// The same schedule's tick, swept by two replicas at once, is one run.
func TestShared_ScheduleFiresOnce(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkScheduleFiresOnce(t, open)
	})
}

func checkScheduleFiresOnce(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	const schedules = 12
	for i := 0; i < schedules; i++ {
		id := fmt.Sprintf("nightly-%d", i)
		if err := a.en.CreateSchedule(Schedule{
			ScheduleId: id, Namespace: "default",
			Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
			Action: ScheduleAction{WorkflowType: TypeRef{Name: "Backup"}, TaskQueue: "tq"},
		}); err != nil {
			t.Fatal(err)
		}
		// Due: it was last anchored two hours ago.
		if err := a.en.editSchedule("default", id, func(s *Schedule) {
			s.Info.UpdateTime = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
		}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, p := range []*pod{a, b, b, a} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.en.sweepSchedules(); err != nil {
				t.Errorf("sweep: %v", err)
			}
		}()
	}
	wg.Wait()

	runs, err := a.en.ListWorkflows("default")
	if err != nil {
		t.Fatal(err)
	}
	perSchedule := map[string]int{}
	for _, r := range runs {
		id, _ := r.SearchAttrs[searchAttrScheduleID].(string)
		perSchedule[id]++
	}
	for i := 0; i < schedules; i++ {
		id := fmt.Sprintf("nightly-%d", i)
		if perSchedule[id] != 1 {
			t.Errorf("schedule %s fired %d times for one tick across two replicas, want 1", id, perSchedule[id])
		}
		s, _, _ := b.en.DescribeSchedule("default", id)
		if s.Info.ActionCount != 1 {
			t.Errorf("schedule %s counts %d actions, want 1", id, s.Info.ActionCount)
		}
	}
	if wf, _ := deliveries(a, b); wf != schedules {
		t.Errorf("workflow tasks handed out = %d, want one per run (%d)", wf, schedules)
	}
}

// One replica sweeps among several: the leadership is held by one at a time, and
// goes to another when its holder stops asking.
func TestShared_OneReplicaLeadsTheSweep(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkOneReplicaLeadsTheSweep(t, open)
	})
}

func checkOneReplicaLeadsTheSweep(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	a.m.t.lead, b.m.t.lead = time.Second, time.Second
	if !a.en.cl.lead("sweep") {
		t.Fatal("the first to ask leads")
	}
	if b.en.cl.lead("sweep") {
		t.Fatal("two replicas lead one sweep")
	}
	if !a.en.cl.lead("sweep") {
		t.Fatal("the leader keeps it by asking")
	}
	time.Sleep(2200 * time.Millisecond)
	if !b.en.cl.lead("sweep") {
		t.Fatal("the leader stopped asking: another takes the sweep")
	}
	if a.en.cl.lead("sweep") {
		t.Fatal("the old leader asks and finds it taken")
	}
}

// A run's lock is held across replicas: two critical sections on one key never
// overlap, whichever replica enters.
func TestShared_RunLockExcludesAcrossReplicas(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkRunLockExcludesAcrossReplicas(t, open)
	})
}

func checkRunLockExcludesAcrossReplicas(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	var inside, overlaps, entered atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		en := []*engine{a.en, b.en}[i%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := en.lockRun("default", "wf", "run")
			if inside.Add(1) > 1 {
				overlaps.Add(1)
			}
			entered.Add(1)
			time.Sleep(2 * time.Millisecond)
			inside.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if entered.Load() != 24 || overlaps.Load() != 0 {
		t.Fatalf("entered %d, overlapped %d: two replicas held one run", entered.Load(), overlaps.Load())
	}
}

// A lock whose holder died does not hold for ever.
func TestShared_LockOfADeadReplicaExpires(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkLockOfADeadReplicaExpires(t, open)
	})
}

func checkLockOfADeadReplicaExpires(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	a.m.t.lock, b.m.t.lock = time.Second, time.Second
	_ = a.en.lockRun("default", "wf", "run") // never released: its holder is gone
	a.die(t)
	got := make(chan struct{})
	go func() {
		defer close(got)
		b.en.lockRun("default", "wf", "run")()
	}()
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the lock of a dead replica still holds")
	}
}

// Two replicas that start the same (workflowId, requestId) at once get one run.
func TestShared_StartIsIdempotentAcrossReplicas(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkStartIsIdempotentAcrossReplicas(t, open)
	})
}

func checkStartIsIdempotentAcrossReplicas(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	for i := 0; i < 10; i++ {
		id, req := fmt.Sprintf("wf-%d", i), fmt.Sprintf("req-%d", i)
		var wg sync.WaitGroup
		runs := make([]string, 4)
		for j, p := range []*pod{a, b, a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				wf, err := p.en.StartWorkflowWithRequestID("default", id, "", TypeRef{Name: "W"}, "tq", nil, req)
				if err != nil {
					t.Errorf("start: %v", err)
					return
				}
				runs[j] = wf.Execution.RunId
			}()
		}
		wg.Wait()
		for _, r := range runs {
			if r != runs[0] {
				t.Fatalf("%s: replicas started different runs for one request: %v", id, runs)
			}
		}
	}
	if wf, _ := deliveries(a, b); wf != 10 {
		t.Fatalf("workflow tasks handed out = %d, want 10", wf)
	}
}

// startWithActivity starts a run on p and schedules activity seq 0 for it.
func startWithActivity(t *testing.T, p *pod, id string) string {
	t.Helper()
	wf, err := p.en.StartWorkflow("default", id, "", TypeRef{Name: "W"}, "tq", []any{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	run := wf.Execution.RunId
	unlock := p.en.lockRun("default", id, run)
	defer unlock()
	if err := p.en.applyScheduleActivity("default", id, run, 0, "Act", []byte(`[]`), "tq", 0, 0, nil); err != nil {
		t.Fatalf("schedule: %v", err)
	}
	return run
}

// Work in flight on a live replica is left to it, whoever looks; and when it dies,
// exactly one replica takes it up, once, however many look at once.
func TestShared_ActivityIsDispatchedOnce(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkActivityIsDispatchedOnce(t, open)
	})
}

func checkActivityIsDispatchedOnce(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	run := startWithActivity(t, a, "wf-1")
	if _, act := deliveries(a, b); act != 1 {
		t.Fatalf("the activity was handed out %d times, want 1", act)
	}

	// Both replicas look at the run while its owner is alive, at the same time.
	var wg sync.WaitGroup
	for _, p := range []*pod{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.en.Recover(); err != nil {
				t.Errorf("recover: %v", err)
			}
		}()
	}
	wg.Wait()
	if wf, act := deliveries(b); wf != 0 || act != 0 {
		t.Fatalf("b took up work a is alive to answer: %d workflow tasks, %d activities", wf, act)
	}
	if _, act := deliveries(a); act != 1 {
		t.Fatalf("a was handed its own activity again: %d", act)
	}

	// a dies mid-activity. b looks, from three goroutines at once.
	a.die(t)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.en.Recover(); err != nil {
				t.Errorf("recover: %v", err)
			}
		}()
	}
	wg.Wait()
	if _, act := deliveries(b); act != 1 {
		t.Fatalf("b handed the dead replica's activity out %d times, want exactly 1", act)
	}
	rec, _, _ := b.en.getWorkflowActivity("default", "wf-1", run, 0)
	if rec.Owner != b.m.id || rec.Epoch != 2 {
		t.Fatalf("activity is owned by %q at epoch %d, want b at epoch 2", rec.Owner, rec.Epoch)
	}

	// a, which was not dead after all, answers the hand-out it had: refused, and the
	// activity is no further on. A failure of it must not count as an attempt.
	failure := rbacDenied(t)
	if err := b.en.completeWorkflowActivity("default", "wf-1", run, 0, 1, nil, failure); err != nil {
		t.Fatal(err)
	}
	if err := b.en.completeWorkflowActivity("default", "wf-1", run, 0, 1, []byte(`"stale"`), nil); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = b.en.getWorkflowActivity("default", "wf-1", run, 0)
	if rec.isTerminal() || rec.Attempt != 1 {
		t.Fatalf("a stale answer moved the activity: %+v", rec)
	}
	if got := countEventType(t, b.en, "wf-1", run, evtActivityCompleted); got != 0 {
		t.Fatalf("a stale answer completed the activity: %d", got)
	}

	// b's own answer stands, and the run advances once.
	if err := b.en.completeWorkflowActivity("default", "wf-1", run, 0, 2, []byte(`"done"`), nil); err != nil {
		t.Fatal(err)
	}
	if got := countEventType(t, b.en, "wf-1", run, evtActivityCompleted); got != 1 {
		t.Fatalf("ACTIVITY_TASK_COMPLETED events = %d, want 1", got)
	}
}

// A decision handed out again supersedes the one before it; the answer of the earlier
// one, from a replica that was declared dead and was not, is not applied.
func TestShared_DecisionsAreFencedByEpoch(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkDecisionsAreFencedByEpoch(t, open)
	})
}

func checkDecisionsAreFencedByEpoch(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	wf, err := a.en.StartWorkflow("default", "wf-1", "", TypeRef{Name: "W"}, "tq", nil)
	if err != nil {
		t.Fatal(err)
	}
	run := wf.Execution.RunId
	claim, _, _ := a.en.getTask("default", "wf-1", run)
	if claim.Owner != a.m.id || claim.Epoch != 1 || a.en.superseded("default", "wf-1", run, 1) {
		t.Fatalf("a holds epoch 1: %+v", claim)
	}

	a.die(t)
	if err := b.en.Recover(); err != nil {
		t.Fatal(err)
	}
	claim, _, _ = b.en.getTask("default", "wf-1", run)
	if claim.Owner != b.m.id || claim.Epoch != 2 {
		t.Fatalf("b took the decision at epoch 2: %+v", claim)
	}
	if !b.en.superseded("default", "wf-1", run, 1) || b.en.superseded("default", "wf-1", run, 2) {
		t.Fatal("epoch 1 is superseded and epoch 2 is not")
	}
	if wf, _ := deliveries(b); wf != 1 {
		t.Fatalf("b handed out %d decisions for the run, want 1", wf)
	}

	// A second pass over a run b holds takes nothing more.
	if err := b.en.Recover(); err != nil {
		t.Fatal(err)
	}
	if wf, _ := deliveries(b); wf != 1 {
		t.Fatalf("a second look handed the decision out again: %d", wf)
	}
}

// Runs of an org are taken up too, not only the root tenant's, and so are rows that
// carry no owner at all: what was moved here from files.
func TestShared_RecoverTakesUpEveryTenantsRuns(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkRecoverTakesUpEveryTenantsRuns(t, open)
	})
}

func checkRecoverTakesUpEveryTenantsRuns(t *testing.T, open dbtest.Open) {
	a, b := podPair(t, open)
	acme := a.en.As(Org("acme"))
	if err := acme.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default"}}); err != nil {
		t.Fatal(err)
	}
	wf, err := acme.StartWorkflow("default", "wf-org", "", TypeRef{Name: "W"}, "tq", nil)
	if err != nil {
		t.Fatal(err)
	}
	// What a file leaves: no claim on the decision.
	if err := acme.store.del(taskKey("default", "wf-org", wf.Execution.RunId)); err != nil {
		t.Fatal(err)
	}
	before, _ := deliveries(b)
	if err := b.en.Recover(); err != nil {
		t.Fatal(err)
	}
	if after, _ := deliveries(b); after != before+1 {
		t.Fatalf("an org's run with no owner was handed out %d times, want 1", after-before)
	}
}

// A cancel that nobody answers is acked by whichever replica sweeps, not only by the
// one that took the cancel.
func TestShared_CancelIsSweptByAnyReplica(t *testing.T) {
	dbtest.Engines(t, func(t *testing.T, open dbtest.Open) {
		checkCancelIsSweptByAnyReplica(t, open)
	})
}

func checkCancelIsSweptByAnyReplica(t *testing.T, open dbtest.Open) {
	defer func(d time.Duration) { cancelTimeout = d }(cancelTimeout)
	a, b := podPair(t, open)
	wf, err := a.en.StartWorkflow("default", "wf-1", "", TypeRef{Name: "W"}, "tq", nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.en.CancelWorkflow("default", "wf-1", wf.Execution.RunId)
	if err != nil || got.Status != "WORKFLOW_EXECUTION_STATUS_CANCELING" {
		t.Fatalf("cancel with a worker subscribed waits for it: %v %v", got, err)
	}
	cancelTimeout = 0
	time.Sleep(1100 * time.Millisecond) // the mark is in whole seconds
	if err := b.en.sweepCanceling(); err != nil {
		t.Fatal(err)
	}
	done, _, _ := a.en.DescribeWorkflow("default", "wf-1", wf.Execution.RunId)
	if done.Status != "WORKFLOW_EXECUTION_STATUS_CANCELED" {
		t.Fatalf("a replica that did not take the cancel did not ack it: %s", done.Status)
	}
	if ok, _ := a.en.store.get(cancelKey("default", "wf-1", wf.Execution.RunId), new(cancelMark)); ok {
		t.Fatal("the cancel mark outlived the run")
	}
}
