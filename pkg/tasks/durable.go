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
//   - Recover — boot-time crash-recovery that re-dispatches in-flight
//     activities and re-enqueues workflow tasks from the durable store.
//
// Advancing a single run is serialized by a per-run mutex (lockRun) so the
// read-check-then-write of a schedule/complete is atomic above the store's
// single writer.

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
// single run. Entry points (respondWorkflowHandler, completeWorkflow
// Activity, recoverRun, the retry timer) acquire it; internal helpers
// (applyScheduleActivity, dispatchActivityAttempt, scheduleWorkflowTask)
// assume the caller holds it.
func (e *engine) lockRun(ns, wf, run string) func() {
	return e.runMu.lock(e.orgID + "|" + ns + "|" + wf + "|" + run)
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

// scheduleWorkflowTask enqueues a workflow task carrying the run's full
// history so the stateless worker replays and advances. No-op if the run
// is already terminal. Caller need not hold the run lock at start (the run
// has no other advancer yet); advance paths call it under the lock.
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
	hist, err := e.workflowHistoryBytes(ns, wf, run)
	if err != nil {
		return err
	}
	if e.disp != nil {
		e.disp.EnqueueWorkflowTask(e.orgID, ns, wfe.TaskQueue, wf, run, wfe.Type.Name, hist)
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
		e.disp.DispatchActivity(e.orgID, ns, rec.TaskQueue, wf, run, rec.Seq, rec.ActivityID, rec.ActivityType, rec.Input, rec.StartToCloseMs, rec.HeartbeatMs)
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
// ignored). Acquires the run lock.
func (e *engine) completeWorkflowActivity(ns, wf, run string, seq int, result, failure []byte) error {
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
// is in-memory; a crash before it fires is covered by the boot recovery
// pass, which re-dispatches any non-terminal activity (losing the residual
// backoff, which is acceptable — durable timers are Phase-2b).
func (e *engine) scheduleActivityRetry(ns, wf, run string, seq int, delay time.Duration) {
	if delay < 0 {
		delay = 0
	}
	time.AfterFunc(delay, func() {
		unlock := e.lockRun(ns, wf, run)
		defer unlock()
		rec, ok, _ := e.getWorkflowActivity(ns, wf, run, seq)
		if !ok || rec.isTerminal() {
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

// Recover rebuilds in-flight dispatch state from the durable store after a
// restart. For every non-terminal workflow it re-dispatches any activity
// that is scheduled/started but not terminal (idempotent per (run, seq))
// and re-enqueues a workflow task so the stateless worker replays and
// advances. Because dispatch is seq-keyed and the decider dedups on replay,
// recovery causes neither lost nor double execution: an already-terminal
// activity is skipped (never re-run), a not-yet-terminal one is re-driven.
func (e *engine) Recover() error {
	namespaces, err := e.ListNamespaces()
	if err != nil {
		return err
	}
	for i := range namespaces {
		ns := namespaces[i].NamespaceInfo.Name
		wfs, err := e.ListWorkflows(ns)
		if err != nil {
			return err
		}
		for j := range wfs {
			if isTerminal(wfs[j].Status) {
				continue
			}
			if err := e.recoverRun(ns, wfs[j].Execution.WorkflowId, wfs[j].Execution.RunId); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *engine) recoverRun(ns, wf, run string) error {
	unlock := e.lockRun(ns, wf, run)
	defer unlock()
	acts, err := e.listWorkflowActivities(ns, wf, run)
	if err != nil {
		return err
	}
	for k := range acts {
		if acts[k].isTerminal() {
			continue
		}
		rec := acts[k]
		if err := e.dispatchActivityAttempt(ns, wf, run, &rec, false); err != nil {
			return err
		}
	}
	return e.scheduleWorkflowTask(ns, wf, run)
}
