// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// httpFixture spins up the HTTP shim against a fresh in-memory engine.
// Returns the engine + a roundtripper that issues requests against the
// in-process mux without going through a network socket.
func httpFixture(t *testing.T) (*engine, http.Handler) {
	t.Helper()
	en, _ := engineFixture(t)
	em := &Embedded{engine: en}
	return en, em.HTTPHandler()
}

func httpDo(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

func TestEngine_TriggerSchedule(t *testing.T) {
	en, ns := engineFixture(t)
	if err := en.CreateSchedule(Schedule{
		ScheduleId: "sc-trig", Namespace: ns,
		Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: ScheduleAction{WorkflowType: TypeRef{Name: "Demo"}, TaskQueue: "default"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	wf1, err := en.TriggerSchedule(ns, "sc-trig", "req-1")
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if wf1.Status != "WORKFLOW_EXECUTION_STATUS_RUNNING" {
		t.Fatalf("want running, got %s", wf1.Status)
	}
	s, _, _ := en.DescribeSchedule(ns, "sc-trig")
	if s.Info.ActionCount != 1 {
		t.Fatalf("actionCount=%d, want 1", s.Info.ActionCount)
	}
	// Idempotent on requestID.
	wf2, err := en.TriggerSchedule(ns, "sc-trig", "req-1")
	if err != nil {
		t.Fatalf("trigger 2: %v", err)
	}
	_ = wf2
	s, _, _ = en.DescribeSchedule(ns, "sc-trig")
	if s.Info.ActionCount != 1 {
		t.Fatalf("idempotency broke: actionCount=%d", s.Info.ActionCount)
	}
	// Different requestID fires.
	if _, err := en.TriggerSchedule(ns, "sc-trig", "req-2"); err != nil {
		t.Fatalf("trigger 3: %v", err)
	}
	s, _, _ = en.DescribeSchedule(ns, "sc-trig")
	if s.Info.ActionCount != 2 {
		t.Fatalf("actionCount=%d, want 2", s.Info.ActionCount)
	}
	// Unknown schedule.
	if _, err := en.TriggerSchedule(ns, "ghost", ""); err == nil {
		t.Fatalf("expected not-found")
	}
}

func TestEngine_UpdateSchedule(t *testing.T) {
	en, ns := engineFixture(t)
	if err := en.CreateSchedule(Schedule{
		ScheduleId: "sc-up", Namespace: ns,
		Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: ScheduleAction{WorkflowType: TypeRef{Name: "Demo"}, TaskQueue: "qa"},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	upd := Schedule{
		Spec:   ScheduleSpec{CronString: []string{"@every 30m"}},
		Action: ScheduleAction{WorkflowType: TypeRef{Name: "Demo2"}, TaskQueue: "qb"},
		State:  ScheduleState{Paused: true, Note: "manual"},
	}
	out, err := en.UpdateSchedule(ns, "sc-up", upd)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.Action.TaskQueue != "qb" || out.Action.WorkflowType.Name != "Demo2" {
		t.Fatalf("action not replaced: %+v", out.Action)
	}
	if !out.State.Paused {
		t.Fatalf("state not replaced")
	}
	if out.ScheduleId != "sc-up" || out.Namespace != ns {
		t.Fatalf("identity changed: %s/%s", out.Namespace, out.ScheduleId)
	}
	if _, err := en.UpdateSchedule(ns, "ghost", upd); err == nil {
		t.Fatalf("expected not-found")
	}
}

func TestEngine_ScheduleMatchingTimes(t *testing.T) {
	en, ns := engineFixture(t)
	_ = en.CreateSchedule(Schedule{
		ScheduleId: "sc-mt", Namespace: ns,
		Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: ScheduleAction{WorkflowType: TypeRef{Name: "Demo"}, TaskQueue: "default"},
	})
	from := time.Now().UTC()
	to := from.Add(3*time.Hour + 30*time.Minute)
	times, err := en.ScheduleMatchingTimes(ns, "sc-mt", from, to)
	if err != nil {
		t.Fatalf("matching: %v", err)
	}
	if len(times) < 2 || len(times) > 4 {
		t.Fatalf("expected ~3 times in 3.5h, got %d", len(times))
	}
	// to before from is rejected.
	if _, err := en.ScheduleMatchingTimes(ns, "sc-mt", to, from); err == nil {
		t.Fatalf("expected to-before-from error")
	}
}

func TestEngine_SetCurrentDeploymentVersion(t *testing.T) {
	en, ns := engineFixture(t)
	_ = en.CreateDeployment(Deployment{
		Namespace: ns, SeriesName: "svc-a",
		BuildIDs: []DeploymentBuild{
			{BuildId: "b1", State: "DEPLOYMENT_STATE_CURRENT"},
			{BuildId: "b2", State: "DEPLOYMENT_STATE_DRAFT"},
		},
		DefaultBuildId: "b1",
	})
	d, err := en.SetCurrentDeploymentVersion(ns, "svc-a", "b2")
	if err != nil {
		t.Fatalf("set-current: %v", err)
	}
	if d.DefaultBuildId != "b2" {
		t.Fatalf("default not flipped")
	}
	// Idempotent.
	d2, err := en.SetCurrentDeploymentVersion(ns, "svc-a", "b2")
	if err != nil || d2.DefaultBuildId != "b2" {
		t.Fatalf("idempotent: %v", err)
	}
	// Reject unknown buildId.
	if _, err := en.SetCurrentDeploymentVersion(ns, "svc-a", "ghost"); err == nil {
		t.Fatalf("expected reject unknown buildId")
	}
}

func TestEngine_ListWorkflowChain(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.StartWorkflow(ns, "wf-chain", "", TypeRef{Name: "Demo"}, "default", nil)
	_, _ = en.StartWorkflow(ns, "wf-chain", "", TypeRef{Name: "Demo"}, "default", nil)
	rows, err := en.ListWorkflowChain(ns, "wf-chain")
	if err != nil {
		t.Fatalf("list chain: %v", err)
	}
	if len(rows) < 1 {
		t.Fatalf("expected >=1 row, got %d", len(rows))
	}
}

func TestEngine_RevokeIdentity(t *testing.T) {
	en, ns := engineFixture(t)
	_ = en.GrantIdentity(Identity{Email: "a@x", Namespace: ns, Role: "admin"})
	if err := en.RevokeIdentity(ns, "a@x"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	rows, _ := en.ListIdentities(ns)
	if len(rows) != 0 {
		t.Fatalf("identity not revoked")
	}
}

func TestEngine_DeprecateNamespace(t *testing.T) {
	en, ns := engineFixture(t)
	n, err := en.DeprecateNamespace(ns)
	if err != nil {
		t.Fatalf("deprecate: %v", err)
	}
	if n.NamespaceInfo.State != "NAMESPACE_STATE_DELETED" {
		t.Fatalf("state=%s", n.NamespaceInfo.State)
	}
	if _, err := en.DeprecateNamespace("ghost"); err == nil {
		t.Fatalf("expected not-found")
	}
}

func TestEngine_RemoveSearchAttribute(t *testing.T) {
	en, ns := engineFixture(t)
	_ = en.AddSearchAttribute(ns, SearchAttribute{Name: "Region", Type: "Keyword"})
	if err := en.RemoveSearchAttribute(ns, "Region"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := en.RemoveSearchAttribute("ghost-ns", "x"); err == nil {
		t.Fatalf("expected ns-not-registered error")
	}
}

func TestEngine_DeleteNexusEndpoint(t *testing.T) {
	en, ns := engineFixture(t)
	_ = en.CreateNexusEndpoint(NexusEndpoint{Name: "e1", Namespace: ns, Target: "ns2://x/y"})
	if err := en.DeleteNexusEndpoint(ns, "e1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _ := en.ListNexusEndpoints(ns)
	if len(rows) != 0 {
		t.Fatalf("not deleted")
	}
}

func TestWorkerRegistry(t *testing.T) {
	r := newWorkerRegistry()
	w := r.Register(Worker{Identity: "w1", Namespace: "default", TaskQueue: "q"})
	if w == nil || w.FirstSeen == "" {
		t.Fatalf("first seen not set")
	}
	first := w.FirstSeen
	time.Sleep(2 * time.Millisecond)
	w2 := r.Register(Worker{Identity: "w1", Namespace: "default", TaskQueue: "q"})
	if w2.FirstSeen != first {
		t.Fatalf("first seen reset on re-register")
	}
	if got, ok := r.Get("default", "w1"); !ok || got.Identity != "w1" {
		t.Fatalf("get failed")
	}
	if _, ok := r.Get("default", "ghost"); ok {
		t.Fatalf("ghost reported as found")
	}
	if got := r.List("default"); len(got) != 1 {
		t.Fatalf("list len=%d", len(got))
	}
}

// ── HTTP table-driven tests ─────────────────────────────────────────

func TestHTTP_Settings(t *testing.T) {
	_, h := httpFixture(t)
	code, body := httpDo(t, h, http.MethodGet, "/v1/tasks/settings", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if _, ok := body["version"]; !ok {
		t.Fatalf("no version in settings")
	}
	caps, ok := body["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("no capabilities")
	}
	if caps["supportsSchedules"] != true {
		t.Fatalf("supportsSchedules missing")
	}
	// Method-not-GET → 404.
	code, _ = httpDo(t, h, http.MethodPost, "/v1/tasks/settings", nil)
	if code != 404 {
		t.Fatalf("expected 404 for POST, got %d", code)
	}
}

func TestHTTP_ScheduleTrigger(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.CreateSchedule(Schedule{
		ScheduleId: "sc-1", Namespace: "default",
		Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: ScheduleAction{WorkflowType: TypeRef{Name: "Demo"}, TaskQueue: "q"},
	})
	cases := []struct {
		name string
		path string
		body map[string]any
		code int
	}{
		{"ok", "/v1/tasks/namespaces/default/schedules/sc-1/trigger", map[string]any{"requestId": "r1"}, 200},
		{"idempotent", "/v1/tasks/namespaces/default/schedules/sc-1/trigger", map[string]any{"requestId": "r1"}, 200},
		{"unknown", "/v1/tasks/namespaces/default/schedules/ghost/trigger", map[string]any{}, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := httpDo(t, h, http.MethodPost, c.path, c.body)
			if code != c.code {
				t.Fatalf("code=%d want %d", code, c.code)
			}
		})
	}
	s, _, _ := en.DescribeSchedule("default", "sc-1")
	if s.Info.ActionCount != 1 {
		t.Fatalf("idempotency broke at HTTP layer: actionCount=%d", s.Info.ActionCount)
	}
}

func TestHTTP_ScheduleUpdate(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.CreateSchedule(Schedule{
		ScheduleId: "sc-up", Namespace: "default",
		Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: ScheduleAction{WorkflowType: TypeRef{Name: "Demo"}, TaskQueue: "qa"},
	})
	body := map[string]any{
		"spec":   map[string]any{"cronString": []string{"@every 30m"}},
		"action": map[string]any{"workflowType": map[string]string{"name": "Demo2"}, "taskQueue": "qb"},
		"state":  map[string]any{"paused": true, "note": "ops"},
	}
	code, _ := httpDo(t, h, http.MethodPost, "/v1/tasks/namespaces/default/schedules/sc-up", body)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	s, _, _ := en.DescribeSchedule("default", "sc-up")
	if s.Action.TaskQueue != "qb" {
		t.Fatalf("update did not apply")
	}
	// 404
	code, _ = httpDo(t, h, http.MethodPost, "/v1/tasks/namespaces/default/schedules/ghost", body)
	if code != 404 {
		t.Fatalf("ghost code=%d", code)
	}
}

func TestHTTP_ScheduleMatchingTimes(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.CreateSchedule(Schedule{
		ScheduleId: "sc-mt", Namespace: "default",
		Spec:   ScheduleSpec{CronString: []string{"@every 1h"}},
		Action: ScheduleAction{WorkflowType: TypeRef{Name: "Demo"}, TaskQueue: "default"},
	})
	now := time.Now().UTC().Format(time.RFC3339)
	to := time.Now().UTC().Add(2*time.Hour + 30*time.Minute).Format(time.RFC3339)
	url := "/v1/tasks/namespaces/default/schedules/sc-mt/matching-times?from=" + now + "&to=" + to
	code, body := httpDo(t, h, http.MethodGet, url, nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	mt, ok := body["matchingTimes"].([]any)
	if !ok || len(mt) < 1 {
		t.Fatalf("matchingTimes missing/empty: %+v", body)
	}
}

func TestHTTP_DeploymentSetCurrent(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.CreateDeployment(Deployment{
		Namespace: "default", SeriesName: "svc-x",
		BuildIDs:       []DeploymentBuild{{BuildId: "v1"}, {BuildId: "v2"}},
		DefaultBuildId: "v1",
	})
	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"ok", map[string]any{"buildId": "v2"}, 200},
		{"idempotent", map[string]any{"buildId": "v2"}, 200},
		{"reject-unknown", map[string]any{"buildId": "v99"}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := httpDo(t, h, http.MethodPost, "/v1/tasks/namespaces/default/deployments/svc-x/set-current", c.body)
			if code != c.code {
				t.Fatalf("code=%d want %d", code, c.code)
			}
		})
	}
}

func TestHTTP_Archival(t *testing.T) {
	_, h := httpFixture(t)
	code, body := httpDo(t, h, http.MethodGet, "/v1/tasks/namespaces/default/archival", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if body["enabled"] != false {
		t.Fatalf("expected enabled=false")
	}
}

func TestHTTP_WorkflowExecutionsChain(t *testing.T) {
	en, h := httpFixture(t)
	_, _ = en.StartWorkflow("default", "wf-chain", "", TypeRef{Name: "Demo"}, "q", nil)
	code, body := httpDo(t, h, http.MethodGet, "/v1/tasks/namespaces/default/workflows/wf-chain/executions", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if _, ok := body["executions"]; !ok {
		t.Fatalf("no executions field")
	}
}

func TestHTTP_TaskQueuePartitions(t *testing.T) {
	_, h := httpFixture(t)
	code, body := httpDo(t, h, http.MethodGet, "/v1/tasks/namespaces/default/task-queues/q/partitions", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	parts, ok := body["partitions"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("expected 1 partition, got %+v", body)
	}
}

func TestHTTP_WorkerDetail(t *testing.T) {
	en, h := httpFixture(t)
	en.workers.Register(Worker{Identity: "w-1", Namespace: "default", TaskQueue: "q", SDKName: "go", SDKVersion: "1"})
	cases := []struct {
		name string
		path string
		code int
	}{
		{"ok", "/v1/tasks/namespaces/default/workers/w-1", 200},
		{"unknown", "/v1/tasks/namespaces/default/workers/ghost", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _ := httpDo(t, h, http.MethodGet, c.path, nil)
			if code != c.code {
				t.Fatalf("code=%d want %d", code, c.code)
			}
		})
	}
}

func TestHTTP_NexusDelete(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.CreateNexusEndpoint(NexusEndpoint{Name: "e1", Namespace: "default", Target: "ns2://x/y"})
	code, _ := httpDo(t, h, http.MethodDelete, "/v1/tasks/namespaces/default/nexus/e1", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
}

func TestHTTP_IdentityRevoke(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.GrantIdentity(Identity{Email: "z@x", Namespace: "default", Role: "admin"})
	code, _ := httpDo(t, h, http.MethodDelete, "/v1/tasks/namespaces/default/identities/z@x", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
}

func TestHTTP_SearchAttributeDelete(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.AddSearchAttribute("default", SearchAttribute{Name: "Region", Type: "Keyword"})
	code, _ := httpDo(t, h, http.MethodDelete, "/v1/tasks/namespaces/default/search-attributes/Region", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	_ = en
}

func TestHTTP_NamespaceDelete(t *testing.T) {
	_, h := httpFixture(t)
	code, _ := httpDo(t, h, http.MethodDelete, "/v1/tasks/namespaces/default", nil)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
}

func TestHTTP_DeploymentVersionDelete(t *testing.T) {
	en, h := httpFixture(t)
	_ = en.CreateDeployment(Deployment{
		Namespace: "default", SeriesName: "svc-y",
		BuildIDs:       []DeploymentBuild{{BuildId: "v1"}, {BuildId: "v2"}},
		DefaultBuildId: "v1",
	})
	// can't delete current
	code, _ := httpDo(t, h, http.MethodDelete, "/v1/tasks/namespaces/default/deployments/svc-y/versions/v1", nil)
	if code != 404 {
		t.Fatalf("expected 404 for deleting current, got %d", code)
	}
	// can delete non-current
	code, _ = httpDo(t, h, http.MethodDelete, "/v1/tasks/namespaces/default/deployments/svc-y/versions/v2", nil)
	if code != 200 {
		t.Fatalf("delete non-current code=%d", code)
	}
}
