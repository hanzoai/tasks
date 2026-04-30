// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"net/http"
	"testing"
)

// ── engine: standalone activities ───────────────────────────────────

func TestActivities_StartAndDescribe(t *testing.T) {
	en, ns := engineFixture(t)
	a, err := en.StartActivity(ns, "act-1", "", TypeRef{Name: "Echo"}, "q", map[string]any{"hello": "world"}, nil, "", "", "", "", "user@x", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if a.Status != activityStateScheduled {
		t.Fatalf("status=%s", a.Status)
	}
	if a.Execution.WorkflowId != "act-1" || a.Execution.RunId == "" {
		t.Fatalf("ids: %+v", a.Execution)
	}
	got, ok, err := en.DescribeActivity(ns, "act-1", a.Execution.RunId)
	if err != nil || !ok {
		t.Fatalf("describe: ok=%v err=%v", ok, err)
	}
	if got.Type.Name != "Echo" {
		t.Fatalf("type=%s", got.Type.Name)
	}
}

func TestActivities_StartIdempotent(t *testing.T) {
	en, ns := engineFixture(t)
	a1, err := en.StartActivity(ns, "act-i", "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "req-1")
	if err != nil {
		t.Fatalf("start1: %v", err)
	}
	a2, err := en.StartActivity(ns, "act-i", "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "req-1")
	if err != nil {
		t.Fatalf("start2: %v", err)
	}
	if a1.Execution.RunId != a2.Execution.RunId {
		t.Fatalf("idempotency broke: %s vs %s", a1.Execution.RunId, a2.Execution.RunId)
	}
}

func TestActivities_HeartbeatPromotesStarted(t *testing.T) {
	en, ns := engineFixture(t)
	a, _ := en.StartActivity(ns, "act-h", "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "")
	if err := en.HeartbeatActivity(ns, "act-h", a.Execution.RunId, "tick"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	got, _, _ := en.DescribeActivity(ns, "act-h", a.Execution.RunId)
	if got.Status != activityStateStarted {
		t.Fatalf("status=%s, want STARTED", got.Status)
	}
	if got.LastHeartbeatTime == "" {
		t.Fatalf("LastHeartbeatTime not set")
	}
}

func TestActivities_TerminalRejectsHeartbeat(t *testing.T) {
	en, ns := engineFixture(t)
	a, _ := en.StartActivity(ns, "act-t", "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "")
	if err := en.CompleteActivity(ns, "act-t", a.Execution.RunId, "ok", "user"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := en.HeartbeatActivity(ns, "act-t", a.Execution.RunId, "tick"); err == nil {
		t.Fatalf("heartbeat after complete: expected error")
	}
	if err := en.CancelActivity(ns, "act-t", a.Execution.RunId, "", ""); err == nil {
		t.Fatalf("cancel after complete: expected error")
	}
}

func TestActivities_HistoryIncludesScheduledStartedCompleted(t *testing.T) {
	en, ns := engineFixture(t)
	a, _ := en.StartActivity(ns, "act-hist", "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "")
	_ = en.HeartbeatActivity(ns, "act-hist", a.Execution.RunId, "go")
	_ = en.CompleteActivity(ns, "act-hist", a.Execution.RunId, "ok", "")
	events, _, err := en.GetActivityHistory(ns, "act-hist", a.Execution.RunId, 0, 0, false)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(events) < 3 {
		t.Fatalf("len=%d, want >=3", len(events))
	}
	if events[0].EventType != "ACTIVITY_TASK_SCHEDULED" {
		t.Fatalf("first=%s", events[0].EventType)
	}
}

func TestActivities_ListPaginates(t *testing.T) {
	en, ns := engineFixture(t)
	for i := 0; i < 5; i++ {
		_, _ = en.StartActivity(ns, "act-l-"+string(rune('a'+i)), "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "")
	}
	page, next, err := en.ListActivities(ns, "", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page) != 2 || next == "" {
		t.Fatalf("page=%d next=%q", len(page), next)
	}
	page2, _, _ := en.ListActivities(ns, next, 10)
	if len(page2) != 3 {
		t.Fatalf("page2=%d", len(page2))
	}
}

// ── HTTP routes (table-driven) ──────────────────────────────────────

func TestHTTP_Activities(t *testing.T) {
	en, h := httpFixture(t)
	// seed
	a, _ := en.StartActivity("default", "a-x", "", TypeRef{Name: "Echo"}, "q", nil, nil, "", "", "", "", "", "")
	run := a.Execution.RunId

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		code   int
	}{
		{"list", http.MethodGet, "/v1/tasks/namespaces/default/activities", nil, 200},
		{"list-pagesize", http.MethodGet, "/v1/tasks/namespaces/default/activities?pageSize=1", nil, 200},
		{"start-ok", http.MethodPost, "/v1/tasks/namespaces/default/activities", map[string]any{
			"activityId":   "a-y",
			"activityType": map[string]string{"name": "Echo"},
			"taskQueue":    "q",
			"requestId":    "rq",
		}, 200},
		{"start-validate-empty", http.MethodPost, "/v1/tasks/namespaces/default/activities", map[string]any{
			"activityId": "",
		}, 400},
		{"describe-ok", http.MethodGet, "/v1/tasks/namespaces/default/activities/a-x/" + run, nil, 200},
		{"describe-404", http.MethodGet, "/v1/tasks/namespaces/default/activities/ghost/r0", nil, 404},
		{"heartbeat-ok", http.MethodPost, "/v1/tasks/namespaces/default/activities/a-x/" + run + "/heartbeat", map[string]any{"details": "tick"}, 200},
		{"history-ok", http.MethodGet, "/v1/tasks/namespaces/default/activities/a-x/" + run + "/history", nil, 200},
		{"history-404", http.MethodGet, "/v1/tasks/namespaces/default/activities/ghost/r0/history", nil, 404},
		{"complete-ok", http.MethodPost, "/v1/tasks/namespaces/default/activities/a-x/" + run + "/complete", map[string]any{"result": "ok"}, 200},
		{"cancel-after-complete-409", http.MethodPost, "/v1/tasks/namespaces/default/activities/a-x/" + run + "/cancel", map[string]any{"reason": "x"}, 409},
		{"fail-404", http.MethodPost, "/v1/tasks/namespaces/default/activities/ghost/r0/fail", map[string]any{"cause": "x"}, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := httpDo(t, h, c.method, c.path, c.body)
			if code != c.code {
				t.Fatalf("code=%d want %d", code, c.code)
			}
		})
	}
}

func TestHTTP_Activities_FailFlow(t *testing.T) {
	en, h := httpFixture(t)
	a, _ := en.StartActivity("default", "a-f", "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "")
	run := a.Execution.RunId
	code, _ := httpDo(t, h, http.MethodPost, "/v1/tasks/namespaces/default/activities/a-f/"+run+"/fail", map[string]any{"cause": "boom"}, )
	if code != 200 {
		t.Fatalf("fail code=%d", code)
	}
	got, _, _ := en.DescribeActivity("default", "a-f", run)
	if got.Status != activityStateFailed || got.FailureCause != "boom" {
		t.Fatalf("post-fail %+v", got)
	}
}

func TestHTTP_Activities_CancelFlow(t *testing.T) {
	en, h := httpFixture(t)
	a, _ := en.StartActivity("default", "a-c", "", TypeRef{Name: "T"}, "q", nil, nil, "", "", "", "", "", "")
	run := a.Execution.RunId
	code, _ := httpDo(t, h, http.MethodPost, "/v1/tasks/namespaces/default/activities/a-c/"+run+"/cancel", map[string]any{"reason": "stop"})
	if code != 200 {
		t.Fatalf("cancel code=%d", code)
	}
	got, _, _ := en.DescribeActivity("default", "a-c", run)
	if got.Status != activityStateCanceled {
		t.Fatalf("status=%s", got.Status)
	}
}

// ── Deployment + Version CRUD ────────────────────────────────────────

func TestEngine_CreateDeployment_Conflict(t *testing.T) {
	en, ns := engineFixture(t)
	if _, err := en.CreateDeployment(ns, "svc", "", "z@x", "small"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := en.CreateDeployment(ns, "svc", "", "", ""); err == nil {
		t.Fatalf("expected duplicate error")
	}
	if _, err := en.CreateDeployment(ns, "", "", "", ""); err == nil {
		t.Fatalf("expected name required")
	}
}

func TestEngine_UpdateDeployment(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.CreateDeployment(ns, "svc-u", "old", "a@x", "small")
	desc := "new"
	owner := "b@x"
	d, err := en.UpdateDeployment(ns, "svc-u", DeploymentPatch{Description: &desc, OwnerEmail: &owner})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if d.Description != "new" || d.OwnerEmail != "b@x" {
		t.Fatalf("not patched: %+v", d)
	}
	if _, err := en.UpdateDeployment(ns, "ghost", DeploymentPatch{Description: &desc}); err == nil {
		t.Fatalf("expected not-found")
	}
}

func TestEngine_DeleteDeployment(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.CreateDeployment(ns, "svc-d", "", "", "")
	_, _ = en.CreateVersion(ns, "svc-d", "v1", "", "", "", nil)
	if err := en.DeleteDeployment(ns, "svc-d", false); err == nil {
		t.Fatalf("expected error: versions exist")
	}
	if err := en.DeleteDeployment(ns, "svc-d", true); err != nil {
		t.Fatalf("force delete: %v", err)
	}
	if _, ok, _ := en.DescribeDeployment(ns, "svc-d"); ok {
		t.Fatalf("not deleted")
	}
}

func TestEngine_CreateVersion_Conflict(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.CreateDeployment(ns, "svc-v", "", "", "")
	v, err := en.CreateVersion(ns, "svc-v", "v1", "first", "small", "ghcr.io/x:v1", map[string]string{"K": "V"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.State != "DEPLOYMENT_STATE_CURRENT" {
		t.Fatalf("first should be CURRENT, got %s", v.State)
	}
	if _, err := en.CreateVersion(ns, "svc-v", "v1", "", "", "", nil); err == nil {
		t.Fatalf("expected duplicate")
	}
	d, _, _ := en.DescribeDeployment(ns, "svc-v")
	if d.DefaultBuildId != "v1" {
		t.Fatalf("default not set: %s", d.DefaultBuildId)
	}
}

func TestEngine_UpdateVersion(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.CreateDeployment(ns, "svc-uv", "", "", "")
	_, _ = en.CreateVersion(ns, "svc-uv", "v1", "old", "small", "img:1", nil)
	desc := "new"
	v, err := en.UpdateVersion(ns, "svc-uv", "v1", DeploymentVersionPatch{Description: &desc, Env: map[string]string{"X": "Y"}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if v.Description != "new" || v.Env["X"] != "Y" {
		t.Fatalf("not patched: %+v", v)
	}
	if _, err := en.UpdateVersion(ns, "svc-uv", "ghost", DeploymentVersionPatch{Description: &desc}); err == nil {
		t.Fatalf("expected not-found")
	}
}

func TestEngine_ValidateVersion(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.CreateDeployment(ns, "svc-val", "", "", "")
	_, _ = en.CreateVersion(ns, "svc-val", "v1", "", "", "", nil)
	res, err := en.ValidateVersion(ns, "svc-val", "v1")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !res.NetworkOk {
		t.Fatalf("networkOk false")
	}
	if _, err := en.ValidateVersion(ns, "svc-val", "ghost"); err == nil {
		t.Fatalf("expected not-found")
	}
}

// HTTP routes — table-driven, one case per route happy/error.
func TestHTTP_Deployments(t *testing.T) {
	en, h := httpFixture(t)
	_, _ = en.CreateDeployment("default", "seed", "", "", "")
	_, _ = en.CreateVersion("default", "seed", "v1", "", "", "", nil)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
		code   int
	}{
		{"create-ok", http.MethodPost, "/v1/tasks/namespaces/default/deployments", map[string]any{"name": "svc-h", "ownerEmail": "z@x"}, 200},
		{"create-conflict", http.MethodPost, "/v1/tasks/namespaces/default/deployments", map[string]any{"name": "svc-h"}, 409},
		{"describe-ok", http.MethodGet, "/v1/tasks/namespaces/default/deployments/seed", nil, 200},
		{"patch-ok", http.MethodPost, "/v1/tasks/namespaces/default/deployments/seed", map[string]any{"description": "patched"}, 200},
		{"patch-404", http.MethodPost, "/v1/tasks/namespaces/default/deployments/ghost", map[string]any{"description": "x"}, 404},
		{"create-version-ok", http.MethodPost, "/v1/tasks/namespaces/default/deployments/seed/versions", map[string]any{"buildId": "v2", "image": "ghcr.io/x:v2"}, 200},
		{"create-version-409", http.MethodPost, "/v1/tasks/namespaces/default/deployments/seed/versions", map[string]any{"buildId": "v1"}, 409},
		{"create-version-404", http.MethodPost, "/v1/tasks/namespaces/default/deployments/ghost/versions", map[string]any{"buildId": "v1"}, 404},
		{"patch-version-ok", http.MethodPost, "/v1/tasks/namespaces/default/deployments/seed/versions/v1", map[string]any{"description": "u"}, 200},
		{"patch-version-404", http.MethodPost, "/v1/tasks/namespaces/default/deployments/seed/versions/ghost", map[string]any{"description": "u"}, 404},
		{"validate-ok", http.MethodPost, "/v1/tasks/namespaces/default/deployments/seed/versions/v1/validate", nil, 200},
		{"validate-404", http.MethodPost, "/v1/tasks/namespaces/default/deployments/seed/versions/ghost/validate", nil, 404},
		{"delete-conflict", http.MethodDelete, "/v1/tasks/namespaces/default/deployments/seed", nil, 409},
		{"delete-force", http.MethodDelete, "/v1/tasks/namespaces/default/deployments/seed?force=true", nil, 200},
		{"delete-404", http.MethodDelete, "/v1/tasks/namespaces/default/deployments/ghost", nil, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := httpDo(t, h, c.method, c.path, c.body)
			if code != c.code {
				t.Fatalf("code=%d want %d", code, c.code)
			}
		})
	}
}
