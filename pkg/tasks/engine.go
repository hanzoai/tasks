// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

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
	store  *store
	broker *broker
	disp   *dispatcher
}

func newEngine(s *store) *engine {
	return &engine{store: s, broker: newBroker(), disp: newDispatcher()}
}

// WithOrg returns an engine view scoped to orgID. Reads and writes go
// through a store wrapper that prefixes every key with "org:<id>:".
// orgID == "" returns e unchanged (legacy embedded-use behavior).
func (e *engine) WithOrg(orgID string) *engine {
	if orgID == "" {
		return e
	}
	return &engine{store: e.store.withOrg(orgID), broker: e.broker, disp: e.disp}
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
	e.broker.publish(Event{Kind: "namespace.registered", Namespace: ns.NamespaceInfo.Name, Data: ns})
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
	if _, ok, _ := e.DescribeNamespace(ns); !ok {
		return nil, fmt.Errorf("namespace %q not registered", ns)
	}
	if typ.Name == "" {
		return nil, fmt.Errorf("workflow type required")
	}
	if workflowId == "" {
		workflowId = strings.ToLower(typ.Name) + "-" + newRunId()[:8]
	}
	if runId == "" {
		runId = newRunId()
	}
	if taskQueue == "" {
		taskQueue = "default"
	}
	wf := WorkflowExecution{
		Execution: ExecutionRef{WorkflowId: workflowId, RunId: runId},
		Type:      typ,
		StartTime: nowRFC3339(),
		Status:    "WORKFLOW_EXECUTION_STATUS_RUNNING",
		TaskQueue: taskQueue,
		Input:     input,
	}
	key := fmt.Sprintf("wf/%s/%s/%s", ns, workflowId, runId)
	if err := e.store.put(key, wf); err != nil {
		return nil, err
	}
	if e.disp != nil {
		var inputBytes []byte
		if input != nil {
			inputBytes, _ = json.Marshal(input)
		}
		e.disp.EnqueueWorkflowTask(ns, taskQueue, workflowId, runId, typ.Name, inputBytes)
	}
	e.broker.publish(Event{
		Kind:       "workflow.started",
		Namespace:  ns,
		WorkflowID: workflowId,
		RunID:      runId,
		Data:       wf,
	})
	return &wf, nil
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

func (e *engine) terminalTransition(ns, workflowId, runId, status, evKind string) (*WorkflowExecution, error) {
	wf, ok, err := e.DescribeWorkflow(ns, workflowId, runId)
	if !ok {
		return nil, fmt.Errorf("workflow %s/%s/%s not found", ns, workflowId, runId)
	}
	if err != nil {
		return nil, err
	}
	wf.Status = status
	wf.CloseTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, wf.Execution.WorkflowId, wf.Execution.RunId), wf); err != nil {
		return nil, err
	}
	e.broker.publish(Event{
		Kind:       evKind,
		Namespace:  ns,
		WorkflowID: wf.Execution.WorkflowId,
		RunID:      wf.Execution.RunId,
		Data:       wf,
	})
	return wf, nil
}

func (e *engine) CancelWorkflow(ns, workflowId, runId string) (*WorkflowExecution, error) {
	return e.terminalTransition(ns, workflowId, runId, "WORKFLOW_EXECUTION_STATUS_CANCELED", "workflow.canceled")
}

func (e *engine) TerminateWorkflow(ns, workflowId, runId string) (*WorkflowExecution, error) {
	return e.terminalTransition(ns, workflowId, runId, "WORKFLOW_EXECUTION_STATUS_TERMINATED", "workflow.terminated")
}

func (e *engine) SignalWorkflow(ns, workflowId, runId, name string, payload any) error {
	wf, ok, err := e.DescribeWorkflow(ns, workflowId, runId)
	if !ok || err != nil {
		return fmt.Errorf("workflow not found")
	}
	wf.HistoryLen++
	if err := e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, wf.Execution.WorkflowId, wf.Execution.RunId), wf); err != nil {
		return err
	}
	e.broker.publish(Event{
		Kind:       "workflow.signaled",
		Namespace:  ns,
		WorkflowID: wf.Execution.WorkflowId,
		RunID:      wf.Execution.RunId,
		Data:       map[string]any{"signal": name, "input": payload, "history_length": wf.HistoryLen},
	})
	return nil
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
	e.broker.publish(Event{Kind: "schedule.created", Namespace: s.Namespace, ScheduleID: s.ScheduleId, Data: s})
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

// ── deployments ─────────────────────────────────────────────────────

func (e *engine) CreateDeployment(d Deployment) error {
	if d.SeriesName == "" {
		return fmt.Errorf("seriesName required")
	}
	d.CreateTime = nowRFC3339()
	return e.store.put(fmt.Sprintf("dp/%s/%s", d.Namespace, d.SeriesName), d)
}

func (e *engine) ListDeployments(ns string) ([]Deployment, error) {
	return listInto[Deployment](e.store, fmt.Sprintf("dp/%s/", ns))
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
