// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"fmt"
)

// Server-side appliers for the two durable workflow commands beyond the
// activity core (schedule/complete/fail): continueAsNew and startChild.
// Both are emitted by the decider on RespondWorkflowTaskCompleted and
// applied here under the run lock (respondWorkflowHandler holds it), in the
// same event-sourced style as applyScheduleActivity — history is the source
// of truth, application is idempotent on replay.

// history event types for the durable workflow commands.
const (
	evtContinuedAsNew = "WORKFLOW_EXECUTION_CONTINUED_AS_NEW"
	evtChildStarted   = "CHILD_WORKFLOW_EXECUTION_STARTED"
)

// applyContinueAsNew closes the current run as CONTINUED_AS_NEW and starts a
// successor run under the SAME workflowId with a fresh runId, carrying the
// new input (a JSON-encoded argument array) plus the run's task queue,
// type, search attributes and memo. The successor's first workflow task is
// scheduled by startWorkflowFull, so the worker replays it from event 0.
//
// Caller holds the run lock for (ns, wf, run). The successor has a distinct
// runId ⇒ a distinct lock key, so no re-entrancy.
func (e *engine) applyContinueAsNew(ns, wf, run string, input []byte, wfType, taskQueue string) error {
	cur, ok, err := e.DescribeWorkflow(ns, wf, run)
	if err != nil {
		return err
	}
	if !ok || isTerminal(cur.Status) {
		return nil // idempotent: already continued / terminal on replay
	}
	if wfType == "" {
		wfType = cur.Type.Name
	}
	if taskQueue == "" {
		taskQueue = cur.TaskQueue
	}
	var args any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return fmt.Errorf("continueAsNew: decode input: %w", err)
		}
	}
	newRun := newRunId()
	if _, err := e.terminalTransition(ns, wf, run,
		"WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW",
		"workflow.continued_as_new", evtContinuedAsNew,
		map[string]any{"newExecutionRunId": newRun}); err != nil {
		return err
	}
	// A CONTINUED_AS_NEW run is terminal ⇒ no conflict policy needed; the
	// successor is a brand-new run under the same workflowId.
	_, err = e.startWorkflowFull(ns, wf, newRun, TypeRef{Name: wfType}, taskQueue, args, "", cur.SearchAttrs, cur.Memo, "")
	return err
}

// applyStartChild starts a detached child workflow (parentClosePolicy
// ABANDON semantics: the child is an independent top-level run the parent
// does not await to completion). It is idempotent per (parentRun, seq): a
// replay or recovery re-application is a no-op once the parent history
// carries CHILD_WORKFLOW_EXECUTION_STARTED{seq}. After starting the child
// it appends that event and re-schedules the parent's workflow task so the
// parent replays and its startChild future resolves.
//
// Caller holds the parent run lock for (ns, parentWf, parentRun).
func (e *engine) applyStartChild(ns, parentWf, parentRun string, seq int, childWfId, childType, taskQueue string, input []byte, searchAttrs map[string]any) error {
	if childType == "" {
		return fmt.Errorf("startChild: child workflow type required")
	}
	if e.childStartRecorded(ns, parentWf, parentRun, seq) {
		return nil // exactly-once: already started on a prior episode
	}
	var args any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return fmt.Errorf("startChild: decode input: %w", err)
		}
	}
	if childWfId == "" {
		childWfId = childType + "-child-" + newRunId()[:12]
	}
	// USE_EXISTING so a redundant application never spawns a second child
	// under the same workflowId; the child is fully independent (ABANDON).
	child, err := e.startWorkflowFull(ns, childWfId, "", TypeRef{Name: childType}, taskQueue, args, "", searchAttrs, nil, "WORKFLOW_ID_CONFLICT_POLICY_USE_EXISTING")
	if err != nil {
		return err
	}
	if _, err := e.appendHistory(ns, parentWf, parentRun, evtChildStarted, map[string]any{
		"seq":             seq,
		"childWorkflowId": child.Execution.WorkflowId,
		"childRunId":      child.Execution.RunId,
	}); err != nil {
		return err
	}
	e.emit(Event{Kind: "workflow.child_started", Namespace: ns, WorkflowID: parentWf, RunID: parentRun,
		Data: map[string]any{"seq": seq, "childWorkflowId": child.Execution.WorkflowId, "childRunId": child.Execution.RunId}})
	return e.scheduleWorkflowTask(ns, parentWf, parentRun)
}

// childStartRecorded reports whether the parent history already carries a
// CHILD_WORKFLOW_EXECUTION_STARTED for this seq (the exactly-once anchor).
func (e *engine) childStartRecorded(ns, parentWf, parentRun string, seq int) bool {
	events, err := listInto[HistoryEvent](e.store, fmt.Sprintf("wfh/%s/%s/%s/", ns, parentWf, parentRun))
	if err != nil {
		return false
	}
	for i := range events {
		if events[i].EventType != evtChildStarted {
			continue
		}
		if s, ok := events[i].Attributes["seq"]; ok && int(toFloat(s)) == seq {
			return true
		}
	}
	return false
}

// toFloat coerces a JSON-decoded numeric attribute (float64) to float64.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}
