// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// TriggerSchedule fires a synthetic action for the schedule, bumping
// info.actionCount and starting a workflow per the schedule's action.
// Idempotent on requestID: a second call with the same (ns, id, reqID)
// returns the prior workflow without firing again.
func (e *engine) TriggerSchedule(ns, id, requestID string) (*WorkflowExecution, error) {
	s, ok, err := e.DescribeSchedule(ns, id)
	if !ok {
		return nil, fmt.Errorf("schedule not found")
	}
	if err != nil {
		return nil, err
	}
	if requestID != "" {
		var prev struct {
			WorkflowId string `json:"workflowId"`
			RunId      string `json:"runId"`
		}
		if ok, _ := e.store.get(fmt.Sprintf("sctrig/%s/%s/%s", ns, id, requestID), &prev); ok && prev.RunId != "" {
			if wf, ok2, _ := e.DescribeWorkflow(ns, prev.WorkflowId, prev.RunId); ok2 {
				return wf, nil
			}
		}
	}
	wf, err := e.StartWorkflow(s.Namespace, "", "", s.Action.WorkflowType, s.Action.TaskQueue, s.Action.Input)
	if err != nil {
		return nil, err
	}
	s.Info.ActionCount++
	s.Info.UpdateTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("sc/%s/%s", ns, id), s); err != nil {
		return nil, err
	}
	if requestID != "" {
		_ = e.store.put(fmt.Sprintf("sctrig/%s/%s/%s", ns, id, requestID), map[string]string{
			"workflowId": wf.Execution.WorkflowId,
			"runId":      wf.Execution.RunId,
		})
	}
	e.broker.publish(Event{Kind: "schedule.triggered", Namespace: ns, ScheduleID: id, Data: wf})
	return wf, nil
}

// UpdateSchedule replaces spec/action/state on an existing schedule.
// Not a delete+recreate: ScheduleId, Namespace, Info.CreateTime, and
// Info.ActionCount are preserved.
func (e *engine) UpdateSchedule(ns, id string, in Schedule) (*Schedule, error) {
	cur, ok, err := e.DescribeSchedule(ns, id)
	if !ok {
		return nil, fmt.Errorf("schedule not found")
	}
	if err != nil {
		return nil, err
	}
	cur.Spec = in.Spec
	cur.Action = in.Action
	cur.State = in.State
	cur.Info.UpdateTime = nowRFC3339()
	if next, ok := nextScheduleFire(*cur, time.Now().UTC()); ok {
		cur.Info.NextActionTime = next.UTC().Format(time.RFC3339)
	}
	if err := e.store.put(fmt.Sprintf("sc/%s/%s", ns, id), *cur); err != nil {
		return nil, err
	}
	e.broker.publish(Event{Kind: "schedule.updated", Namespace: ns, ScheduleID: id, Data: cur})
	return cur, nil
}

// ScheduleMatchingTimes returns the next firing times in [from, to].
// Pure compute against the spec; touches no storage.
func (e *engine) ScheduleMatchingTimes(ns, id string, from, to time.Time) ([]time.Time, error) {
	s, ok, err := e.DescribeSchedule(ns, id)
	if !ok {
		return nil, fmt.Errorf("schedule not found")
	}
	if err != nil {
		return nil, err
	}
	if to.Before(from) {
		return nil, fmt.Errorf("to before from")
	}
	out := []time.Time{}
	cursor := from
	for i := 0; i < 1000; i++ {
		next, ok := nextScheduleFire(*s, cursor)
		if !ok || next.After(to) {
			break
		}
		out = append(out, next)
		cursor = next.Add(time.Second)
	}
	return out, nil
}

// SetCurrentDeploymentVersion flips the default build for a deployment
// series. Rejected if buildId is not present in the series. Idempotent
// when buildId already matches the default.
func (e *engine) SetCurrentDeploymentVersion(ns, name, buildId string) (*Deployment, error) {
	if buildId == "" {
		return nil, fmt.Errorf("buildId required")
	}
	var d Deployment
	ok, err := e.store.get(fmt.Sprintf("dp/%s/%s", ns, name), &d)
	if !ok {
		return nil, fmt.Errorf("deployment not found")
	}
	if err != nil {
		return nil, err
	}
	if d.DefaultBuildId == buildId {
		return &d, nil
	}
	found := false
	for i := range d.Versions {
		if d.Versions[i].BuildId == buildId {
			found = true
			d.Versions[i].State = "DEPLOYMENT_STATE_CURRENT"
		} else if d.Versions[i].State == "DEPLOYMENT_STATE_CURRENT" {
			d.Versions[i].State = "DEPLOYMENT_STATE_RETIRED"
		}
	}
	if !found {
		return nil, fmt.Errorf("buildId %q not in deployment", buildId)
	}
	d.DefaultBuildId = buildId
	d.UpdateTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("dp/%s/%s", ns, name), d); err != nil {
		return nil, err
	}
	return &d, nil
}

// DeleteDeploymentVersion removes a single buildId from the deployment
// series. The current/default cannot be deleted; flip default first.
func (e *engine) DeleteDeploymentVersion(ns, name, buildId string) (*Deployment, error) {
	var d Deployment
	ok, err := e.store.get(fmt.Sprintf("dp/%s/%s", ns, name), &d)
	if !ok {
		return nil, fmt.Errorf("deployment not found")
	}
	if err != nil {
		return nil, err
	}
	if d.DefaultBuildId == buildId {
		return nil, fmt.Errorf("cannot delete current buildId; set-current to another first")
	}
	out := d.Versions[:0]
	removed := false
	for _, b := range d.Versions {
		if b.BuildId == buildId {
			removed = true
			continue
		}
		out = append(out, b)
	}
	if !removed {
		return nil, fmt.Errorf("buildId %q not found", buildId)
	}
	d.Versions = out
	d.UpdateTime = nowRFC3339()
	if err := e.store.put(fmt.Sprintf("dp/%s/%s", ns, name), d); err != nil {
		return nil, err
	}
	return &d, nil
}

// QueryArchival returns archived workflows when archival is enabled.
// Today archival is unconditionally disabled — UI renders the documented
// "disabled" state from `enabled:false`.
func (e *engine) QueryArchival(_, _, _ string) (map[string]any, error) {
	return map[string]any{
		"enabled":    false,
		"executions": []any{},
		"nextPageToken": "",
		"error":      "archival disabled",
	}, nil
}

// ListWorkflowChain returns alternate runs of the same workflowId,
// newest first. Today this is just every run keyed under the workflow
// in storage; once continueAsNew lands the result will include the
// linkage chain.
func (e *engine) ListWorkflowChain(ns, workflowId string) ([]WorkflowExecution, error) {
	rows, err := listInto[WorkflowExecution](e.store, fmt.Sprintf("wf/%s/%s/", ns, workflowId))
	if err != nil {
		return nil, err
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].StartTime > rows[j].StartTime })
	return rows, nil
}

// RevokeIdentity removes (ns, email) from the identity table.
// Inverse of GrantIdentity. No-op if missing.
func (e *engine) RevokeIdentity(ns, email string) error {
	if ns == "" || email == "" {
		return fmt.Errorf("namespace + email required")
	}
	return e.store.del(fmt.Sprintf("id/%s/%s", ns, email))
}

// DeleteNexusEndpoint removes a Nexus endpoint by name. Inverse of
// CreateNexusEndpoint.
func (e *engine) DeleteNexusEndpoint(ns, name string) error {
	if ns == "" || name == "" {
		return fmt.Errorf("namespace + name required")
	}
	return e.store.del(fmt.Sprintf("nx/%s/%s", ns, name))
}

// RemoveSearchAttribute deletes a search attribute. Inverse of
// AddSearchAttribute.
func (e *engine) RemoveSearchAttribute(ns, name string) error {
	if _, ok, _ := e.DescribeNamespace(ns); !ok {
		return fmt.Errorf("namespace %q not registered", ns)
	}
	if name == "" {
		return fmt.Errorf("search attribute name required")
	}
	return e.store.del(fmt.Sprintf("sa/%s/%s", ns, name))
}

// DeprecateNamespace marks state=NAMESPACE_STATE_DELETED. Soft delete:
// records remain but reads in production should refuse to mutate.
// Inverse-side of RegisterNamespace; we never hard-delete.
func (e *engine) DeprecateNamespace(name string) (*Namespace, error) {
	n, ok, err := e.DescribeNamespace(name)
	if !ok {
		return nil, fmt.Errorf("namespace %q not registered", name)
	}
	if err != nil {
		return nil, err
	}
	n.NamespaceInfo.State = "NAMESPACE_STATE_DELETED"
	n.IsActive = false
	if err := e.store.put("ns/"+name, *n); err != nil {
		return nil, err
	}
	return n, nil
}

// ── worker registry ────────────────────────────────────────────────

// Worker is the registered worker as exposed to the UI.
type Worker struct {
	Identity      string `json:"identity"`
	Namespace     string `json:"namespace"`
	TaskQueue     string `json:"taskQueue,omitempty"`
	SDKName       string `json:"sdkName,omitempty"`
	SDKVersion    string `json:"sdkVersion,omitempty"`
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`
	FirstSeen     string `json:"firstSeen,omitempty"`
}

type workerRegistry struct {
	mu sync.RWMutex
	by map[string]map[string]*Worker // ns → identity → worker
}

func newWorkerRegistry() *workerRegistry {
	return &workerRegistry{by: map[string]map[string]*Worker{}}
}

// Register upserts the worker identity. First call records FirstSeen,
// every call refreshes LastHeartbeat.
func (r *workerRegistry) Register(w Worker) *Worker {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.Namespace == "" || w.Identity == "" {
		return nil
	}
	bucket, ok := r.by[w.Namespace]
	if !ok {
		bucket = map[string]*Worker{}
		r.by[w.Namespace] = bucket
	}
	now := nowRFC3339()
	if cur, ok := bucket[w.Identity]; ok {
		cur.LastHeartbeat = now
		if w.TaskQueue != "" {
			cur.TaskQueue = w.TaskQueue
		}
		if w.SDKName != "" {
			cur.SDKName = w.SDKName
		}
		if w.SDKVersion != "" {
			cur.SDKVersion = w.SDKVersion
		}
		return cur
	}
	w.FirstSeen = now
	w.LastHeartbeat = now
	bucket[w.Identity] = &w
	return &w
}

// Get returns the registered worker, false if unknown.
func (r *workerRegistry) Get(ns, identity string) (*Worker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bucket, ok := r.by[ns]
	if !ok {
		return nil, false
	}
	w, ok := bucket[identity]
	return w, ok
}

// List returns every registered worker in ns.
func (r *workerRegistry) List(ns string) []Worker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	bucket, ok := r.by[ns]
	if !ok {
		return []Worker{}
	}
	out := make([]Worker, 0, len(bucket))
	for _, w := range bucket {
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}
