// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// cancelTimeout bounds how long a workflow may sit in CANCELING before
// the sweeper auto-acks it to CANCELED. Mutable for tests.
var cancelTimeout = 60 * time.Second

// cancelTracker records when each (ns, wfID, runID) entered CANCELING.
type cancelTracker struct {
	mu sync.Mutex
	at map[string]time.Time
}

func newCancelTracker() *cancelTracker { return &cancelTracker{at: map[string]time.Time{}} }

func (c *cancelTracker) mark(ns, wfID, runID string) {
	c.mu.Lock()
	c.at[wfKey(ns, wfID, runID)] = time.Now()
	c.mu.Unlock()
}

func (c *cancelTracker) clear(ns, wfID, runID string) {
	c.mu.Lock()
	delete(c.at, wfKey(ns, wfID, runID))
	c.mu.Unlock()
}

// expired returns the keys older than cancelTimeout. Caller must hold no lock.
func (c *cancelTracker) expired() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := time.Now().Add(-cancelTimeout)
	out := []string{}
	for k, t := range c.at {
		if t.Before(cutoff) {
			out = append(out, k)
		}
	}
	return out
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// engine wraps the store with domain operations. ZAP handlers and the
// HTTP shim both call into this layer — there is exactly one
// implementation per opcode.
//
// Workflow execution semantics (deterministic replay of user code,
// activity heartbeating, child-workflow lifecycle) are NOT implemented
// here yet. StartWorkflow records the execution as RUNNING and emits
// no events; SignalWorkflow / CancelWorkflow / TerminateWorkflow apply
// state transitions on the record only. This is enough for the UI to
// be inspectable end-to-end and for higher-level callers to depend on
// the API while the worker SDK runtime lands.
type engine struct {
	store      *store
	broker     *broker
	disp       *dispatcher
	cancelling *cancelTracker
	workers    *workerRegistry
	orgID      string // "" = unscoped (embedded/dev). Stamped on every emitted Event.
}

func newEngine(s *store) *engine {
	return &engine{store: s, broker: newBroker(), disp: newDispatcher(), cancelling: newCancelTracker(), workers: newWorkerRegistry()}
}

// WithOrg returns an engine view scoped to orgID. Reads and writes go
// through a store wrapper that prefixes every key with "org:<id>:".
// orgID == "" returns e unchanged (legacy embedded-use behavior).
func (e *engine) WithOrg(orgID string) *engine {
	if orgID == "" {
		return e
	}
	return &engine{store: e.store.withOrg(orgID), broker: e.broker, disp: e.disp, cancelling: e.cancelling, workers: e.workers, orgID: orgID}
}

// emit publishes an event tagged with the engine's org so per-org SSE
// filtering can trust OrgID. Use everywhere instead of e.broker.publish.
func (e *engine) emit(ev Event) {
	ev.OrgID = e.orgID
	e.broker.publish(ev)
}

// ── ids ─────────────────────────────────────────────────────────────

func newRunId() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ── namespaces ──────────────────────────────────────────────────────

func (e *engine) RegisterNamespace(ns Namespace) error {
	if ns.NamespaceInfo.Name == "" {
		return fmt.Errorf("namespace name required")
	}
	if ns.NamespaceInfo.State == "" {
		ns.NamespaceInfo.State = "NAMESPACE_STATE_REGISTERED"
	}
	if ns.NamespaceInfo.CreateTime == "" {
		ns.NamespaceInfo.CreateTime = nowRFC3339()
	}
	if ns.Config.WorkflowExecutionRetentionTtl == "" {
		ns.Config.WorkflowExecutionRetentionTtl = "720h"
	}
	if ns.Config.APSLimit == 0 {
		ns.Config.APSLimit = 400
	}
	if ns.NamespaceInfo.Region == "" {
		ns.NamespaceInfo.Region = "embedded"
	}
	ns.IsActive = ns.NamespaceInfo.State == "NAMESPACE_STATE_REGISTERED"
	if err := e.store.put("ns/"+ns.NamespaceInfo.Name, ns); err != nil {
		return err
	}
	e.emit(Event{Kind: "namespace.registered", Namespace: ns.NamespaceInfo.Name, Data: ns})
	return nil
}

func (e *engine) DescribeNamespace(name string) (*Namespace, bool, error) {
	var n Namespace
	ok, err := e.store.get("ns/"+name, &n)
	if !ok {
		return nil, false, err
	}
	return &n, true, err
}

func (e *engine) ListNamespaces() ([]Namespace, error) {
	return listInto[Namespace](e.store, "ns/")
}

// ── workflows ───────────────────────────────────────────────────────

func (e *engine) StartWorkflow(ns, workflowId, runId string, typ TypeRef, taskQueue string, input any) (*WorkflowExecution, error) {
	return e.startWorkflowWithRequestID(ns, workflowId, runId, typ, taskQueue, input, "")
}

// StartWorkflowWithRequestID is the idempotent form. When requestID is
// non-empty and a prior Start under the same (workflowID, requestID)
// landed, the existing execution is returned without a new run.
func (e *engine) StartWorkflowWithRequestID(ns, workflowId, runId string, typ TypeRef, taskQueue string, input any, requestID string) (*WorkflowExecution, error) {
	return e.startWorkflowWithRequestID(ns, workflowId, runId, typ, taskQueue, input, requestID)
}

func (e *engine) startWorkflowWithRequestID(ns, workflowId, runId string, typ TypeRef, taskQueue string, input any, requestID string) (*WorkflowExecution, error) {
	if _, ok, _ := e.DescribeNamespace(ns); !ok {
		return nil, fmt.Errorf("namespace %q not registered", ns)
	}
	if typ.Name == "" {
		return nil, fmt.Errorf("workflow type required")
	}
	if workflowId == "" {
		workflowId = strings.ToLower(typ.Name) + "-" + newRunId()[:8]
	}
	if requestID != "" {
		if existingRun, ok, _ := e.lookupIdempotency(ns, workflowId, requestID); ok {
			if wf, ok2, _ := e.DescribeWorkflow(ns, workflowId, existingRun); ok2 {
				return wf, nil
			}
		}
	}
	if runId == "" {
		runId = newRunId()
	}
	if taskQueue == "" {
		taskQueue = "default"
	}
	startTime := nowRFC3339()
	wf := WorkflowExecution{
		Execution: ExecutionRef{WorkflowId: workflowId, RunId: runId},
		Type:      typ,
		StartTime: startTime,
		Status:    "WORKFLOW_EXECUTION_STATUS_RUNNING",
		TaskQueue: taskQueue,
		Input:     input,
	}
	key := fmt.Sprintf("wf/%s/%s/%s", ns, workflowId, runId)
	if err := e.store.put(key, wf); err != nil {
		return nil, err
	}
	if requestID != "" {
		_ = e.store.put(fmt.Sprintf("idem/%s/%s/%s", ns, workflowId, requestID), runId)
	}
	if _, err := e.appendHistory(ns, workflowId, runId, "WORKFLOW_EXECUTION_STARTED", map[string]any{
		"workflowType": typ.Name,
		"taskQueue":    taskQueue,
		"input":        input,
	}); err != nil {
		return nil, err
	}
	// Reload to surface bumped HistoryLen.
	if reloaded, ok, _ := e.DescribeWorkflow(ns, workflowId, runId); ok {
		wf = *reloaded
	}
	if e.disp != nil {
		var inputBytes []byte
		if input != nil {
			inputBytes, _ = json.Marshal(input)
		}
		e.disp.EnqueueWorkflowTask(ns, taskQueue, workflowId, runId, typ.Name, inputBytes)
	}
	e.emit(Event{
		Kind:       "workflow.started",
		Namespace:  ns,
		WorkflowID: workflowId,
		RunID:      runId,
		Data:       wf,
	})
	return &wf, nil
}

// lookupIdempotency returns the runID previously associated with
// (workflowID, requestID), if any.
func (e *engine) lookupIdempotency(ns, workflowID, requestID string) (string, bool, error) {
	var runID string
	ok, err := e.store.get(fmt.Sprintf("idem/%s/%s/%s", ns, workflowID, requestID), &runID)
	return runID, ok, err
}

func (e *engine) DescribeWorkflow(ns, workflowId, runId string) (*WorkflowExecution, bool, error) {
	if runId != "" {
		var wf WorkflowExecution
		ok, err := e.store.get(fmt.Sprintf("wf/%s/%s/%s", ns, workflowId, runId), &wf)
		if !ok {
			return nil, false, err
		}
		return &wf, true, err
	}
	// runId not supplied — return the most recent execution under workflowId.
	rows, err := listInto[WorkflowExecution](e.store, fmt.Sprintf("wf/%s/%s/", ns, workflowId))
	if err != nil || len(rows) == 0 {
		return nil, false, err
	}
	latest := rows[0]
	for _, r := range rows[1:] {
		if r.StartTime > latest.StartTime {
			latest = r
		}
	}
	return &latest, true, nil
}

func (e *engine) ListWorkflows(ns string) ([]WorkflowExecution, error) {
	return listInto[WorkflowExecution](e.store, fmt.Sprintf("wf/%s/", ns))
}

func (e *engine) terminalTransition(ns, workflowId, runId, status, evKind, eventType string, attrs map[string]any) (*WorkflowExecution, error) {
	wf, ok, err := e.DescribeWorkflow(ns, workflowId, runId)
	if !ok {
		return nil, fmt.Errorf("workflow %s/%s/%s not found", ns, workflowId, runId)
	}
	if err != nil {
		return nil, err
	}
	if isTerminal(wf.Status) {
		return wf, nil
	}
	wf.Status = status
	wf.CloseTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, wf.Execution.WorkflowId, wf.Execution.RunId), wf); err != nil {
		return nil, err
	}
	if _, err := e.appendHistory(ns, wf.Execution.WorkflowId, wf.Execution.RunId, eventType, attrs); err != nil {
		return nil, err
	}
	// Reload to capture the bumped HistoryLen.
	wf, _, _ = e.DescribeWorkflow(ns, wf.Execution.WorkflowId, wf.Execution.RunId)
	e.emit(Event{
		Kind:       evKind,
		Namespace:  ns,
		WorkflowID: wf.Execution.WorkflowId,
		RunID:      wf.Execution.RunId,
		Data:       wf,
	})
	return wf, nil
}

func isTerminal(status string) bool {
	switch status {
	case "WORKFLOW_EXECUTION_STATUS_COMPLETED",
		"WORKFLOW_EXECUTION_STATUS_FAILED",
		"WORKFLOW_EXECUTION_STATUS_CANCELED",
		"WORKFLOW_EXECUTION_STATUS_TERMINATED",
		"WORKFLOW_EXECUTION_STATUS_TIMED_OUT",
		"WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW":
		return true
	}
	return false
}

func (e *engine) CancelWorkflow(ns, workflowId, runId string) (*WorkflowExecution, error) {
	return e.CancelWorkflowWithReason(ns, workflowId, runId, "", "")
}

// CancelWorkflowWithReason records WORKFLOW_EXECUTION_CANCEL_REQUESTED
// and transitions the workflow to CANCELING. If a worker is subscribed
// to the workflow's task queue, OpcodeDeliverCancelRequest is pushed
// and the engine waits for the worker to ack via a Canceled command on
// RespondWorkflowTaskCompleted. If no worker is subscribed, the cancel
// is auto-acked synchronously to CANCELED so the API converges in
// the absence of a cooperating worker.
func (e *engine) CancelWorkflowWithReason(ns, workflowId, runId, reason, identity string) (*WorkflowExecution, error) {
	wf, ok, err := e.DescribeWorkflow(ns, workflowId, runId)
	if !ok {
		return nil, fmt.Errorf("workflow %s/%s/%s not found", ns, workflowId, runId)
	}
	if err != nil {
		return nil, err
	}
	if isTerminal(wf.Status) {
		return wf, nil
	}
	if wf.Status == "WORKFLOW_EXECUTION_STATUS_CANCELING" {
		// Re-cancel while CANCELING: push again to subscribers but do
		// not append a second CANCEL_REQUESTED.
		if e.disp != nil {
			e.disp.PushCancelRequest(ns, wf.TaskQueue, wf.Execution.WorkflowId, wf.Execution.RunId, reason, identity)
		}
		return wf, nil
	}
	if _, err := e.appendHistory(ns, workflowId, runId, "WORKFLOW_EXECUTION_CANCEL_REQUESTED", map[string]any{
		"reason":   reason,
		"identity": identity,
	}); err != nil {
		return nil, err
	}
	wf, _, _ = e.DescribeWorkflow(ns, workflowId, runId)
	subscribed := e.disp != nil && e.disp.HasSubscribers(ns, wf.TaskQueue, kindWorkflow)
	if !subscribed {
		// No worker — auto-ack to CANCELED synchronously.
		return e.terminalTransition(ns, workflowId, runId, "WORKFLOW_EXECUTION_STATUS_CANCELED", "workflow.canceled", "WORKFLOW_EXECUTION_CANCELED", map[string]any{
			"reason":   reason,
			"identity": identity,
		})
	}
	wf.Status = "WORKFLOW_EXECUTION_STATUS_CANCELING"
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, wf.Execution.WorkflowId, wf.Execution.RunId), wf); err != nil {
		return nil, err
	}
	if e.cancelling != nil {
		e.cancelling.mark(ns, wf.Execution.WorkflowId, wf.Execution.RunId)
	}
	e.disp.PushCancelRequest(ns, wf.TaskQueue, wf.Execution.WorkflowId, wf.Execution.RunId, reason, identity)
	e.emit(Event{
		Kind:       "workflow.cancel_requested",
		Namespace:  ns,
		WorkflowID: wf.Execution.WorkflowId,
		RunID:      wf.Execution.RunId,
		Data:       wf,
	})
	return wf, nil
}

// AckCanceled finalizes a CANCELING workflow → CANCELED. Idempotent
// against terminal states; called from the respond-workflow-task ack
// path and from the cancel sweeper.
func (e *engine) AckCanceled(ns, workflowID, runID, reason, identity string) (*WorkflowExecution, error) {
	if e.cancelling != nil {
		e.cancelling.clear(ns, workflowID, runID)
	}
	return e.terminalTransition(ns, workflowID, runID, "WORKFLOW_EXECUTION_STATUS_CANCELED", "workflow.canceled", "WORKFLOW_EXECUTION_CANCELED", map[string]any{
		"reason":   reason,
		"identity": identity,
	})
}

// sweepCanceling auto-acks workflows that have been CANCELING longer
// than cancelTimeout. Called periodically by runScheduler.
func (e *engine) sweepCanceling() {
	if e.cancelling == nil {
		return
	}
	for _, k := range e.cancelling.expired() {
		parts := strings.SplitN(k, "|", 3)
		if len(parts) != 3 {
			e.cancelling.mu.Lock()
			delete(e.cancelling.at, k)
			e.cancelling.mu.Unlock()
			continue
		}
		_, _ = e.AckCanceled(parts[0], parts[1], parts[2], "sweep:cancel-timeout", "engine")
	}
}

func (e *engine) TerminateWorkflow(ns, workflowId, runId string) (*WorkflowExecution, error) {
	return e.TerminateWorkflowWithReason(ns, workflowId, runId, "", "")
}

func (e *engine) TerminateWorkflowWithReason(ns, workflowId, runId, reason, identity string) (*WorkflowExecution, error) {
	return e.terminalTransition(ns, workflowId, runId, "WORKFLOW_EXECUTION_STATUS_TERMINATED", "workflow.terminated", "WORKFLOW_EXECUTION_TERMINATED", map[string]any{
		"reason":   reason,
		"identity": identity,
	})
}

func (e *engine) SignalWorkflow(ns, workflowId, runId, name string, payload any) error {
	return e.signalWorkflow(ns, workflowId, runId, name, payload, "")
}

func (e *engine) signalWorkflow(ns, workflowId, runId, name string, payload any, identity string) error {
	wf, ok, err := e.DescribeWorkflow(ns, workflowId, runId)
	if !ok || err != nil {
		return fmt.Errorf("workflow not found")
	}
	if isTerminal(wf.Status) {
		return fmt.Errorf("workflow not running: status=%s", wf.Status)
	}
	if _, err := e.appendHistory(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "WORKFLOW_EXECUTION_SIGNALED", map[string]any{
		"signalName": name,
		"input":      payload,
		"identity":   identity,
	}); err != nil {
		return err
	}
	wf, _, _ = e.DescribeWorkflow(ns, wf.Execution.WorkflowId, wf.Execution.RunId)
	e.emit(Event{
		Kind:       "workflow.signaled",
		Namespace:  ns,
		WorkflowID: wf.Execution.WorkflowId,
		RunID:      wf.Execution.RunId,
		Data:       map[string]any{"signal": name, "input": payload, "history_length": wf.HistoryLen},
	})
	return nil
}

// SignalWithStartWorkflow signals an existing running workflow under
// workflowId; if none is running, starts a fresh one and signals that.
// The (start, signal) pair is atomic from the caller's view.
func (e *engine) SignalWithStartWorkflow(ns, workflowId, runId string, typ TypeRef, taskQueue string, input any, signalName string, signalPayload any, requestID string) (*WorkflowExecution, error) {
	if existing, ok, _ := e.DescribeWorkflow(ns, workflowId, ""); ok && !isTerminal(existing.Status) {
		if err := e.SignalWorkflow(ns, existing.Execution.WorkflowId, existing.Execution.RunId, signalName, signalPayload); err != nil {
			return nil, err
		}
		return existing, nil
	}
	wf, err := e.startWorkflowWithRequestID(ns, workflowId, runId, typ, taskQueue, input, requestID)
	if err != nil {
		return nil, err
	}
	if err := e.SignalWorkflow(ns, wf.Execution.WorkflowId, wf.Execution.RunId, signalName, signalPayload); err != nil {
		return nil, err
	}
	wf, _, _ = e.DescribeWorkflow(ns, wf.Execution.WorkflowId, wf.Execution.RunId)
	return wf, nil
}

// ── history ────────────────────────────────────────────────────────

// appendHistory writes a HistoryEvent to durable storage and bumps the
// workflow's HistoryLen. EventId is monotonic per (ns, workflowID,
// runID). Returns the appended event.
func (e *engine) appendHistory(ns, workflowID, runID, eventType string, attrs map[string]any) (*HistoryEvent, error) {
	wf, ok, err := e.DescribeWorkflow(ns, workflowID, runID)
	if !ok {
		return nil, fmt.Errorf("workflow %s/%s/%s not found", ns, workflowID, runID)
	}
	if err != nil {
		return nil, err
	}
	wf.HistoryLen++
	ev := &HistoryEvent{
		EventId:    wf.HistoryLen,
		EventTime:  nowRFC3339(),
		EventType:  eventType,
		Attributes: attrs,
	}
	key := fmt.Sprintf("wfh/%s/%s/%s/%020d", ns, wf.Execution.WorkflowId, wf.Execution.RunId, ev.EventId)
	if err := e.store.put(key, ev); err != nil {
		return nil, err
	}
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, wf.Execution.WorkflowId, wf.Execution.RunId), wf); err != nil {
		return nil, err
	}
	return ev, nil
}

// GetWorkflowHistory returns events in [afterEventID+1, +pageSize] in
// the requested order. afterEventID==0 starts from the beginning.
// pageSize<=0 defaults to 100.
func (e *engine) GetWorkflowHistory(ns, workflowID, runID string, afterEventID int64, pageSize int, reverse bool) ([]HistoryEvent, int64, error) {
	if pageSize <= 0 {
		pageSize = 100
	}
	wf, ok, err := e.DescribeWorkflow(ns, workflowID, runID)
	if !ok {
		return nil, 0, fmt.Errorf("workflow not found")
	}
	if err != nil {
		return nil, 0, err
	}
	all, err := listInto[HistoryEvent](e.store, fmt.Sprintf("wfh/%s/%s/%s/", ns, wf.Execution.WorkflowId, wf.Execution.RunId))
	if err != nil {
		return nil, 0, err
	}
	if reverse {
		// Reverse in place.
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

// QueryWorkflow dispatches the query through the dispatcher push path:
// if a worker is subscribed to the workflow's task queue, the query is
// pushed via OpcodeDeliverQuery and the call blocks for the worker's
// OpcodeRespondQuery (default 5s timeout, ctx-overrideable). Built-in
// synthetic queries (__stack_trace, __workflow_metadata) are handled
// engine-side without round-tripping a worker. ErrNoWorkersSubscribed
// is returned when the workflow has a task queue with no subscribers.
func (e *engine) QueryWorkflow(ns, workflowID, runID, queryType string, args any) (any, error) {
	return e.QueryWorkflowCtx(context.Background(), ns, workflowID, runID, queryType, args)
}

func (e *engine) QueryWorkflowCtx(ctx context.Context, ns, workflowID, runID, queryType string, args any) (any, error) {
	wf, ok, err := e.DescribeWorkflow(ns, workflowID, runID)
	if !ok {
		return nil, fmt.Errorf("workflow not found")
	}
	if err != nil {
		return nil, err
	}
	switch queryType {
	case "__stack_trace":
		return map[string]any{"stack": ""}, nil
	case "__workflow_metadata":
		return map[string]any{
			"workflowType": wf.Type.Name,
			"status":       wf.Status,
			"historyLen":   wf.HistoryLen,
		}, nil
	}
	if e.disp == nil {
		return nil, ErrNoWorkersSubscribed
	}
	var argBytes []byte
	if args != nil {
		argBytes, _ = json.Marshal(args)
	}
	token, ch, err := e.disp.PushQuery(ns, wf.TaskQueue, wf.Execution.WorkflowId, wf.Execution.RunId, queryType, argBytes)
	if err != nil {
		return nil, err
	}
	timeout := 5 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 && d < timeout {
			timeout = d
		}
	}
	select {
	case res := <-ch:
		if res.errMsg != "" {
			return nil, fmt.Errorf("query failed: %s", res.errMsg)
		}
		var out any
		if len(res.result) > 0 {
			if err := json.Unmarshal(res.result, &out); err != nil {
				return string(res.result), nil
			}
		}
		return out, nil
	case <-ctx.Done():
		e.disp.CancelQuery(token)
		return nil, ctx.Err()
	case <-time.After(timeout):
		e.disp.CancelQuery(token)
		return nil, fmt.Errorf("query timeout after %s", timeout)
	}
}

// ResetWorkflow forks a workflow's history at eventID, writes a fresh
// run starting from a synthetic START event and the truncated history,
// and links the runs via wf.ResetPoints (stored as a memo key).
func (e *engine) ResetWorkflow(ns, workflowID, runID string, eventID int64, reason, identity string) (*WorkflowExecution, error) {
	src, ok, err := e.DescribeWorkflow(ns, workflowID, runID)
	if !ok {
		return nil, fmt.Errorf("workflow not found")
	}
	if err != nil {
		return nil, err
	}
	if eventID < 1 || eventID > src.HistoryLen {
		return nil, fmt.Errorf("invalid eventId %d (history has %d events)", eventID, src.HistoryLen)
	}
	all, err := listInto[HistoryEvent](e.store, fmt.Sprintf("wfh/%s/%s/%s/", ns, src.Execution.WorkflowId, src.Execution.RunId))
	if err != nil {
		return nil, err
	}
	newRun := newRunId()
	newWf := WorkflowExecution{
		Execution: ExecutionRef{WorkflowId: src.Execution.WorkflowId, RunId: newRun},
		Type:      src.Type,
		StartTime: nowRFC3339(),
		Status:    "WORKFLOW_EXECUTION_STATUS_RUNNING",
		TaskQueue: src.TaskQueue,
		Input:     src.Input,
		Memo: map[string]any{
			"resetFromRunId":  src.Execution.RunId,
			"resetFromEventId": eventID,
			"resetReason":     reason,
			"resetIdentity":   identity,
		},
	}
	// Persist the new execution shell first so appendHistory finds it.
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, newWf.Execution.WorkflowId, newWf.Execution.RunId), newWf); err != nil {
		return nil, err
	}
	// Replay events [1..eventID] under the new runID with the same
	// EventId numbering so the fork is identical up to the cut.
	for _, ev := range all {
		if ev.EventId > eventID {
			break
		}
		key := fmt.Sprintf("wfh/%s/%s/%s/%020d", ns, newWf.Execution.WorkflowId, newWf.Execution.RunId, ev.EventId)
		if err := e.store.put(key, ev); err != nil {
			return nil, err
		}
	}
	newWf.HistoryLen = eventID
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, newWf.Execution.WorkflowId, newWf.Execution.RunId), newWf); err != nil {
		return nil, err
	}
	if _, err := e.appendHistory(ns, newWf.Execution.WorkflowId, newWf.Execution.RunId, "WORKFLOW_EXECUTION_RESET", map[string]any{
		"baseRunId":  src.Execution.RunId,
		"eventId":    eventID,
		"reason":     reason,
		"identity":   identity,
	}); err != nil {
		return nil, err
	}
	// Mark the source as terminated-by-reset; mirror Temporal semantics.
	src.Status = "WORKFLOW_EXECUTION_STATUS_TERMINATED"
	src.CloseTime = nowRFC3339()
	_ = e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, src.Execution.WorkflowId, src.Execution.RunId), src)
	if e.disp != nil {
		var inputBytes []byte
		if newWf.Input != nil {
			inputBytes, _ = json.Marshal(newWf.Input)
		}
		e.disp.EnqueueWorkflowTask(ns, newWf.TaskQueue, newWf.Execution.WorkflowId, newWf.Execution.RunId, newWf.Type.Name, inputBytes)
	}
	out, _, _ := e.DescribeWorkflow(ns, newWf.Execution.WorkflowId, newWf.Execution.RunId)
	e.emit(Event{
		Kind:       "workflow.reset",
		Namespace:  ns,
		WorkflowID: out.Execution.WorkflowId,
		RunID:      out.Execution.RunId,
		Data:       out,
	})
	return out, nil
}

// ListWorkflowExecutions filters ListWorkflows by the supplied
// visibility query. The grammar:
//
//	expr   := orExpr
//	orExpr := andExpr ('OR' andExpr)*
//	andExpr:= notExpr ('AND' notExpr)*
//	notExpr:= 'NOT' notExpr | atom
//	atom   := '(' expr ')' | comparison
//	comparison := ident op value | ident 'IN' '(' values ')'
//	            | ident 'NOT' 'IN' '(' values ')'
//	            | ident 'BETWEEN' value 'AND' value
//	op     := '=' | '!=' | '>' | '<' | '>=' | '<='
//
// Empty query → no predicates → all rows match.
func (e *engine) ListWorkflowExecutions(ns, query string) ([]WorkflowExecution, error) {
	rows, err := e.ListWorkflows(ns)
	if err != nil {
		return nil, err
	}
	expr, err := parseVisibilityQuery(query)
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowExecution, 0, len(rows))
	for i := range rows {
		if expr == nil || expr.eval(&rows[i]) {
			out = append(out, rows[i])
		}
	}
	return out, nil
}

// visNode is the evaluator interface; concrete types implement eval.
type visNode interface {
	eval(*WorkflowExecution) bool
}

type visAnd struct{ a, b visNode }

func (n visAnd) eval(wf *WorkflowExecution) bool { return n.a.eval(wf) && n.b.eval(wf) }

type visOr struct{ a, b visNode }

func (n visOr) eval(wf *WorkflowExecution) bool { return n.a.eval(wf) || n.b.eval(wf) }

type visNot struct{ a visNode }

func (n visNot) eval(wf *WorkflowExecution) bool { return !n.a.eval(wf) }

type visCmp struct {
	field string
	op    string // = != > < >= <= IN NOT_IN BETWEEN
	value string
	list  []string // for IN / NOT_IN
	low   string   // for BETWEEN
	high  string   // for BETWEEN
}

func (n visCmp) eval(wf *WorkflowExecution) bool { return matchCmp(wf, n) }

// ── tokenizer ───────────────────────────────────────────────────────

type vtok struct {
	kind string // ident | str | num | sym | kw
	val  string
}

func vtokenize(q string) ([]vtok, error) {
	var out []vtok
	i := 0
	for i < len(q) {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n':
			i++
		case c == '(' || c == ')' || c == ',':
			out = append(out, vtok{"sym", string(c)})
			i++
		case c == '=':
			out = append(out, vtok{"sym", "="})
			i++
		case c == '!':
			if i+1 < len(q) && q[i+1] == '=' {
				out = append(out, vtok{"sym", "!="})
				i += 2
			} else {
				return nil, fmt.Errorf("unexpected '!' at %d", i)
			}
		case c == '>':
			if i+1 < len(q) && q[i+1] == '=' {
				out = append(out, vtok{"sym", ">="})
				i += 2
			} else {
				out = append(out, vtok{"sym", ">"})
				i++
			}
		case c == '<':
			if i+1 < len(q) && q[i+1] == '=' {
				out = append(out, vtok{"sym", "<="})
				i += 2
			} else {
				out = append(out, vtok{"sym", "<"})
				i++
			}
		case c == '\'' || c == '"':
			quote := c
			i++
			start := i
			for i < len(q) && q[i] != quote {
				i++
			}
			if i >= len(q) {
				return nil, fmt.Errorf("unterminated string starting at %d", start-1)
			}
			out = append(out, vtok{"str", q[start:i]})
			i++
		case (c >= '0' && c <= '9') || c == '-' || c == '+':
			start := i
			i++
			for i < len(q) && ((q[i] >= '0' && q[i] <= '9') || q[i] == '.') {
				i++
			}
			out = append(out, vtok{"num", q[start:i]})
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_':
			start := i
			for i < len(q) && ((q[i] >= 'A' && q[i] <= 'Z') || (q[i] >= 'a' && q[i] <= 'z') || (q[i] >= '0' && q[i] <= '9') || q[i] == '_') {
				i++
			}
			word := q[start:i]
			upper := strings.ToUpper(word)
			switch upper {
			case "AND", "OR", "NOT", "IN", "BETWEEN":
				out = append(out, vtok{"kw", upper})
			default:
				out = append(out, vtok{"ident", word})
			}
		default:
			return nil, fmt.Errorf("unexpected char %q at %d", c, i)
		}
	}
	return out, nil
}

// ── parser ─────────────────────────────────────────────────────────

type vparser struct {
	toks []vtok
	pos  int
}

func (p *vparser) peek() *vtok {
	if p.pos >= len(p.toks) {
		return nil
	}
	return &p.toks[p.pos]
}

func (p *vparser) eat() *vtok {
	t := p.peek()
	if t != nil {
		p.pos++
	}
	return t
}

func (p *vparser) expectSym(s string) error {
	t := p.eat()
	if t == nil || t.kind != "sym" || t.val != s {
		return fmt.Errorf("expected %q", s)
	}
	return nil
}

func parseVisibilityQuery(q string) (visNode, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	toks, err := vtokenize(q)
	if err != nil {
		return nil, err
	}
	p := &vparser{toks: toks}
	n, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.pos != len(toks) {
		return nil, fmt.Errorf("trailing tokens at pos %d", p.pos)
	}
	return n, nil
}

func (p *vparser) parseOr() (visNode, error) {
	a, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || t.kind != "kw" || t.val != "OR" {
			return a, nil
		}
		p.eat()
		b, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		a = visOr{a, b}
	}
}

func (p *vparser) parseAnd() (visNode, error) {
	a, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		t := p.peek()
		if t == nil || t.kind != "kw" || t.val != "AND" {
			return a, nil
		}
		p.eat()
		b, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		a = visAnd{a, b}
	}
}

func (p *vparser) parseNot() (visNode, error) {
	t := p.peek()
	if t != nil && t.kind == "kw" && t.val == "NOT" {
		// "NOT IN" only valid as part of an IN comparison; we look ahead
		// for an ident before consuming.
		if p.pos+1 < len(p.toks) && p.toks[p.pos+1].kind == "ident" {
			// Could still be "NOT atom"; only treat as NOT IN if the
			// ident is followed by IN — but that's parsed inside the
			// atom's comparison. So here, NOT followed by ident is a
			// NOT-of-comparison.
		}
		p.eat()
		inner, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return visNot{inner}, nil
	}
	return p.parseAtom()
}

func (p *vparser) parseAtom() (visNode, error) {
	t := p.peek()
	if t == nil {
		return nil, fmt.Errorf("unexpected end of query")
	}
	if t.kind == "sym" && t.val == "(" {
		p.eat()
		n, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if err := p.expectSym(")"); err != nil {
			return nil, err
		}
		return n, nil
	}
	return p.parseComparison()
}

func (p *vparser) parseComparison() (visNode, error) {
	id := p.eat()
	if id == nil || id.kind != "ident" {
		return nil, fmt.Errorf("expected identifier, got %+v", id)
	}
	op := p.eat()
	if op == nil {
		return nil, fmt.Errorf("expected operator after %q", id.val)
	}
	// IN / NOT IN / BETWEEN.
	if op.kind == "kw" {
		switch op.val {
		case "IN":
			vs, err := p.parseValueList()
			if err != nil {
				return nil, err
			}
			return visCmp{field: id.val, op: "IN", list: vs}, nil
		case "NOT":
			n2 := p.eat()
			if n2 == nil || n2.kind != "kw" || n2.val != "IN" {
				return nil, fmt.Errorf("expected IN after NOT")
			}
			vs, err := p.parseValueList()
			if err != nil {
				return nil, err
			}
			return visCmp{field: id.val, op: "NOT_IN", list: vs}, nil
		case "BETWEEN":
			lo, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			and := p.eat()
			if and == nil || and.kind != "kw" || and.val != "AND" {
				return nil, fmt.Errorf("expected AND in BETWEEN")
			}
			hi, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			return visCmp{field: id.val, op: "BETWEEN", low: lo, high: hi}, nil
		}
		return nil, fmt.Errorf("unexpected keyword %q", op.val)
	}
	if op.kind != "sym" {
		return nil, fmt.Errorf("expected operator, got %+v", op)
	}
	switch op.val {
	case "=", "!=", ">", "<", ">=", "<=":
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		return visCmp{field: id.val, op: op.val, value: v}, nil
	}
	return nil, fmt.Errorf("unexpected operator %q", op.val)
}

func (p *vparser) parseValue() (string, error) {
	t := p.eat()
	if t == nil {
		return "", fmt.Errorf("expected value")
	}
	switch t.kind {
	case "str", "num", "ident":
		return t.val, nil
	}
	return "", fmt.Errorf("expected value, got %+v", t)
}

func (p *vparser) parseValueList() ([]string, error) {
	if err := p.expectSym("("); err != nil {
		return nil, err
	}
	var out []string
	for {
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		t := p.peek()
		if t != nil && t.kind == "sym" && t.val == "," {
			p.eat()
			continue
		}
		break
	}
	if err := p.expectSym(")"); err != nil {
		return nil, err
	}
	return out, nil
}

// ── evaluator ───────────────────────────────────────────────────────

func matchCmp(wf *WorkflowExecution, c visCmp) bool {
	got := fieldValue(wf, c.field)
	if c.field == "ExecutionStatus" {
		got = normalizeStatus(got)
	}
	switch c.op {
	case "=":
		return got == compareValue(c.field, c.value)
	case "!=":
		return got != compareValue(c.field, c.value)
	case ">":
		return got > compareValue(c.field, c.value)
	case "<":
		return got < compareValue(c.field, c.value)
	case ">=":
		return got >= compareValue(c.field, c.value)
	case "<=":
		return got <= compareValue(c.field, c.value)
	case "IN":
		for _, v := range c.list {
			if got == compareValue(c.field, v) {
				return true
			}
		}
		return false
	case "NOT_IN":
		for _, v := range c.list {
			if got == compareValue(c.field, v) {
				return false
			}
		}
		return true
	case "BETWEEN":
		lo := compareValue(c.field, c.low)
		hi := compareValue(c.field, c.high)
		return got >= lo && got <= hi
	}
	return false
}

func fieldValue(wf *WorkflowExecution, f string) string {
	switch f {
	case "WorkflowType":
		return wf.Type.Name
	case "WorkflowId":
		return wf.Execution.WorkflowId
	case "RunId":
		return wf.Execution.RunId
	case "ExecutionStatus":
		return wf.Status
	case "StartTime":
		return wf.StartTime
	case "CloseTime":
		return wf.CloseTime
	case "TaskQueue":
		return wf.TaskQueue
	}
	if v, ok := wf.SearchAttrs[f]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

func compareValue(field, want string) string {
	if field == "ExecutionStatus" {
		return normalizeStatus(want)
	}
	return want
}

func normalizeStatus(s string) string {
	s = strings.ToUpper(s)
	if !strings.HasPrefix(s, "WORKFLOW_EXECUTION_STATUS_") {
		s = "WORKFLOW_EXECUTION_STATUS_" + s
	}
	return s
}

// ── schedules ───────────────────────────────────────────────────────

func (e *engine) CreateSchedule(s Schedule) error {
	if s.ScheduleId == "" {
		return fmt.Errorf("scheduleId required")
	}
	if s.Namespace == "" {
		return fmt.Errorf("namespace required")
	}
	s.Info.CreateTime = nowRFC3339()
	if next, ok := nextScheduleFire(s, time.Now().UTC()); ok {
		s.Info.NextActionTime = next.UTC().Format(time.RFC3339)
	}
	if err := e.store.put(fmt.Sprintf("sc/%s/%s", s.Namespace, s.ScheduleId), s); err != nil {
		return err
	}
	e.emit(Event{Kind: "schedule.created", Namespace: s.Namespace, ScheduleID: s.ScheduleId, Data: s})
	return nil
}

func (e *engine) DescribeSchedule(ns, id string) (*Schedule, bool, error) {
	var s Schedule
	ok, err := e.store.get(fmt.Sprintf("sc/%s/%s", ns, id), &s)
	if !ok {
		return nil, false, err
	}
	return &s, true, err
}

func (e *engine) ListSchedules(ns string) ([]Schedule, error) {
	return listInto[Schedule](e.store, fmt.Sprintf("sc/%s/", ns))
}

func (e *engine) DeleteSchedule(ns, id string) error {
	return e.store.del(fmt.Sprintf("sc/%s/%s", ns, id))
}

func (e *engine) PauseSchedule(ns, id string, paused bool, note string) error {
	s, ok, err := e.DescribeSchedule(ns, id)
	if !ok || err != nil {
		return fmt.Errorf("schedule not found")
	}
	s.State.Paused = paused
	s.State.Note = note
	s.Info.UpdateTime = nowRFC3339()
	return e.store.put(fmt.Sprintf("sc/%s/%s", ns, id), s)
}

// ── search attributes ───────────────────────────────────────────────

var validSearchAttrTypes = map[string]bool{
	"Keyword": true, "Text": true, "Int": true, "Double": true,
	"Bool": true, "Datetime": true, "KeywordList": true,
}

// reservedSearchAttrNames are the system fields the visibility evaluator
// interprets directly. Allowing user code to register a custom attribute
// with one of these names would let it shadow the real workflow field
// (e.g. ExecutionStatus="*") and silently widen list results. Names
// starting with "Temporal" are reserved for upstream-compat fields.
var reservedSearchAttrNames = map[string]bool{
	"WorkflowType": true, "WorkflowId": true, "RunId": true,
	"ExecutionStatus": true, "StartTime": true, "CloseTime": true,
	"TaskQueue": true,
}

func (e *engine) AddSearchAttribute(ns string, sa SearchAttribute) error {
	if _, ok, _ := e.DescribeNamespace(ns); !ok {
		return fmt.Errorf("namespace %q not registered", ns)
	}
	if sa.Name == "" {
		return fmt.Errorf("search attribute name required")
	}
	if reservedSearchAttrNames[sa.Name] || strings.HasPrefix(sa.Name, "Temporal") {
		return fmt.Errorf("search attribute %q is reserved", sa.Name)
	}
	if !validSearchAttrTypes[sa.Type] {
		return fmt.Errorf("invalid search attribute type %q", sa.Type)
	}
	key := fmt.Sprintf("sa/%s/%s", ns, sa.Name)
	var existing SearchAttribute
	if ok, _ := e.store.get(key, &existing); ok {
		return fmt.Errorf("search attribute %q already exists", sa.Name)
	}
	return e.store.put(key, sa)
}

func (e *engine) ListSearchAttributes(ns string) ([]SearchAttribute, error) {
	return listInto[SearchAttribute](e.store, fmt.Sprintf("sa/%s/", ns))
}

// ── namespace metadata ──────────────────────────────────────────────

// NamespaceMetadataPatch carries the fields a metadata update may set.
// Empty string fields are ignored; non-nil maps fully replace.
type NamespaceMetadataPatch struct {
	Description             *string            `json:"description,omitempty"`
	OwnerEmail              *string            `json:"ownerEmail,omitempty"`
	Retention               *string            `json:"retention,omitempty"`
	HistoryArchivalState    *string            `json:"historyArchivalState,omitempty"`
	HistoryArchivalUri      *string            `json:"historyArchivalUri,omitempty"`
	VisibilityArchivalState *string            `json:"visibilityArchivalState,omitempty"`
	VisibilityArchivalUri   *string            `json:"visibilityArchivalUri,omitempty"`
	CustomData              map[string]string  `json:"customData,omitempty"`
}

func (e *engine) UpdateNamespaceMetadata(name string, p NamespaceMetadataPatch) (*Namespace, error) {
	n, ok, err := e.DescribeNamespace(name)
	if !ok {
		return nil, fmt.Errorf("namespace %q not registered", name)
	}
	if err != nil {
		return nil, err
	}
	if p.Description != nil {
		n.NamespaceInfo.Description = *p.Description
	}
	if p.OwnerEmail != nil {
		n.NamespaceInfo.OwnerEmail = *p.OwnerEmail
	}
	if p.Retention != nil {
		n.Config.WorkflowExecutionRetentionTtl = *p.Retention
	}
	if p.HistoryArchivalState != nil {
		n.Config.HistoryArchivalState = *p.HistoryArchivalState
	}
	if p.HistoryArchivalUri != nil {
		n.Config.HistoryArchivalUri = *p.HistoryArchivalUri
	}
	if p.VisibilityArchivalState != nil {
		n.Config.VisibilityArchivalState = *p.VisibilityArchivalState
	}
	if p.VisibilityArchivalUri != nil {
		n.Config.VisibilityArchivalUri = *p.VisibilityArchivalUri
	}
	if p.CustomData != nil {
		n.Config.CustomData = p.CustomData
	}
	if err := e.store.put("ns/"+name, *n); err != nil {
		return nil, err
	}
	return n, nil
}

// ── workflow user metadata ──────────────────────────────────────────

func (e *engine) UpdateWorkflowMetadata(ns, workflowID, runID string, m WorkflowUserMetadata) (*WorkflowExecution, error) {
	wf, ok, err := e.DescribeWorkflow(ns, workflowID, runID)
	if !ok {
		return nil, fmt.Errorf("workflow not found")
	}
	if err != nil {
		return nil, err
	}
	prev := wf.UserMetadata
	cur := WorkflowUserMetadata{}
	if prev != nil {
		cur = *prev
	}
	if m.Summary != "" {
		cur.Summary = m.Summary
	}
	if m.Details != "" {
		cur.Details = m.Details
	}
	cur.UpdatedBy = m.UpdatedBy
	cur.UpdatedAt = nowRFC3339()
	wf.UserMetadata = &cur
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, wf.Execution.WorkflowId, wf.Execution.RunId), wf); err != nil {
		return nil, err
	}
	return wf, nil
}

// ── batches ─────────────────────────────────────────────────────────

func (e *engine) StartBatch(b BatchOperation) (*BatchOperation, error) {
	if b.BatchId == "" {
		b.BatchId = "batch-" + newRunId()[:12]
	}
	b.State = "BATCH_OPERATION_STATE_RUNNING"
	b.StartTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("bt/%s/%s", b.Namespace, b.BatchId), b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (e *engine) DescribeBatch(ns, id string) (*BatchOperation, bool, error) {
	var b BatchOperation
	ok, err := e.store.get(fmt.Sprintf("bt/%s/%s", ns, id), &b)
	if !ok {
		return nil, false, err
	}
	return &b, true, err
}

func (e *engine) ListBatches(ns string) ([]BatchOperation, error) {
	return listInto[BatchOperation](e.store, fmt.Sprintf("bt/%s/", ns))
}

// TerminateBatch marks a batch as terminated and appends a terminate
// event to the namespace's batch event log. Idempotent: if the batch is
// already terminated, returns the existing record without re-emitting.
func (e *engine) TerminateBatch(ns, batchID, reason, identity string) (*BatchOperation, error) {
	b, ok, err := e.DescribeBatch(ns, batchID)
	if !ok {
		return nil, fmt.Errorf("batch not found")
	}
	if err != nil {
		return nil, err
	}
	if b.State == "BATCH_OPERATION_STATE_TERMINATED" {
		return b, nil
	}
	b.State = "BATCH_OPERATION_STATE_TERMINATED"
	b.CloseTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("bt/%s/%s", ns, b.BatchId), b); err != nil {
		return nil, err
	}
	e.emit(Event{
		Kind:      "batch.terminated",
		Namespace: ns,
		BatchID:   b.BatchId,
		Data: map[string]any{
			"reason":   reason,
			"identity": identity,
		},
	})
	return b, nil
}

// ── deployments ─────────────────────────────────────────────────────

// CreateDeployment registers a new deployment series. Returns 409-style
// error if the name already exists in the namespace. Caller must check
// for "already exists" substring to map to HTTP 409.
func (e *engine) CreateDeployment(ns, name, description, ownerEmail, defaultCompute string) (*Deployment, error) {
	if name == "" {
		return nil, fmt.Errorf("name required")
	}
	key := fmt.Sprintf("dp/%s/%s", ns, name)
	var existing Deployment
	if ok, _ := e.store.get(key, &existing); ok {
		return nil, fmt.Errorf("deployment %q already exists", name)
	}
	d := Deployment{
		Name:           name,
		Namespace:      ns,
		Description:    description,
		OwnerEmail:     ownerEmail,
		DefaultCompute: defaultCompute,
		Versions:       []DeploymentVersion{},
		CreateTime:     nowRFC3339(),
	}
	if err := e.store.put(key, d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (e *engine) ListDeployments(ns string) ([]Deployment, error) {
	return listInto[Deployment](e.store, fmt.Sprintf("dp/%s/", ns))
}

// DescribeDeployment returns a single deployment by name.
func (e *engine) DescribeDeployment(ns, name string) (*Deployment, bool, error) {
	var d Deployment
	ok, err := e.store.get(fmt.Sprintf("dp/%s/%s", ns, name), &d)
	if !ok {
		return nil, false, err
	}
	return &d, true, err
}

// ── nexus ───────────────────────────────────────────────────────────

func (e *engine) CreateNexusEndpoint(n NexusEndpoint) error {
	if n.Name == "" {
		return fmt.Errorf("nexus endpoint name required")
	}
	n.CreateTime = nowRFC3339()
	return e.store.put(fmt.Sprintf("nx/%s/%s", n.Namespace, n.Name), n)
}

func (e *engine) ListNexusEndpoints(ns string) ([]NexusEndpoint, error) {
	return listInto[NexusEndpoint](e.store, fmt.Sprintf("nx/%s/", ns))
}

// ListAllNexusEndpoints aggregates Nexus endpoints across all namespaces.
// Used by the cross-namespace top-level UI route.
func (e *engine) ListAllNexusEndpoints() ([]NexusEndpoint, error) {
	return listInto[NexusEndpoint](e.store, "nx/")
}

// ── identities ──────────────────────────────────────────────────────

func (e *engine) GrantIdentity(i Identity) error {
	if i.Email == "" || i.Namespace == "" {
		return fmt.Errorf("email + namespace required")
	}
	i.GrantTime = nowRFC3339()
	return e.store.put(fmt.Sprintf("id/%s/%s", i.Namespace, i.Email), i)
}

func (e *engine) ListIdentities(ns string) ([]Identity, error) {
	return listInto[Identity](e.store, fmt.Sprintf("id/%s/", ns))
}

// ── cron sweeper ────────────────────────────────────────────────────

// runScheduler ticks every 5s and fires due schedules through StartWorkflow.
// Cron specs are parsed via robfig/cron with seconds-optional 5/6-field
// syntax; first invalid spec is skipped without aborting the sweep.
func (e *engine) runScheduler(stop <-chan struct{}) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			_ = e.sweepSchedules()
			e.sweepCanceling()
		}
	}
}

func (e *engine) sweepSchedules() error {
	now := time.Now().UTC()
	return e.store.list("sc/", func(_ string, body []byte) error {
		var s Schedule
		if err := unmarshal(body, &s); err != nil {
			return err
		}
		if s.State.Paused {
			return nil
		}
		next, ok := nextScheduleFire(s, anchorTime(s, now))
		if !ok {
			return nil
		}
		// Fire any pending actions whose next-time is in the past.
		fired := false
		for !next.After(now) {
			if _, err := e.StartWorkflow(s.Namespace, "", "", s.Action.WorkflowType, s.Action.TaskQueue, s.Action.Input); err != nil {
				return err
			}
			s.Info.ActionCount++
			s.Info.UpdateTime = next.UTC().Format(time.RFC3339)
			fired = true
			n2, ok2 := nextScheduleFire(s, next)
			if !ok2 {
				next = time.Time{}
				break
			}
			next = n2
		}
		if !fired && s.Info.NextActionTime == next.UTC().Format(time.RFC3339) {
			return nil
		}
		if !next.IsZero() {
			s.Info.NextActionTime = next.UTC().Format(time.RFC3339)
		}
		return e.store.put(fmt.Sprintf("sc/%s/%s", s.Namespace, s.ScheduleId), s)
	})
}

// anchorTime returns the timestamp from which the next cron firing
// should be computed. Uses the last update time if present, otherwise
// the create time, otherwise now.
func anchorTime(s Schedule, fallback time.Time) time.Time {
	for _, ts := range []string{s.Info.UpdateTime, s.Info.CreateTime} {
		if ts == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t
		}
	}
	return fallback
}

// cronParser parses both 5-field (minute precision) and 6-field
// (with-seconds) crontab syntax. Robfig defaults to 5 fields; we add
// the optional seconds descriptor so workflows can be sub-minute.
var cronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// nextScheduleFire computes the next fire time across all configured
// cron strings, returning the earliest. (false, _) means the schedule
// has no valid spec.
func nextScheduleFire(s Schedule, after time.Time) (time.Time, bool) {
	var earliest time.Time
	for _, cs := range s.Spec.CronString {
		sched, err := cronParser.Parse(cs)
		if err != nil {
			continue
		}
		t := sched.Next(after)
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	if earliest.IsZero() {
		return time.Time{}, false
	}
	return earliest, true
}

func unmarshal(b []byte, v any) error {
	if err := jsonUnmarshal(b, v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
