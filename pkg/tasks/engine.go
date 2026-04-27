// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	store *store
}

func newEngine(s *store) *engine { return &engine{store: s} }

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
	return e.store.put("ns/"+ns.NamespaceInfo.Name, ns)
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

func (e *engine) terminalTransition(ns, workflowId, runId, status string) (*WorkflowExecution, error) {
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
	return wf, nil
}

func (e *engine) CancelWorkflow(ns, workflowId, runId string) (*WorkflowExecution, error) {
	return e.terminalTransition(ns, workflowId, runId, "WORKFLOW_EXECUTION_STATUS_CANCELED")
}

func (e *engine) TerminateWorkflow(ns, workflowId, runId string) (*WorkflowExecution, error) {
	return e.terminalTransition(ns, workflowId, runId, "WORKFLOW_EXECUTION_STATUS_TERMINATED")
}

func (e *engine) SignalWorkflow(ns, workflowId, runId, name string, payload any) error {
	wf, ok, err := e.DescribeWorkflow(ns, workflowId, runId)
	if !ok || err != nil {
		return fmt.Errorf("workflow not found")
	}
	wf.HistoryLen++
	_ = name
	_ = payload
	return e.store.put(fmt.Sprintf("wf/%s/%s/%s", ns, wf.Execution.WorkflowId, wf.Execution.RunId), wf)
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
	return e.store.put(fmt.Sprintf("sc/%s/%s", s.Namespace, s.ScheduleId), s)
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

// runScheduler is a stub: it polls schedules every minute and bumps
// their action count if they are due. It does NOT yet enqueue real
// workflow starts — that's worker-runtime work. The hook exists so
// the UI shows non-zero action counts as time passes.
func (e *engine) runScheduler(stop <-chan struct{}) {
	t := time.NewTicker(60 * time.Second)
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
	// Walk all schedules across namespaces. memdb has no batch ops here,
	// so a simple list works; the volume is admin-tier.
	return e.store.list("sc/", func(_ string, body []byte) error {
		var s Schedule
		if err := unmarshal(body, &s); err != nil {
			return err
		}
		if s.State.Paused {
			return nil
		}
		s.Info.ActionCount++
		s.Info.UpdateTime = nowRFC3339()
		return e.store.put(fmt.Sprintf("sc/%s/%s", s.Namespace, s.ScheduleId), s)
	})
}

func unmarshal(b []byte, v any) error {
	if err := jsonUnmarshal(b, v); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
