// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hanzoai/tasks/pkg/sdk/client"
)

// The request a client's CreateSchedule sends, decoded the way the ZAP
// handler receives it, keeps the action input that each fire passes on.
func TestScheduleFromSDK_KeepsActionInput(t *testing.T) {
	sent := map[string]any{
		"namespace":   "hanzo",
		"schedule_id": "git-cron-update_mirrors",
		"schedule": client.Schedule{
			ID:   "git-cron-update_mirrors",
			Spec: client.ScheduleSpec{Cron: []string{"@every 10m"}},
			Action: client.ScheduleAction{
				WorkflowType: "GitCronJob",
				TaskQueue:    "hanzo-git-cron",
				Input:        []any{"update_mirrors"},
			},
		},
	}
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}

	s := scheduleFromSDK(req, "default")

	if s.ScheduleId != "git-cron-update_mirrors" || s.Namespace != "hanzo" {
		t.Fatalf("identity = %q in %q", s.ScheduleId, s.Namespace)
	}
	if s.Action.WorkflowType.Name != "GitCronJob" || s.Action.TaskQueue != "hanzo-git-cron" {
		t.Fatalf("action = %+v", s.Action)
	}
	if !reflect.DeepEqual(s.Action.Input, []any{"update_mirrors"}) {
		t.Fatalf("input = %#v, want [update_mirrors]", s.Action.Input)
	}
	if !reflect.DeepEqual(s.Spec.CronString, []string{"@every 10m"}) {
		t.Fatalf("cron = %#v", s.Spec.CronString)
	}
}

// A fired schedule starts its workflow with the action input.
func TestTriggerSchedule_StartsWithActionInput(t *testing.T) {
	en, _ := engineFixture(t)
	req := map[string]any{
		"schedule_id": "job",
		"schedule": map[string]any{
			"spec":   map[string]any{"cron": []any{"@every 10m"}},
			"action": map[string]any{"workflow_type": "GitCronJob", "task_queue": "q", "input": []any{"update_mirrors"}},
		},
	}
	if err := en.CreateSchedule(scheduleFromSDK(req, "default")); err != nil {
		t.Fatal(err)
	}
	wf, err := en.TriggerSchedule("default", "job", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wf.Input, []any{"update_mirrors"}) {
		t.Fatalf("started input = %#v, want [update_mirrors]", wf.Input)
	}
}
