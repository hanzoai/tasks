// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"fmt"
	"sort"
	"time"
)

// Standalone activities are first-class records keyed by
// (ns, activityId, runId) under "act/" with their own history under
// "ahist/". They live independent of any workflow and use the
// ACTIVITY_TASK_STATE_* status family.

const (
	activityStateScheduled = "ACTIVITY_TASK_STATE_SCHEDULED"
	activityStateStarted   = "ACTIVITY_TASK_STATE_STARTED"
	activityStateCompleted = "ACTIVITY_TASK_STATE_COMPLETED"
	activityStateFailed    = "ACTIVITY_TASK_STATE_FAILED"
	activityStateCanceled  = "ACTIVITY_TASK_STATE_CANCELED"
)

// activityIdempotency stores the runId previously associated with
// (activityId, requestId) so retried StartActivity calls converge.
type activityIdempotency struct {
	RunId string `json:"runId"`
}

// StartActivity persists a standalone activity in SCHEDULED state and
// appends ACTIVITY_TASK_SCHEDULED to its history. Idempotent on
// requestID: a second call returns the prior activity unchanged.
func (e *engine) StartActivity(ns, activityID, runID string, typ TypeRef, taskQueue string, input any, retry *RetryPolicy, scheduleToClose, scheduleToStart, startToClose, heartbeat string, identity, requestID string) (*StandaloneActivity, error) {
	if _, ok, _ := e.DescribeNamespace(ns); !ok {
		return nil, fmt.Errorf("namespace %q not registered", ns)
	}
	if typ.Name == "" {
		return nil, fmt.Errorf("activity type required")
	}
	if activityID == "" {
		return nil, fmt.Errorf("activityId required")
	}
	if requestID != "" {
		var prev activityIdempotency
		if ok, _ := e.store.get(fmt.Sprintf("aidem/%s/%s/%s", ns, activityID, requestID), &prev); ok && prev.RunId != "" {
			if a, ok2, _ := e.DescribeActivity(ns, activityID, prev.RunId); ok2 {
				return a, nil
			}
		}
	}
	if runID == "" {
		runID = newRunId()
	}
	if taskQueue == "" {
		taskQueue = "default"
	}
	maxAttempts := 0
	if retry != nil {
		maxAttempts = retry.MaximumAttempts
	}
	a := StandaloneActivity{
		Execution:              ExecutionRef{WorkflowId: activityID, RunId: runID},
		Type:                   typ,
		TaskQueue:              taskQueue,
		Status:                 activityStateScheduled,
		StartTime:              nowRFC3339(),
		RetryPolicy:            retry,
		Input:                  input,
		Identity:               identity,
		Attempt:                1,
		MaximumAttempts:        maxAttempts,
		ScheduleToCloseTimeout: scheduleToClose,
		ScheduleToStartTimeout: scheduleToStart,
		StartToCloseTimeout:    startToClose,
		HeartbeatTimeout:       heartbeat,
	}
	if err := e.store.put(actKey(ns, activityID, runID), a); err != nil {
		return nil, err
	}
	if requestID != "" {
		_ = e.store.put(fmt.Sprintf("aidem/%s/%s/%s", ns, activityID, requestID), activityIdempotency{RunId: runID})
	}
	if _, err := e.appendActivityHistory(ns, activityID, runID, "ACTIVITY_TASK_SCHEDULED", map[string]any{
		"activityType": typ.Name,
		"taskQueue":    taskQueue,
		"input":        input,
		"identity":     identity,
	}); err != nil {
		return nil, err
	}
	out, _, _ := e.DescribeActivity(ns, activityID, runID)
	e.emit(Event{Kind: "activity.scheduled", Namespace: ns, WorkflowID: activityID, RunID: runID, Data: out})
	return out, nil
}

// defaultLeaseTTL bounds how long a claimed activity may go without a
// heartbeat before the reaper reclaims it, when neither the activity's
// HeartbeatTimeout nor the claim's lease say otherwise.
const defaultLeaseTTL = 60 * time.Second

// activityLeaseTTL derives the lease window for a claimed activity: its own
// HeartbeatTimeout (the job author's heartbeat SLA doubles as the claim
// lease) if set, else the worker-supplied fallback, else defaultLeaseTTL.
func activityLeaseTTL(a *StandaloneActivity, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(a.HeartbeatTimeout); err == nil && d > 0 {
		return d
	}
	if fallback > 0 {
		return fallback
	}
	return defaultLeaseTTL
}

// ClaimNextActivity atomically claims the oldest SCHEDULED activity in ns
// matching taskQueue (empty ⇒ any queue) for worker `identity`, transitioning
// it SCHEDULED→STARTED under a lease. This is the NAT-safe worker primitive:
// an outbound worker polls it to pull work rather than accepting a push.
//
// Before claiming it reaps expired leases (reapExpiredLeases) so a dead
// worker's in-flight activity returns to SCHEDULED and this or a peer worker
// reclaims it — self-healing on the hot path, no separate timer required in
// the org-scoped (cloud) deployment where the background sweep cannot see the
// per-org shard. The per-(org,ns) claim lock serializes concurrent workers so
// no two ever claim the same activity. Returns (nil,false,nil) when the queue
// is empty.
func (e *engine) ClaimNextActivity(ns, taskQueue, identity string, lease time.Duration) (*StandaloneActivity, bool, error) {
	if _, ok, _ := e.DescribeNamespace(ns); !ok {
		return nil, false, fmt.Errorf("namespace %q not registered", ns)
	}
	unlock := e.runMu.lock(e.principal.String() + "|claim|" + ns)
	defer unlock()
	if err := e.reapExpiredLeases(ns); err != nil {
		return nil, false, err
	}
	rows, err := listInto[StandaloneActivity](e.store, fmt.Sprintf("act/%s/", ns))
	if err != nil {
		return nil, false, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].StartTime == rows[j].StartTime {
			return rows[i].Execution.WorkflowId < rows[j].Execution.WorkflowId
		}
		return rows[i].StartTime < rows[j].StartTime
	})
	for i := range rows {
		a := rows[i]
		if a.Status != activityStateScheduled {
			continue
		}
		if taskQueue != "" && a.TaskQueue != taskQueue {
			continue
		}
		wf, run := a.Execution.WorkflowId, a.Execution.RunId
		now := time.Now().UTC()
		a.Status = activityStateStarted
		a.Identity = identity
		a.LastHeartbeatTime = now.Format(time.RFC3339)
		a.LeaseExpiry = now.Add(activityLeaseTTL(&a, lease)).Format(time.RFC3339)
		if err := e.store.put(actKey(ns, wf, run), a); err != nil {
			return nil, false, err
		}
		if _, err := e.appendActivityHistory(ns, wf, run, "ACTIVITY_TASK_STARTED", map[string]any{
			"identity":    identity,
			"attempt":     a.Attempt,
			"leaseExpiry": a.LeaseExpiry,
		}); err != nil {
			return nil, false, err
		}
		out, _, _ := e.DescribeActivity(ns, wf, run)
		e.emit(Event{Kind: "activity.started", Namespace: ns, WorkflowID: wf, RunID: run, Data: out})
		return out, true, nil
	}
	return nil, false, nil
}

// reapExpiredLeases returns each STARTED activity in ns whose lease elapsed
// back to SCHEDULED (Attempt incremented, lease + claimant cleared) so a
// worker reclaims it; an activity that has exhausted MaximumAttempts is FAILED
// instead. Activities without a lease (LeaseExpiry=="") are left untouched —
// they are presence/heartbeat records, not claimed jobs. Callers on the claim
// path hold the ns claim lock; the background sweep calls it directly.
func (e *engine) reapExpiredLeases(ns string) error {
	rows, err := listInto[StandaloneActivity](e.store, fmt.Sprintf("act/%s/", ns))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for i := range rows {
		a := rows[i]
		if a.Status != activityStateStarted || a.LeaseExpiry == "" {
			continue
		}
		exp, perr := time.Parse(time.RFC3339, a.LeaseExpiry)
		if perr != nil || !now.After(exp) {
			continue
		}
		wf, run := a.Execution.WorkflowId, a.Execution.RunId
		if a.MaximumAttempts > 0 && a.Attempt >= a.MaximumAttempts {
			_ = e.FailActivity(ns, wf, run, "lease expired: worker unresponsive", "reaper")
			continue
		}
		a.Status = activityStateScheduled
		a.Attempt++
		a.Identity = ""
		a.LeaseExpiry = ""
		if err := e.store.put(actKey(ns, wf, run), a); err != nil {
			return err
		}
		if _, err := e.appendActivityHistory(ns, wf, run, "ACTIVITY_TASK_TIMED_OUT", map[string]any{
			"reason":  "lease expired",
			"attempt": a.Attempt,
		}); err != nil {
			return err
		}
		out, _, _ := e.DescribeActivity(ns, wf, run)
		e.emit(Event{Kind: "activity.timedout", Namespace: ns, WorkflowID: wf, RunID: run, Data: out})
	}
	return nil
}

// ListActivities returns standalone activities in ns. cursor is the
// activityId+runId from the previous page; pageSize<=0 → 100.
func (e *engine) ListActivities(ns, cursor string, pageSize int) ([]StandaloneActivity, string, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	rows, err := listInto[StandaloneActivity](e.store, fmt.Sprintf("act/%s/", ns))
	if err != nil {
		return nil, "", err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Execution.WorkflowId == rows[j].Execution.WorkflowId {
			return rows[i].Execution.RunId < rows[j].Execution.RunId
		}
		return rows[i].Execution.WorkflowId < rows[j].Execution.WorkflowId
	})
	start := 0
	if cursor != "" {
		for i := range rows {
			k := rows[i].Execution.WorkflowId + "|" + rows[i].Execution.RunId
			if k == cursor {
				start = i + 1
				break
			}
		}
	}
	end := start + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	page := rows[start:end]
	next := ""
	if end < len(rows) && len(page) > 0 {
		last := page[len(page)-1]
		next = last.Execution.WorkflowId + "|" + last.Execution.RunId
	}
	return page, next, nil
}

// DescribeActivity returns the activity record at (ns, activityId, runId).
func (e *engine) DescribeActivity(ns, activityID, runID string) (*StandaloneActivity, bool, error) {
	var a StandaloneActivity
	ok, err := e.store.get(actKey(ns, activityID, runID), &a)
	if !ok {
		return nil, false, err
	}
	return &a, true, err
}

// CancelActivity transitions the activity to CANCELED. Rejected if the
// activity is already in a terminal state.
func (e *engine) CancelActivity(ns, activityID, runID, reason, identity string) error {
	return e.activityTerminal(ns, activityID, runID, activityStateCanceled, "ACTIVITY_TASK_CANCELED", "activity.canceled", map[string]any{
		"reason":   reason,
		"identity": identity,
	}, "", "")
}

// CompleteActivity transitions the activity to COMPLETED with a result.
func (e *engine) CompleteActivity(ns, activityID, runID string, result any, identity string) error {
	return e.activityTerminal(ns, activityID, runID, activityStateCompleted, "ACTIVITY_TASK_COMPLETED", "activity.completed", map[string]any{
		"result":   result,
		"identity": identity,
	}, "result", "")
}

// FailActivity transitions the activity to FAILED with a cause.
func (e *engine) FailActivity(ns, activityID, runID, cause, identity string) error {
	return e.activityTerminal(ns, activityID, runID, activityStateFailed, "ACTIVITY_TASK_FAILED", "activity.failed", map[string]any{
		"cause":    cause,
		"identity": identity,
	}, "", cause)
}

// HeartbeatActivity bumps LastHeartbeatTime. Rejects if terminal.
func (e *engine) HeartbeatActivity(ns, activityID, runID string, details any) error {
	a, ok, err := e.DescribeActivity(ns, activityID, runID)
	if !ok {
		return fmt.Errorf("activity not found")
	}
	if err != nil {
		return err
	}
	if isActivityTerminal(a.Status) {
		return fmt.Errorf("activity terminal: status=%s", a.Status)
	}
	a.LastHeartbeatTime = nowRFC3339()
	if a.Status == activityStateScheduled {
		a.Status = activityStateStarted
	}
	// A live heartbeat extends the lease of a claimed activity so the reaper
	// leaves it be. Unclaimed presence records (no lease) stay lease-free.
	if a.LeaseExpiry != "" {
		a.LeaseExpiry = time.Now().UTC().Add(activityLeaseTTL(a, 0)).Format(time.RFC3339)
	}
	if err := e.store.put(actKey(ns, activityID, runID), *a); err != nil {
		return err
	}
	if _, err := e.appendActivityHistory(ns, activityID, runID, "ACTIVITY_TASK_HEARTBEAT", map[string]any{
		"details": details,
	}); err != nil {
		return err
	}
	e.emit(Event{Kind: "activity.heartbeat", Namespace: ns, WorkflowID: activityID, RunID: runID, Data: map[string]any{"details": details}})
	return nil
}

// GetActivityHistory returns events in [afterEventID+1, +pageSize] in
// the requested order. afterEventID==0 starts from the beginning.
// pageSize<=0 defaults to 100.
func (e *engine) GetActivityHistory(ns, activityID, runID string, afterEventID int64, pageSize int, reverse bool) ([]HistoryEvent, int64, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	if _, ok, _ := e.DescribeActivity(ns, activityID, runID); !ok {
		return nil, 0, fmt.Errorf("activity not found")
	}
	all, err := listInto[HistoryEvent](e.store, fmt.Sprintf("ahist/%s/%s/%s/", ns, activityID, runID))
	if err != nil {
		return nil, 0, err
	}
	if reverse {
		for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
			all[i], all[j] = all[j], all[i]
		}
	}
	out := make([]HistoryEvent, 0, pageSize)
	for _, ev := range all {
		if !reverse && ev.EventId <= afterEventID {
			continue
		}
		if reverse && afterEventID > 0 && ev.EventId >= afterEventID {
			continue
		}
		out = append(out, ev)
		if len(out) >= pageSize {
			break
		}
	}
	var nextCursor int64
	if len(out) == pageSize {
		nextCursor = out[len(out)-1].EventId
	}
	return out, nextCursor, nil
}

func isActivityTerminal(s string) bool {
	switch s {
	case activityStateCompleted, activityStateFailed, activityStateCanceled:
		return true
	}
	return false
}

func actKey(ns, activityID, runID string) string {
	return fmt.Sprintf("act/%s/%s/%s", ns, activityID, runID)
}

// activityTerminal applies a terminal transition to a standalone
// activity record. Idempotent against terminal states (returns nil).
// resultField/cause carry the optional bookkeeping for the activity row
// itself (CompleteActivity sets Result, FailActivity sets FailureCause).
func (e *engine) activityTerminal(ns, activityID, runID, status, eventType, evKind string, attrs map[string]any, resultField, cause string) error {
	a, ok, err := e.DescribeActivity(ns, activityID, runID)
	if !ok {
		return fmt.Errorf("activity not found")
	}
	if err != nil {
		return err
	}
	if isActivityTerminal(a.Status) {
		return fmt.Errorf("activity terminal: status=%s", a.Status)
	}
	a.Status = status
	a.CloseTime = nowRFC3339()
	if resultField == "result" {
		a.Result = attrs["result"]
	}
	if cause != "" {
		a.FailureCause = cause
	}
	if err := e.store.put(actKey(ns, activityID, runID), *a); err != nil {
		return err
	}
	if _, err := e.appendActivityHistory(ns, activityID, runID, eventType, attrs); err != nil {
		return err
	}
	out, _, _ := e.DescribeActivity(ns, activityID, runID)
	e.emit(Event{Kind: evKind, Namespace: ns, WorkflowID: activityID, RunID: runID, Data: out})
	// Standalone activities reach here from the fleet queue (fn.run,
	// studio.render) and from the lease reaper. Same rule as the durable
	// path: FAILED is evidence, COMPLETED ends the streak, and CANCELED is
	// neither a fault nor a success so it leaves the streak alone.
	switch status {
	case activityStateFailed:
		e.recordFailure(standaloneFailureIdentity(ns, a), false, cause)
	case activityStateCompleted:
		e.clearFailure(standaloneFailureIdentity(ns, a))
	}
	return nil
}

// appendActivityHistory mirrors appendHistory but lives under "ahist/"
// and bumps StandaloneActivity.HistoryLength.
func (e *engine) appendActivityHistory(ns, activityID, runID, eventType string, attrs map[string]any) (*HistoryEvent, error) {
	a, ok, err := e.DescribeActivity(ns, activityID, runID)
	if !ok {
		return nil, fmt.Errorf("activity not found")
	}
	if err != nil {
		return nil, err
	}
	a.HistoryLength++
	ev := &HistoryEvent{
		EventId:    a.HistoryLength,
		EventTime:  nowRFC3339(),
		EventType:  eventType,
		Attributes: attrs,
	}
	if err := e.store.put(fmt.Sprintf("ahist/%s/%s/%s/%020d", ns, activityID, runID, ev.EventId), ev); err != nil {
		return nil, err
	}
	if err := e.store.put(actKey(ns, activityID, runID), *a); err != nil {
		return nil, err
	}
	return ev, nil
}

// ── deployment patches / versions ───────────────────────────────────

// DeploymentPatch carries the optional fields of an UpdateDeployment.
// Empty strings are ignored; non-nil maps fully replace existing data.
type DeploymentPatch struct {
	Description    *string `json:"description,omitempty"`
	OwnerEmail     *string `json:"ownerEmail,omitempty"`
	DefaultCompute *string `json:"defaultCompute,omitempty"`
}

// UpdateDeployment patches metadata on an existing deployment. Name and
// Versions cannot be updated through this path.
func (e *engine) UpdateDeployment(ns, name string, p DeploymentPatch) (*Deployment, error) {
	d, ok, err := e.DescribeDeployment(ns, name)
	if !ok {
		return nil, fmt.Errorf("deployment not found")
	}
	if err != nil {
		return nil, err
	}
	if p.Description != nil {
		d.Description = *p.Description
	}
	if p.OwnerEmail != nil {
		d.OwnerEmail = *p.OwnerEmail
	}
	if p.DefaultCompute != nil {
		d.DefaultCompute = *p.DefaultCompute
	}
	d.UpdateTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("dp/%s/%s", ns, name), *d); err != nil {
		return nil, err
	}
	return d, nil
}

// DeleteDeployment removes a deployment. Refuses if any versions exist
// unless force=true.
func (e *engine) DeleteDeployment(ns, name string, force bool) error {
	d, ok, err := e.DescribeDeployment(ns, name)
	if !ok {
		return fmt.Errorf("deployment not found")
	}
	if err != nil {
		return err
	}
	if !force && len(d.Versions) > 0 {
		return fmt.Errorf("deployment has %d versions; use force=true to delete", len(d.Versions))
	}
	return e.store.del(fmt.Sprintf("dp/%s/%s", ns, name))
}

// CreateVersion appends a new version to an existing deployment. The
// first version becomes the default. Rejects duplicate buildId.
func (e *engine) CreateVersion(ns, name, buildId, description, compute, image string, env map[string]string) (*DeploymentVersion, error) {
	if buildId == "" {
		return nil, fmt.Errorf("buildId required")
	}
	d, ok, err := e.DescribeDeployment(ns, name)
	if !ok {
		return nil, fmt.Errorf("deployment not found")
	}
	if err != nil {
		return nil, err
	}
	for _, v := range d.Versions {
		if v.BuildId == buildId {
			return nil, fmt.Errorf("buildId %q already exists", buildId)
		}
	}
	state := "DEPLOYMENT_STATE_DRAFT"
	if d.DefaultBuildId == "" {
		state = "DEPLOYMENT_STATE_CURRENT"
		d.DefaultBuildId = buildId
	}
	v := DeploymentVersion{
		BuildId:     buildId,
		State:       state,
		Description: description,
		Compute:     compute,
		Image:       image,
		Env:         env,
		CreateTime:  nowRFC3339(),
	}
	d.Versions = append(d.Versions, v)
	d.UpdateTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("dp/%s/%s", ns, name), *d); err != nil {
		return nil, err
	}
	return &v, nil
}

// DeploymentVersionPatch is metadata-only — buildId and CreateTime are
// immutable.
type DeploymentVersionPatch struct {
	Description *string           `json:"description,omitempty"`
	Compute     *string           `json:"compute,omitempty"`
	Image       *string           `json:"image,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// UpdateVersion patches metadata on a deployment version. buildId is
// immutable.
func (e *engine) UpdateVersion(ns, name, buildId string, p DeploymentVersionPatch) (*DeploymentVersion, error) {
	d, ok, err := e.DescribeDeployment(ns, name)
	if !ok {
		return nil, fmt.Errorf("deployment not found")
	}
	if err != nil {
		return nil, err
	}
	for i := range d.Versions {
		if d.Versions[i].BuildId != buildId {
			continue
		}
		if p.Description != nil {
			d.Versions[i].Description = *p.Description
		}
		if p.Compute != nil {
			d.Versions[i].Compute = *p.Compute
		}
		if p.Image != nil {
			d.Versions[i].Image = *p.Image
		}
		if p.Env != nil {
			d.Versions[i].Env = p.Env
		}
		d.Versions[i].UpdateTime = nowRFC3339()
		d.UpdateTime = d.Versions[i].UpdateTime
		if err := e.store.put(fmt.Sprintf("dp/%s/%s", ns, name), *d); err != nil {
			return nil, err
		}
		v := d.Versions[i]
		return &v, nil
	}
	return nil, fmt.Errorf("buildId %q not found", buildId)
}

// VersionValidationResult is the synthetic snapshot returned by
// ValidateVersion. Real validation requires the worker SDK runtime to
// answer; for now we return a deterministic body keyed off whether the
// version row exists.
type VersionValidationResult struct {
	NetworkOk         bool     `json:"networkOk"`
	WorkerRegistered  bool     `json:"workerRegistered"`
	HeartbeatReceived bool     `json:"heartbeatReceived"`
	LatencyMs         int      `json:"latencyMs"`
	Errors            []string `json:"errors"`
}

// ValidateVersion returns the snapshot of validation checks. Today the
// result is synthetic — wire the worker SDK runtime probes here when
// they land. The shape is the contract.
func (e *engine) ValidateVersion(ns, name, buildId string) (*VersionValidationResult, error) {
	d, ok, err := e.DescribeDeployment(ns, name)
	if !ok {
		return nil, fmt.Errorf("deployment not found")
	}
	if err != nil {
		return nil, err
	}
	for _, v := range d.Versions {
		if v.BuildId != buildId {
			continue
		}
		// Latency probe stub: time the round-trip to the store.
		start := time.Now()
		_, _, _ = e.DescribeDeployment(ns, name)
		latency := int(time.Since(start) / time.Millisecond)
		return &VersionValidationResult{
			NetworkOk:         true,
			WorkerRegistered:  len(e.workers.List(ns)) > 0,
			HeartbeatReceived: false, // requires worker SDK runtime
			LatencyMs:         latency,
			Errors:            []string{},
		}, nil
	}
	return nil, fmt.Errorf("buildId %q not found", buildId)
}
