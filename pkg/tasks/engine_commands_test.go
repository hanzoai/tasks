// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"testing"
)

// TestEngine_SearchAttributes_StoredAndQueryable proves per-workflow search
// attributes reach storage and are visibility-queryable (the social
// deletePost / startWorkflow list-by-postId path).
func TestEngine_SearchAttributes_StoredAndQueryable(t *testing.T) {
	en, ns := engineFixture(t)
	sa := map[string]any{"postId": "p1", "organizationId": "o1"}
	if _, err := en.startWorkflowFull(ns, "post_p1", "", TypeRef{Name: "PostWorkflow"}, "main", nil, "", sa, nil, ""); err != nil {
		t.Fatalf("start with search attrs: %v", err)
	}
	_, _ = en.startWorkflowFull(ns, "post_p2", "", TypeRef{Name: "PostWorkflow"}, "main", nil, "", map[string]any{"postId": "p2", "organizationId": "o1"}, nil, "")

	cases := []struct {
		query string
		want  int
	}{
		{`postId = "p1"`, 1},
		{`postId = "p2"`, 1},
		{`postId = "nope"`, 0},
		{`organizationId = "o1"`, 2},
		{`organizationId = "o1" AND ExecutionStatus = "Running"`, 2},
		{`postId = "p1" AND ExecutionStatus = "Running"`, 1},
	}
	for _, c := range cases {
		got, err := en.ListWorkflowExecutions(ns, c.query)
		if err != nil {
			t.Fatalf("list %q: %v", c.query, err)
		}
		if len(got) != c.want {
			t.Fatalf("query %q: len=%d, want %d", c.query, len(got), c.want)
		}
	}
}

// TestEngine_WorkflowIdConflictPolicy covers TERMINATE_EXISTING and
// USE_EXISTING against a live run under a deterministic workflowId.
func TestEngine_WorkflowIdConflictPolicy(t *testing.T) {
	en, ns := engineFixture(t)

	// TERMINATE_EXISTING supersedes the running run with a fresh one.
	run1, err := en.startWorkflowFull(ns, "post_x", "", TypeRef{Name: "PostWorkflow"}, "main", nil, "", nil, nil, "TERMINATE_EXISTING")
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	run2, err := en.startWorkflowFull(ns, "post_x", "", TypeRef{Name: "PostWorkflow"}, "main", nil, "", nil, nil, "TERMINATE_EXISTING")
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if run2.Execution.RunId == run1.Execution.RunId {
		t.Fatalf("TERMINATE_EXISTING must create a new run")
	}
	if old, _, _ := en.DescribeWorkflow(ns, "post_x", run1.Execution.RunId); old.Status != "WORKFLOW_EXECUTION_STATUS_TERMINATED" {
		t.Fatalf("prior run status=%s, want TERMINATED", old.Status)
	}
	if run2.Status != "WORKFLOW_EXECUTION_STATUS_RUNNING" {
		t.Fatalf("new run status=%s, want RUNNING", run2.Status)
	}

	// USE_EXISTING returns the live run without starting a new one.
	first, _ := en.startWorkflowFull(ns, "post_y", "", TypeRef{Name: "PostWorkflow"}, "main", nil, "", nil, nil, "")
	reuse, err := en.startWorkflowFull(ns, "post_y", "", TypeRef{Name: "PostWorkflow"}, "main", nil, "", nil, nil, "USE_EXISTING")
	if err != nil {
		t.Fatalf("use-existing: %v", err)
	}
	if reuse.Execution.RunId != first.Execution.RunId {
		t.Fatalf("USE_EXISTING must reuse the live run")
	}
}

// TestEngine_ContinueAsNew proves a run closes as CONTINUED_AS_NEW and a
// successor run starts under the same workflowId carrying the new input,
// search attributes and memo.
func TestEngine_ContinueAsNew(t *testing.T) {
	en, ns := engineFixture(t)
	sa := map[string]any{"organizationId": "o1"}
	orig, err := en.startWorkflowFull(ns, "digest_o1", "", TypeRef{Name: "digestEmailWorkflow"}, "main", []any{map[string]any{"n": float64(1)}}, "", sa, nil, "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	newInput, _ := json.Marshal([]any{map[string]any{"n": float64(2)}})
	unlock := en.lockRun(ns, "digest_o1", orig.Execution.RunId)
	if err := en.applyContinueAsNew(ns, "digest_o1", orig.Execution.RunId, newInput, "", ""); err != nil {
		unlock()
		t.Fatalf("continueAsNew: %v", err)
	}
	unlock()

	// Original run is terminal CONTINUED_AS_NEW.
	old, _, _ := en.DescribeWorkflow(ns, "digest_o1", orig.Execution.RunId)
	if old.Status != "WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW" {
		t.Fatalf("original status=%s, want CONTINUED_AS_NEW", old.Status)
	}
	// Latest run is a fresh RUNNING successor with the carried input + attrs.
	succ, ok, _ := en.DescribeWorkflow(ns, "digest_o1", "")
	if !ok || succ.Execution.RunId == orig.Execution.RunId {
		t.Fatalf("successor must be a new run; got ok=%v run=%s", ok, succ.Execution.RunId)
	}
	if succ.Status != "WORKFLOW_EXECUTION_STATUS_RUNNING" {
		t.Fatalf("successor status=%s, want RUNNING", succ.Status)
	}
	if succ.Type.Name != "digestEmailWorkflow" {
		t.Fatalf("successor type=%s, want same type", succ.Type.Name)
	}
	if succ.SearchAttrs["organizationId"] != "o1" {
		t.Fatalf("successor search attrs not carried: %+v", succ.SearchAttrs)
	}
	gotInput, _ := json.Marshal(succ.Input)
	if string(gotInput) != string(newInput) {
		t.Fatalf("successor input=%s, want %s", gotInput, newInput)
	}
}

// TestEngine_StartChild proves a detached child starts, the parent records
// CHILD_WORKFLOW_EXECUTION_STARTED, and re-application is idempotent per seq.
func TestEngine_StartChild(t *testing.T) {
	en, ns := engineFixture(t)
	parent, err := en.startWorkflowFull(ns, "post_parent", "", TypeRef{Name: "postWorkflowV105"}, "main", nil, "", nil, nil, "")
	if err != nil {
		t.Fatalf("start parent: %v", err)
	}
	pw, pr := parent.Execution.WorkflowId, parent.Execution.RunId

	childInput, _ := json.Marshal([]any{map[string]any{"postNow": true}})
	unlock := en.lockRun(ns, pw, pr)
	if err := en.applyStartChild(ns, pw, pr, 0, "post_parent_child", "postWorkflowV105", "main", childInput, map[string]any{"postId": "p1"}); err != nil {
		unlock()
		t.Fatalf("startChild: %v", err)
	}
	// Idempotent re-application for the same seq is a no-op.
	if err := en.applyStartChild(ns, pw, pr, 0, "post_parent_child", "postWorkflowV105", "main", childInput, nil); err != nil {
		unlock()
		t.Fatalf("startChild replay: %v", err)
	}
	unlock()

	// Child exists and is running.
	child, ok, _ := en.DescribeWorkflow(ns, "post_parent_child", "")
	if !ok || child.Status != "WORKFLOW_EXECUTION_STATUS_RUNNING" {
		t.Fatalf("child not running: ok=%v", ok)
	}
	if child.SearchAttrs["postId"] != "p1" {
		t.Fatalf("child search attrs not carried: %+v", child.SearchAttrs)
	}
	// Parent history has exactly one CHILD_WORKFLOW_EXECUTION_STARTED{seq=0}.
	hist, _, _ := en.GetWorkflowHistory(ns, pw, pr, 0, 100, false)
	n := 0
	for _, ev := range hist {
		if ev.EventType == evtChildStarted {
			n++
			if int(toFloat(ev.Attributes["seq"])) != 0 {
				t.Fatalf("child-started seq=%v, want 0", ev.Attributes["seq"])
			}
		}
	}
	if n != 1 {
		t.Fatalf("CHILD_WORKFLOW_EXECUTION_STARTED count=%d, want 1 (idempotent)", n)
	}
}
