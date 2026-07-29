// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Failure reporting — how the engine says, durably, that work it owns is
// broken.
//
// The incident this exists for: cloud's clients/cron schedules fire a
// JobWorkflow whose RunJobActivity re-reads the entry's ConfigMap at fire
// time. The cloud ServiceAccount had no RBAC to read ConfigMaps, so the
// ACTIVITY failed on every single fire for 11 days. StartWorkflow SUCCEEDED
// every time — the schedule's actionCount reached 4489 — so the scheduler
// was genuinely healthy and silent by correctness; the fault lived one layer
// down. All that layer did with it was e.emit(Event{Kind:"activity.failed"})
// into the SSE broker: ephemeral, no subscriber, nothing written anywhere.
// Six nightly backups produced nothing and every dashboard read green. It
// was found only by hand-reading the engine's SQLite.
//
// So a failure now produces two things, and the split is deliberate:
//
//   - a DURABLE row per failing identity, "fail/<ns>/<fingerprint>". It
//     needs no subscriber and outlives the process. It is the evidence:
//     how many consecutive failures, since when, the last error, and a
//     (workflowId, runId) to go read. Callers get it back through
//     View.FailureStreaks — nobody has to open SQLite again.
//   - a THROTTLED log line off the same event, so an operator or an alert
//     that only ever sees stdout still gets paged.
//
// Identity is the recurring SHAPE of the work — (workflowType,
// activityType, taskQueue, scheduleId) — never the run. Every cron fire
// mints a fresh runId, so a run-keyed counter would have read "attempt 1 of
// 10" forty thousand times and never once said "this has been dead for 11
// days". The shape is also what an operator can actually fix.
//
// Nothing here changes retry or isolation semantics. A failure is retried,
// and isolated, exactly as before; it just stops being invisible.

// failReportThrottle is how many consecutive failures of one identity pass
// between log lines after the first. Failures repeat on their own cadence
// (a 5s scheduler tick, a 1s-and-doubling activity backoff), so unthrottled
// a single broken thing writes thousands of identical lines a day and
// buries the signal as thoroughly as silence did. One in 60 keeps any
// realistic dashboard window holding several lines.
const failReportThrottle = 60

// failPersistentAfter is how long an unbroken failure streak must run before
// it counts as PERSISTENT — "this has failed every attempt for a long time",
// the alertable condition that was invisible for 11 days, as opposed to "it
// just failed once and will retry".
//
// Age, not count, on purpose. A nightly backup reaches a count of only 2 in
// two days yet is unambiguously broken, while a single run burning its 10
// default attempts (1s doubling to a 100s cap ≈ 5.5 minutes of backoff)
// must NOT trip it. 15 minutes clears the second and catches the first on
// its second night instead of its eleventh.
const failPersistentAfter = 15 * time.Minute

// shouldReport is the ONE throttle rule in this package: the FIRST failure
// of a streak (the alert), then every `every`-th (the heartbeat, carrying
// whatever the current error text is).
//
// Deliberately NOT "report whenever the error text changes": failure text
// can embed a freshly minted random runId, so a text-change rule would fire
// on every occurrence and restore exactly the flood this exists to prevent.
// A failure mode that shifts instead surfaces on the next heartbeat.
func shouldReport(n, every int) bool { return n == 1 || (every > 0 && n%every == 0) }

// failTracker counts consecutive failures per key in memory so a reporter
// can alert on the first and throttle the rest. Its `every` is fixed at
// construction: the tracker owns its cadence, no call site restates it.
type failTracker struct {
	mu    sync.Mutex
	every int
	n     map[string]int
}

func newFailTracker(every int) *failTracker {
	return &failTracker{every: every, n: map[string]int{}}
}

// fail records a failure for key and reports the consecutive-failure count
// plus whether this one should be reported (see shouldReport).
func (f *failTracker) fail(key string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n[key]++
	n := f.n[key]
	return n, shouldReport(n, f.every)
}

// ok clears key and reports how many consecutive failures preceded it
// (0 = it was already healthy). Clearing is what makes something that
// breaks again alert immediately instead of waiting out the throttle, and
// it is what keeps the map from growing once the fault is fixed.
func (f *failTracker) ok(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.n[key]
	delete(f.n, key)
	return n
}

// ── the durable record ──────────────────────────────────────────────────

// FailureStreak is the durable aggregate for ONE execution identity that is
// currently failing: an activity type (ActivityType set) or a workflow type
// failing on its own (ActivityType empty). One row per broken thing, upserted
// on each failure and DELETED on the first success — so the keyspace is
// bounded by how much is broken right now, not by how often it failed, and a
// non-empty listing is exactly the list of what needs attention.
type FailureStreak struct {
	Org          string `json:"org,omitempty"` // "" = unscoped (embedded/dev)
	Namespace    string `json:"namespace"`
	WorkflowType string `json:"workflowType,omitempty"`
	// ActivityType empty ⇒ the workflow itself failed (a decider fault with
	// no activity to blame), not an activity within it.
	ActivityType string `json:"activityType,omitempty"`
	TaskQueue    string `json:"taskQueue,omitempty"`
	// ScheduleId is set when the run came from a cron schedule, which is the
	// field that turns "some JobWorkflow is broken" into "THIS nightly backup
	// has not run". Stamped by the sweeper as a search attribute.
	ScheduleId string `json:"scheduleId,omitempty"`
	// ConsecutiveFailures is the how-long-has-this-been-dead number: 1 is a
	// blip, 4489 is the incident. Survives restart, unlike the in-memory
	// throttle counter.
	ConsecutiveFailures int64 `json:"consecutiveFailures"`
	// Attempt is the per-run retry attempt of the LAST failure — a different
	// axis from ConsecutiveFailures, which spans runs.
	Attempt int `json:"attempt,omitempty"`
	// Retrying distinguishes "failed and the engine will try again" from
	// "retries are exhausted and this run is dead".
	Retrying bool `json:"retrying,omitempty"`
	// Persistent is the alertable condition: failing continuously for at
	// least failPersistentAfter.
	Persistent       bool   `json:"persistent,omitempty"`
	FirstFailureTime string `json:"firstFailureTime"`
	LastFailureTime  string `json:"lastFailureTime"`
	LastError        string `json:"lastError,omitempty"`
	// LastWorkflowId / LastRunId point at one concrete run to go read. The
	// identity itself is deliberately run-independent.
	LastWorkflowId string `json:"lastWorkflowId,omitempty"`
	LastRunId      string `json:"lastRunId,omitempty"`
}

// failureIdentity is WHAT failed, at the granularity an operator can act on.
// The first four fields form the fingerprint; WorkflowId/RunId/Attempt ride
// along as last-seen detail and are deliberately NOT part of it.
type failureIdentity struct {
	Namespace    string
	WorkflowType string
	ActivityType string
	TaskQueue    string
	ScheduleId   string
	WorkflowId   string
	RunId        string
	Attempt      int
}

// fingerprint hashes the identity into a fixed-length, key-safe token. A
// hash rather than the joined names because every component is caller-supplied
// (workflow types, queue names) and a '/' in one of them would otherwise
// reshape the store key.
func (id failureIdentity) fingerprint() string {
	sum := sha256.Sum256([]byte(strings.Join(
		[]string{id.WorkflowType, id.ActivityType, id.TaskQueue, id.ScheduleId}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func failKey(ns, fingerprint string) string { return "fail/" + ns + "/" + fingerprint }

// ── report messages ─────────────────────────────────────────────────────

const (
	msgActivityFailed     = "tasks: activity failed"
	msgActivityPersistent = "tasks: activity failing persistently"
	msgActivityRecovered  = "tasks: activity recovered"
	msgWorkflowFailed     = "tasks: workflow failed"
	msgWorkflowPersistent = "tasks: workflow failing persistently"
	msgWorkflowRecovered  = "tasks: workflow recovered"
)

// ── record / clear ──────────────────────────────────────────────────────

// recordFailure upserts the durable streak for id and reports it, throttled.
//
// Best-effort by construction: a store error costs the row but never the log
// line, and no error propagates — observing a failure must never become a
// second failure, and must never change what the caller does about the first.
func (e *engine) recordFailure(id failureIdentity, retrying bool, errText string) {
	key := failKey(id.Namespace, id.fingerprint())
	// Serialize the read-modify-write per identity, the same way the claim
	// path serializes per namespace. Two runs of one broken cron fail
	// concurrently all the time; an increment lost there is a streak length
	// that reads low exactly when it matters.
	unlock := e.runMu.lock(e.orgID + "|fail|" + key)
	defer unlock()

	now := time.Now().UTC()
	var rec FailureStreak
	if ok, _ := e.store.get(key, &rec); !ok {
		rec = FailureStreak{
			Org:              e.orgID,
			Namespace:        id.Namespace,
			WorkflowType:     id.WorkflowType,
			ActivityType:     id.ActivityType,
			TaskQueue:        id.TaskQueue,
			ScheduleId:       id.ScheduleId,
			FirstFailureTime: now.Format(time.RFC3339),
		}
	}
	rec.ConsecutiveFailures++
	rec.Attempt = id.Attempt
	rec.Retrying = retrying
	rec.LastFailureTime = now.Format(time.RFC3339)
	rec.LastError = errText
	rec.LastWorkflowId = id.WorkflowId
	rec.LastRunId = id.RunId
	wasPersistent := rec.Persistent
	rec.Persistent = rec.ConsecutiveFailures > 1 &&
		now.Sub(parseTime(rec.FirstFailureTime, now)) >= failPersistentAfter
	_ = e.store.put(key, rec)

	// The in-memory tracker is the THROTTLE authority; the durable row is the
	// EVIDENCE. Separate jobs, so: a store that is itself failing cannot
	// starve the throttle into reporting every occurrence (a row that never
	// loads reads as count 1 forever), and a restart resets the throttle so a
	// still-broken activity re-alerts once after a deploy rather than waiting
	// out 60 more failures. The COUNT on the line always comes from the row.
	_, throttled := e.fails.fail(e.orgID + "|" + key)
	// The persistent transition always reports, whatever the throttle says.
	// It is the whole point of the change and it can happen on failure #2 of
	// a nightly cron — a throttle-only rule would swallow it for 58 more
	// nights, which is the original bug wearing a different hat.
	if !throttled && !(rec.Persistent && !wasPersistent) {
		return
	}

	msg := msgActivityFailed
	if rec.Persistent {
		msg = msgActivityPersistent
	}
	if id.ActivityType == "" {
		msg = msgWorkflowFailed
		if rec.Persistent {
			msg = msgWorkflowPersistent
		}
	}
	args := []any{
		"org", e.orgID,
		"namespace", id.Namespace,
		"workflowType", id.WorkflowType,
		"activityType", id.ActivityType,
		"taskQueue", id.TaskQueue,
		"scheduleId", id.ScheduleId,
		"workflowId", id.WorkflowId,
		"runId", id.RunId,
		"attempt", id.Attempt,
		"retrying", retrying,
		"consecutiveFailures", rec.ConsecutiveFailures,
		"failingSince", rec.FirstFailureTime,
		"error", errText,
	}
	// ERROR only once it is persistent. A single failure that will be retried
	// is normal operation; paging on it trains operators to ignore the level
	// that carries the 11-day outage.
	if rec.Persistent {
		e.log.Error(msg, args...)
		return
	}
	e.log.Warn(msg, args...)
}

// clearFailure ends the streak for id. The durable row is deleted — the
// keyspace answers "what is broken NOW", so a fixed identity must leave
// nothing behind — and if there WAS a streak, a recovery line closes the loop
// the throttle opens: without it an operator cannot tell something that was
// fixed from something whose next throttled line has not come round yet.
func (e *engine) clearFailure(id failureIdentity) {
	key := failKey(id.Namespace, id.fingerprint())
	unlock := e.runMu.lock(e.orgID + "|fail|" + key)
	defer unlock()

	// Unconditional, so the throttle resets even when the row is already gone.
	e.fails.ok(e.orgID + "|" + key)
	var rec FailureStreak
	if ok, _ := e.store.get(key, &rec); !ok {
		return
	}
	_ = e.store.del(key)

	msg := msgActivityRecovered
	if id.ActivityType == "" {
		msg = msgWorkflowRecovered
	}
	e.log.Info(msg,
		"org", e.orgID,
		"namespace", id.Namespace,
		"workflowType", id.WorkflowType,
		"activityType", id.ActivityType,
		"taskQueue", id.TaskQueue,
		"scheduleId", id.ScheduleId,
		"afterConsecutiveFailures", rec.ConsecutiveFailures,
		"failingSince", rec.FirstFailureTime)
}

// FailureStreaks returns every currently-failing identity in ns, worst first.
// This is the read that replaces hand-decoding the store: one row per broken
// thing, each carrying enough to act — org, namespace, workflow/activity type,
// task queue, originating schedule, how many consecutive failures, since when,
// the last error, and a run to go read.
func (e *engine) FailureStreaks(ns string) ([]FailureStreak, error) {
	rows, err := listInto[FailureStreak](e.store, fmt.Sprintf("fail/%s/", ns))
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ConsecutiveFailures != rows[j].ConsecutiveFailures {
			return rows[i].ConsecutiveFailures > rows[j].ConsecutiveFailures
		}
		if rows[i].WorkflowType != rows[j].WorkflowType {
			return rows[i].WorkflowType < rows[j].WorkflowType
		}
		return rows[i].ActivityType < rows[j].ActivityType
	})
	return rows, nil
}

// ── identity constructors ───────────────────────────────────────────────

// searchAttrScheduleID is the visibility attribute the schedule paths stamp
// on every run they start. It is what lets a failure report name the cron
// entry that is dead, and it makes `ScheduleId = "nightly-backup"` a
// queryable filter over executions at the same time.
const searchAttrScheduleID = "ScheduleId"

// scheduleIDOf reads the stamped schedule id off an execution. Empty for runs
// that were not started by a schedule.
func scheduleIDOf(wf *WorkflowExecution) string {
	if wf == nil {
		return ""
	}
	s, _ := wf.SearchAttrs[searchAttrScheduleID].(string)
	return s
}

// activityFailureIdentity identifies one workflow-driven activity. wf may be
// nil (the execution row vanished); the namespace, activity type and queue
// still name the work.
func activityFailureIdentity(wf *WorkflowExecution, ns, wfID, runID string, rec *workflowActivity) failureIdentity {
	id := failureIdentity{
		Namespace:    ns,
		ActivityType: rec.ActivityType,
		TaskQueue:    rec.TaskQueue,
		ScheduleId:   scheduleIDOf(wf),
		WorkflowId:   wfID,
		RunId:        runID,
		Attempt:      rec.Attempt,
	}
	if wf != nil {
		id.WorkflowType = wf.Type.Name
		if id.TaskQueue == "" {
			id.TaskQueue = wf.TaskQueue
		}
	}
	return id
}

// standaloneFailureIdentity identifies a standalone activity, which has no
// enclosing workflow — its type and queue are the whole shape.
func standaloneFailureIdentity(ns string, a *StandaloneActivity) failureIdentity {
	return failureIdentity{
		Namespace:    ns,
		ActivityType: a.Type.Name,
		TaskQueue:    a.TaskQueue,
		WorkflowId:   a.Execution.WorkflowId,
		RunId:        a.Execution.RunId,
		Attempt:      a.Attempt,
	}
}

// workflowFailureIdentity identifies a workflow failing on its own account.
// ActivityType stays empty: there is no activity to blame, which is exactly
// the case an activity-only report would miss (a decider that faults on
// episode 0 never schedules anything).
func workflowFailureIdentity(ns string, wf *WorkflowExecution) failureIdentity {
	return failureIdentity{
		Namespace:    ns,
		WorkflowType: wf.Type.Name,
		TaskQueue:    wf.TaskQueue,
		ScheduleId:   scheduleIDOf(wf),
		WorkflowId:   wf.Execution.WorkflowId,
		RunId:        wf.Execution.RunId,
	}
}

// parseTime reads an RFC3339 stamp, falling back to def on anything
// unparseable so a corrupt row degrades the report instead of skewing it.
func parseTime(s string, def time.Time) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return def
	}
	return t
}
