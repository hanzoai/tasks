// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// Phase-2a: durable, event-sourced activity core.
//
// History is the source of truth. A workflow task is one decision
// episode; the worker is stateless between episodes and rebuilds state by
// replaying the run's history (see pkg/sdk/worker). This file owns the
// server side of that loop for the activity path:
//
//   - applyScheduleActivity  — idempotent, exactly-once schedule keyed by
//     (ns, run, seq); writes ACTIVITY_TASK_SCHEDULED and dispatches.
//   - completeWorkflowActivity — records the terminal (or retried) outcome
//     of an activity and schedules a new workflow task so the worker
//     replays with the longer history and advances.
//   - retry+backoff — server-side, per RetryPolicy, on the same (run, seq).
//   - Recover — crash-recovery that re-dispatches the in-flight activities and
//     re-enqueues the workflow tasks that no live engine owns. It runs when an
//     engine starts and whenever another engine's membership lapses.
//
// Advancing a single run is serialized by a per-run lock (lockRun), held by
// every engine that shares the rows, so the read-check-then-write of a
// schedule/complete is atomic above the store's single writer.
//
// Every piece of work in flight is owned by the engine that handed it to a worker,
// and carries an epoch that rises each time it is handed out. The owner is how an
// engine that dies is told from one that is merely busy (Recover takes up what a
// dead engine owned); the epoch is how a worker's answer from before the work
// changed hands is told from the answer to the hand-out that stands.

// ── workflow-history activity lifecycle event types ─────────────────────
const (
	evtActivityScheduled = "ACTIVITY_TASK_SCHEDULED"
	evtActivityStarted   = "ACTIVITY_TASK_STARTED"
	evtActivityCompleted = "ACTIVITY_TASK_COMPLETED"
	evtActivityFailed    = "ACTIVITY_TASK_FAILED"
)

// ── seq-keyed activity record statuses ──────────────────────────────────
const (
	wfActScheduled = "SCHEDULED"
	wfActStarted   = "STARTED"
	wfActCompleted = "COMPLETED"
	wfActFailed    = "FAILED"
)

// defaultMaxActivityAttempts bounds server-side activity retries when the
// caller's RetryPolicy leaves MaximumAttempts unset (0). Unlimited retry
// is deliberately NOT honored server-side: a permanently failing activity
// must terminate. Callers wanting a different bound set MaximumAttempts
// explicitly (1 disables retries).
const defaultMaxActivityAttempts = 10

// workflowActivity is the durable, seq-keyed record for one activity of a
// workflow run. It is the exactly-once anchor: a given (ns, run, seq) has
// exactly one record, and the user activity function completes at most
// once because a terminal record blocks any further dispatch.
type workflowActivity struct {
	Seq            int                     `json:"seq"`
	ActivityID     string                  `json:"activityId"`
	ActivityType   string                  `json:"activityType"`
	Input          json.RawMessage         `json:"input,omitempty"`
	TaskQueue      string                  `json:"taskQueue,omitempty"`
	Status         string                  `json:"status"`
	Attempt        int                     `json:"attempt"`
	Result         json.RawMessage         `json:"result,omitempty"`
	Failure        json.RawMessage         `json:"failure,omitempty"`
	StartToCloseMs int64                   `json:"startToCloseMs,omitempty"`
	HeartbeatMs    int64                   `json:"heartbeatMs,omitempty"`
	RetryPolicy    *client.RetryPolicyJSON `json:"retryPolicy,omitempty"`
	// Owner is the engine holding this activity's task: the one that handed it to a
	// worker, or that holds the retry's backoff. Epoch rises each time the task is
	// handed out, so an answer carrying an older one is not the stand's.
	Owner string `json:"owner,omitempty"`
	Epoch int64  `json:"epoch,omitempty"`
}

func (a *workflowActivity) isTerminal() bool {
	return a.Status == wfActCompleted || a.Status == wfActFailed
}

func wfActKey(ns, wf, run string, seq int) string {
	return fmt.Sprintf("wfact/%s/%s/%s/%010d", ns, wf, run, seq)
}

func wfActPrefix(ns, wf, run string) string {
	return fmt.Sprintf("wfact/%s/%s/%s/", ns, wf, run)
}

// wfActivityID is the deterministic activity id for (run, seq) so a
// re-dispatch (replay / recovery / retry) always refers to the same
// logical activity.
func wfActivityID(run string, seq int) string {
	return run + "-act-" + strconv.Itoa(seq)
}

// ── per-run serialization ───────────────────────────────────────────────

// keyedMutex serializes operations per string key. One *sync.Mutex per key,
// created on first use. Keys are (org|ns|wf|run) so advancing distinct runs
// is fully parallel while a single run is single-threaded.
type keyedMutex struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func newKeyedMutex() *keyedMutex { return &keyedMutex{m: map[string]*sync.Mutex{}} }

func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	m, ok := k.m[key]
	if !ok {
		m = &sync.Mutex{}
		k.m[key] = m
	}
	k.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// lockRun returns an unlock func serializing all advance operations for a
// single run, across every engine that shares the rows. Entry points (the
// public mutators, respondWorkflowHandler, completeWorkflowActivity, recoverRun,
// the retry timer) acquire it; internal helpers (applyScheduleActivity,
// dispatchActivityAttempt, scheduleWorkflowTask, terminalTransition,
// appendHistory) assume the caller holds it.
func (e *engine) lockRun(ns, wf, run string) func() {
	return e.cl.lock(e.principal.String() + "|" + ns + "|" + wf + "|" + run)
}

// ── activity record I/O ─────────────────────────────────────────────────

func (e *engine) getWorkflowActivity(ns, wf, run string, seq int) (*workflowActivity, bool, error) {
	var a workflowActivity
	ok, err := e.store.get(wfActKey(ns, wf, run, seq), &a)
	if !ok {
		return nil, false, err
	}
	return &a, true, err
}

func (e *engine) putWorkflowActivity(ns, wf, run string, a *workflowActivity) error {
	return e.store.put(wfActKey(ns, wf, run, a.Seq), a)
}

func (e *engine) listWorkflowActivities(ns, wf, run string) ([]workflowActivity, error) {
	return listInto[workflowActivity](e.store, wfActPrefix(ns, wf, run))
}

// ── workflow-task delivery (full history) ───────────────────────────────

// workflowHistoryBytes returns the run's full history as a JSON array of
// HistoryEvent — the event-sourced payload the worker replays.
func (e *engine) workflowHistoryBytes(ns, wf, run string) ([]byte, error) {
	events, err := listInto[HistoryEvent](e.store, fmt.Sprintf("wfh/%s/%s/%s/", ns, wf, run))
	if err != nil {
		return nil, err
	}
	return json.Marshal(events)
}

// wfTask is the claim on a run's next decision, one row per open run
// (wftask/<ns>/<workflowId>/<runId>).
//
// A decision is owed whenever something happens to a run that the workflow has
// not yet seen: it started, an activity finished, a signal came. Owner is the
// engine that handed the decision to a worker, and Epoch rises each time one is
// handed out. A worker's answer is applied only while its epoch is the newest: an
// older one was computed from a shorter history, by a worker whose engine may have
// been declared dead and its work given to another, and the newer decision will
// say what it would have. Idle says nothing is owed: the last decision was applied,
// and the run waits for an event that will make one.
type wfTask struct {
	Owner string `json:"owner,omitempty"`
	Epoch int64  `json:"epoch"`
	Idle  bool   `json:"idle,omitempty"`
}

func taskKey(ns, wf, run string) string { return "wftask/" + ns + "/" + wf + "/" + run }

func (e *engine) getTask(ns, wf, run string) (wfTask, bool, error) {
	var t wfTask
	ok, err := e.store.get(taskKey(ns, wf, run), &t)
	return t, ok, err
}

// superseded reports whether the run's decision has been handed out again since
// the one numbered epoch, which makes that one's answer stale.
func (e *engine) superseded(ns, wf, run string, epoch int64) bool {
	claim, held, _ := e.getTask(ns, wf, run)
	return held && epoch < claim.Epoch
}

// scheduleWorkflowTask claims the run's next decision for this engine and enqueues
// a workflow task carrying the run's full history so the stateless worker replays
// and advances. No-op if the run is already terminal. The caller holds the run's
// lock. The claim is written before the task is handed over, so an engine that
// dies between the two leaves a claim a live engine will find and take.
func (e *engine) scheduleWorkflowTask(ns, wf, run string) error {
	wfe, ok, err := e.DescribeWorkflow(ns, wf, run)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("workflow %s/%s/%s not found", ns, wf, run)
	}
	if isTerminal(wfe.Status) {
		return nil
	}
	cur, _, err := e.getTask(ns, wf, run)
	if err != nil {
		return err
	}
	claim := wfTask{Owner: e.cl.self(), Epoch: cur.Epoch + 1}
	if err := e.store.put(taskKey(ns, wf, run), claim); err != nil {
		return err
	}
	hist, err := e.workflowHistoryBytes(ns, wf, run)
	if err != nil {
		return err
	}
	if e.disp != nil {
		e.disp.EnqueueWorkflowTask(e.principal, ns, wfe.TaskQueue, wf, run, wfe.Type.Name, claim.Epoch, hist)
	}
	return nil
}

// ── schedule (exactly-once) ─────────────────────────────────────────────

// applyScheduleActivity idempotently creates and dispatches the activity
// for (ns, run, seq). A second call for the same (run, seq) — via replay OR
// crash-recovery — is a no-op: the existing record stands and the user
// activity function is never dispatched twice. Caller holds the run lock.
func (e *engine) applyScheduleActivity(ns, wf, run string, seq int, actType string, input []byte, taskQueue string, startToCloseMs, heartbeatMs int64, rp *client.RetryPolicyJSON) error {
	if _, ok, err := e.getWorkflowActivity(ns, wf, run, seq); err != nil {
		return err
	} else if ok {
		return nil // exactly-once: already scheduled
	}
	if taskQueue == "" {
		if wfe, ok, _ := e.DescribeWorkflow(ns, wf, run); ok {
			taskQueue = wfe.TaskQueue
		}
	}
	rec := &workflowActivity{
		Seq:            seq,
		ActivityID:     wfActivityID(run, seq),
		ActivityType:   actType,
		Input:          append(json.RawMessage(nil), input...),
		TaskQueue:      taskQueue,
		Status:         wfActScheduled,
		Owner:          e.cl.self(),
		Attempt:        1,
		StartToCloseMs: startToCloseMs,
		HeartbeatMs:    heartbeatMs,
		RetryPolicy:    rp,
	}
	if err := e.putWorkflowActivity(ns, wf, run, rec); err != nil {
		return err
	}
	if _, err := e.appendHistory(ns, wf, run, evtActivityScheduled, map[string]any{
		"seq":          seq,
		"activityId":   rec.ActivityID,
		"activityType": actType,
		"input":        json.RawMessage(rec.Input),
		"taskQueue":    taskQueue,
		"retryPolicy":  rp,
		"attempt":      1,
	}); err != nil {
		return err
	}
	e.emit(Event{Kind: "activity.scheduled", Namespace: ns, WorkflowID: wf, RunID: run, Data: map[string]any{"seq": seq, "activityType": actType}})
	return e.dispatchActivityAttempt(ns, wf, run, rec, true)
}

// dispatchActivityAttempt hands rec's activity task to a subscribed
// activity worker (or queues it) and, when appendStarted, records an
// ACTIVITY_TASK_STARTED marker for the current attempt. The dispatcher
// mints a fresh token bound to (ns, wf, run, seq) so the worker's Respond
// resolves back to this record. Caller holds the run lock.
func (e *engine) dispatchActivityAttempt(ns, wf, run string, rec *workflowActivity, appendStarted bool) error {
	rec.Status = wfActStarted
	rec.Owner = e.cl.self()
	rec.Epoch++
	if err := e.putWorkflowActivity(ns, wf, run, rec); err != nil {
		return err
	}
	if appendStarted {
		if _, err := e.appendHistory(ns, wf, run, evtActivityStarted, map[string]any{
			"seq":        rec.Seq,
			"activityId": rec.ActivityID,
			"attempt":    rec.Attempt,
		}); err != nil {
			return err
		}
	}
	if e.disp != nil {
		e.disp.DispatchActivity(e.principal, ns, rec.TaskQueue, wf, run, rec.Seq, rec.Epoch, rec.ActivityID, rec.ActivityType, rec.Input, rec.StartToCloseMs, rec.HeartbeatMs)
	}
	return nil
}

// ── completion (terminal / retry) ───────────────────────────────────────

// completeWorkflowActivity records the terminal (or retried) outcome of the
// activity dispatched for (ns, run, seq). On a terminal event it schedules
// a new workflow task so the worker replays with the longer history and
// advances. On a retryable failure with attempts remaining it re-dispatches
// the SAME (run, seq) after backoff without advancing the workflow.
// Idempotent against an already-terminal record (a duplicate Respond is
// ignored), and against an answer from an earlier hand-out of the task than the
// one that stands (epoch; 0 is a caller that holds no token). Acquires the run lock.
func (e *engine) completeWorkflowActivity(ns, wf, run string, seq int, epoch int64, result, failure []byte) error {
	unlock := e.lockRun(ns, wf, run)
	defer unlock()

	rec, ok, err := e.getWorkflowActivity(ns, wf, run, seq)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("activity (run=%s seq=%d) not found", run, seq)
	}
	if rec.isTerminal() {
		return nil // duplicate / already resolved
	}
	if epoch != 0 && epoch != rec.Epoch {
		// Answered by a worker whose engine no longer holds this task: it was
		// declared dead and the task handed to another (or handed out again after a
		// retry), and the hand-out that stands will answer for itself. Counting this
		// failure as an attempt would run the activity once more than its policy.
		return nil
	}
	// One read serves two jobs: the terminal guard below, and the failure
	// identity — the report has to name the SHAPE of the work (workflow type,
	// task queue, originating schedule), because a runId names nothing an
	// operator can act on and is freshly minted on every cron fire anyway.
	wfe, wfOK, _ := e.DescribeWorkflow(ns, wf, run)
	// Never advance a workflow that already reached a terminal state.
	if wfOK && isTerminal(wfe.Status) {
		return nil
	}

	if len(failure) == 0 {
		rec.Status = wfActCompleted
		rec.Result = append(json.RawMessage(nil), result...)
		if err := e.putWorkflowActivity(ns, wf, run, rec); err != nil {
			return err
		}
		if _, err := e.appendHistory(ns, wf, run, evtActivityCompleted, map[string]any{
			"seq":        seq,
			"activityId": rec.ActivityID,
			"result":     json.RawMessage(rec.Result),
		}); err != nil {
			return err
		}
		e.emit(Event{Kind: "activity.completed", Namespace: ns, WorkflowID: wf, RunID: run, Data: map[string]any{"seq": seq}})
		// One success ends the streak: "failed every attempt" is the alertable
		// condition, so anything that succeeds is by definition not it.
		e.clearFailure(activityFailureIdentity(wfe, ns, wf, run, rec))
		return e.scheduleWorkflowTask(ns, wf, run)
	}

	// Failure — retry per policy, else terminal.
	//
	// Both branches report. The emit() below reaches an SSE subscriber if one
	// happens to be attached and is dropped on the floor otherwise; that was
	// the ONLY output this path had while cloud's RunJobActivity failed on
	// every fire for 11 days. recordFailure writes a durable row and a
	// throttled line instead. Retry semantics are untouched — which branch is
	// taken, when, and how many times is decided exactly as before.
	failID := activityFailureIdentity(wfe, ns, wf, run, rec)
	terr := temporal.Decode(failure)
	pol := effectiveRetryPolicy(rec.RetryPolicy)
	if pol.ShouldRetry(terr, int32(rec.Attempt)) {
		delay := pol.NextInterval(int32(rec.Attempt))
		// retrying=true: "failed once, will try again" — the case that must
		// NOT read like the 11-day outage.
		e.recordFailure(failID, true, terr.Error())
		rec.Attempt++
		rec.Status = wfActScheduled
		rec.Owner = e.cl.self()                                // this engine holds the backoff
		rec.Failure = append(json.RawMessage(nil), failure...) // last failure (observability)
		if err := e.putWorkflowActivity(ns, wf, run, rec); err != nil {
			return err
		}
		e.emit(Event{Kind: "activity.retrying", Namespace: ns, WorkflowID: wf, RunID: run, Data: map[string]any{"seq": seq, "attempt": rec.Attempt, "delayMs": delay.Milliseconds()}})
		e.scheduleActivityRetry(ns, wf, run, seq, delay)
		return nil
	}

	rec.Status = wfActFailed
	rec.Failure = append(json.RawMessage(nil), failure...)
	if err := e.putWorkflowActivity(ns, wf, run, rec); err != nil {
		return err
	}
	if _, err := e.appendHistory(ns, wf, run, evtActivityFailed, map[string]any{
		"seq":        seq,
		"activityId": rec.ActivityID,
		"failure":    json.RawMessage(rec.Failure),
	}); err != nil {
		return err
	}
	e.emit(Event{Kind: "activity.failed", Namespace: ns, WorkflowID: wf, RunID: run, Data: map[string]any{"seq": seq, "attempts": rec.Attempt}})
	// retrying=false: this run's activity is dead. In the incident every fire
	// ended here, 4489 times, in silence.
	e.recordFailure(failID, false, terr.Error())
	return e.scheduleWorkflowTask(ns, wf, run)
}

// scheduleActivityRetry re-dispatches (ns, run, seq) after delay. The timer
// is in-memory; the record names this engine as its owner, so a crash before it
// fires is covered by Recover, which re-dispatches any non-terminal activity whose
// owner is gone (losing the residual backoff, which is acceptable — durable timers
// are Phase-2b). A timer that finds the record someone else's, because that engine
// took it up while this one was thought dead, leaves it to them.
func (e *engine) scheduleActivityRetry(ns, wf, run string, seq int, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay, func() {
		unlock := e.lockRun(ns, wf, run)
		defer unlock()
		rec, ok, _ := e.getWorkflowActivity(ns, wf, run, seq)
		if !ok || rec.isTerminal() || rec.Status != wfActScheduled || rec.Owner != e.cl.self() {
			return
		}
		_ = e.dispatchActivityAttempt(ns, wf, run, rec, true)
	})
}

// effectiveRetryPolicy converts the stored wire retry policy to the
// normalized temporal.RetryPolicy used for the server-side retry decision,
// applying the finite default attempt bound.
func effectiveRetryPolicy(rp *client.RetryPolicyJSON) *temporal.RetryPolicy {
	pol := &temporal.RetryPolicy{}
	if rp != nil {
		pol.InitialInterval = time.Duration(rp.InitialIntervalMs) * time.Millisecond
		pol.BackoffCoefficient = rp.BackoffCoefficient
		pol.MaximumInterval = time.Duration(rp.MaximumIntervalMs) * time.Millisecond
		pol.MaximumAttempts = rp.MaximumAttempts
		pol.NonRetryableErrorTypes = rp.NonRetryableErrorTypes
	}
	eff := pol.Normalize()
	if eff.MaximumAttempts <= 0 {
		eff.MaximumAttempts = defaultMaxActivityAttempts
	}
	return eff
}

// ── crash-recovery (single replica) ─────────────────────────────────────

// The open-run index. wfopen/<ns>/<workflowId>/<runId> holds a workflow run
// that has not reached a terminal state, and Recover reads it instead of the
// run rows, so a restart reads the runs it has to drive again and none of the
// ones that finished. wfidx/<ns> marks a shard whose index is complete; a
// shard written before the index existed is walked once to build it
// (indexOpenRuns).
//
// A shard can be sealed, or a process stopped, between any two writes, so
// the entry is put BEFORE a run's first row and deleted AFTER its terminal
// row. Stopping in between leaves an entry with no row, or one whose row is
// terminal, and recovery drops either when it reads it; it never leaves an
// open row the index misses.

// indexPage is how many rows indexOpenRuns reads from the shard at a time.
const indexPage = 1024

func runKey(ns, wf, run string) string  { return "wf/" + ns + "/" + wf + "/" + run }
func openKey(ns, wf, run string) string { return "wfopen/" + ns + "/" + wf + "/" + run }
func openPrefix(ns string) string       { return "wfopen/" + ns + "/" }
func openIndexKey(ns string) string     { return "wfidx/" + ns }

// openRun indexes a workflow run as open. It is written ahead of the run's
// first row.
func (e *engine) openRun(ns string, ref ExecutionRef) error {
	return e.store.put(openKey(ns, ref.WorkflowId, ref.RunId), ref)
}

// closeRun writes wf's terminal row, then drops the run from the open index.
func (e *engine) closeRun(ns string, wf *WorkflowExecution) error {
	wfID, run := wf.Execution.WorkflowId, wf.Execution.RunId
	if err := e.store.put(runKey(ns, wfID, run), wf); err != nil {
		return err
	}
	// A closed run owes no decision and has no cancel to sweep.
	for _, key := range []string{taskKey(ns, wfID, run), cancelKey(ns, wfID, run)} {
		if err := e.store.del(key); err != nil {
			return err
		}
	}
	return e.store.del(openKey(ns, wfID, run))
}

// runRow is the part of a run's row that says whether and when it closed.
// Workflow and standalone-activity rows share these field names, and decoding
// only these skips building the input, result and memo a full decode would.
type runRow struct {
	Execution ExecutionRef `json:"execution"`
	Status    string       `json:"status"`
	StartTime string       `json:"startTime"`
	CloseTime string       `json:"closeTime"`
}

// page reads up to limit rows under prefix after the key `after`, and
// returns them with the last key read and how many rows it read. A row that
// does not decode counts as read and is otherwise skipped.
func (e *engine) page(prefix, after string, limit int) ([]runRow, string, int, error) {
	var rows []runRow
	n := 0
	err := e.store.scan(prefix, after, limit, func(key string, body []byte) error {
		n++
		after = key
		var r runRow
		if json.Unmarshal(body, &r) == nil {
			rows = append(rows, r)
		}
		return nil
	})
	return rows, after, n, err
}

// indexOpenRuns builds ns's open-run index from its workflow rows, unless
// wfidx/<ns> says it is built. That is one walk over the shard's workflow
// rows, ever, a page at a time so the shard is free between pages. Every
// write it makes is idempotent, so a walk cut short is walked again.
func (e *engine) indexOpenRuns(ns string) error {
	var built string
	if ok, err := e.store.get(openIndexKey(ns), &built); err != nil || ok {
		return err
	}
	after := ""
	for {
		rows, last, n, err := e.page("wf/"+ns+"/", after, indexPage)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if isTerminal(r.Status) {
				continue
			}
			if err := e.openRun(ns, r.Execution); err != nil {
				return err
			}
		}
		if n < indexPage {
			break
		}
		after = last
	}
	return e.store.put(openIndexKey(ns), nowRFC3339())
}

// Recover takes up the work that no live engine owns: after a restart, when the
// engine that handed it to a worker is gone, or when the rows were moved here from
// files and carry no owner at all. For every open run it re-dispatches the
// activities that are scheduled or started but not terminal and whose owner is
// not alive (idempotent per (run, seq)), and re-enqueues a workflow task when the
// run's decision is owed by an engine that is not alive. Work a live engine owns
// is left to it, so an engine that starts beside running peers (a rolling update)
// takes nothing from them.
//
// Because dispatch is seq-keyed and the decider dedups on replay, taking up work
// causes neither lost nor double execution: an already-terminal activity is
// skipped (never re-run), a not-yet-terminal one is re-driven, and a worker's
// answer to the hand-out it replaces is refused by its epoch.
//
// It finds those workflows through the open-run index above, so it reads the runs
// still open and none of the ones that finished: its cost is set by how much is in
// flight, not by how much history the store holds. It covers every tenant.
func (e *engine) Recover() error {
	alive, err := e.aliveFn()
	if err != nil {
		return err
	}
	tenants, err := e.store.tenants()
	if err != nil {
		return err
	}
	for _, p := range tenants {
		oe := e.As(p)
		namespaces, err := oe.ListNamespaces()
		if err != nil {
			return err
		}
		for i := range namespaces {
			ns := namespaces[i].NamespaceInfo.Name
			if err := oe.indexOpenRuns(ns); err != nil {
				return err
			}
			open, err := listInto[ExecutionRef](oe.store, openPrefix(ns))
			if err != nil {
				return err
			}
			for _, ref := range open {
				later, err := oe.recoverRun(ns, ref.WorkflowId, ref.RunId, alive)
				if err != nil {
					return err
				}
				if r := (deferredRun{p, ns, ref.WorkflowId, ref.RunId}); later {
					e.later.put(r)
				} else {
					e.later.drop(r)
				}
			}
		}
	}
	return nil
}

// aliveFn answers whether an engine is alive, from the engines alive when it was
// made and, for an owner that is not among them, from a fresh reading taken at most
// once a second: a pass over a large store takes long enough for an engine to join and
// hand out work that the pass would otherwise take for a dead engine's.
func (e *engine) aliveFn() (func(owner string) bool, error) {
	live, err := e.cl.live()
	if err != nil {
		return nil, err
	}
	at := time.Now()
	return func(owner string) bool {
		if owner == "" {
			return false
		}
		if !live[owner] && time.Since(at) > time.Second {
			if fresh, err := e.cl.live(); err == nil {
				live, at = fresh, time.Now()
			}
		}
		return live[owner]
	}, nil
}

func (e *engine) recoverRun(ns, wf, run string, alive func(owner string) bool) (later bool, err error) {
	unlock := e.lockRun(ns, wf, run)
	defer unlock()
	row, ok, err := e.DescribeWorkflow(ns, wf, run)
	if err != nil {
		return false, err
	}
	if !ok || isTerminal(row.Status) {
		// An entry its row contradicts: the process stopped between the
		// writes that open or close a run. There is nothing to drive.
		return false, e.store.del(openKey(ns, wf, run))
	}
	acts, err := e.listWorkflowActivities(ns, wf, run)
	if err != nil {
		return false, err
	}
	for k := range acts {
		if acts[k].isTerminal() || alive(acts[k].Owner) {
			continue
		}
		if !e.serves(ns, acts[k].TaskQueue, kindActivity) {
			later = true // another replica has the worker for it, or this one will
			continue
		}
		rec := acts[k]
		if err := e.dispatchActivityAttempt(ns, wf, run, &rec, false); err != nil {
			return false, err
		}
	}
	task, claimed, err := e.getTask(ns, wf, run)
	if err != nil {
		return false, err
	}
	if claimed && (task.Idle || alive(task.Owner)) {
		return later, nil
	}
	if !e.serves(ns, row.TaskQueue, kindWorkflow) {
		return true, nil
	}
	return later, e.scheduleWorkflowTask(ns, wf, run)
}

// serves reports whether this engine can hand work for (ns, queue) to a worker. An
// engine that owns its files is the only one there is, and takes work whether or not
// its worker is up yet: it waits in the dispatcher for it, as work always has. One of
// several replicas takes only what it can deliver, because a replica whose app has
// not started that worker (apps start a worker the first time a request needs it)
// would otherwise hold work that another replica could have run.
func (e *engine) serves(ns, queue string, kind taskKind) bool {
	if !e.cl.shared() {
		return true
	}
	if queue == "" {
		queue = "default"
	}
	return e.disp != nil && e.disp.HasSubscribers(ns, queue, kind)
}

// deferred is the runs this engine took up and could not deliver: they are looked at
// again on every beat, and are cheap to look at, where walking every open run again
// would not be.
type deferred struct {
	mu   sync.Mutex
	runs map[deferredRun]struct{}
}

type deferredRun struct {
	p          Principal
	ns, wf, id string
}

func newDeferred() *deferred { return &deferred{runs: map[deferredRun]struct{}{}} }

func (d *deferred) put(r deferredRun) {
	d.mu.Lock()
	d.runs[r] = struct{}{}
	d.mu.Unlock()
}

func (d *deferred) drop(r deferredRun) {
	d.mu.Lock()
	delete(d.runs, r)
	d.mu.Unlock()
}

func (d *deferred) list() []deferredRun {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]deferredRun, 0, len(d.runs))
	for r := range d.runs {
		out = append(out, r)
	}
	return out
}

func (d *deferred) len() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.runs)
}

// Retry looks again at the runs this engine could not deliver.
func (e *engine) retryLater() error {
	alive, err := e.aliveFn()
	if err != nil {
		return err
	}
	for _, r := range e.later.list() {
		later, err := e.As(r.p).recoverRun(r.ns, r.wf, r.id, alive)
		if err != nil {
			return err
		}
		if !later {
			e.later.drop(r)
		}
	}
	return nil
}
