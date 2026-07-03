// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// Phase-2a replay decider. A workflow task carries the run's event-sourced
// history; ExecuteActivity resolves from history by seq. These tests assert
// the decider (a) NEVER runs the activity function in-process (the server
// dispatches it to an activity worker), (b) emits a ScheduleActivity{seq}
// command when the activity's result is not yet in history, (c) returns the
// recorded result without re-dispatch when it is, and (d) is deterministic:
// same history ⇒ same command sequence.

// activityLocalCallCount tracks whether an activity fn was invoked
// in-process. For the replay decider this must stay at zero.
var activityLocalCallCount atomic.Int32

func wireTargetActivity(ctx context.Context, n int) (int, error) {
	activityLocalCallCount.Add(1)
	return n * 2, nil
}

// wireCallingWorkflow calls workflow.ExecuteActivity exactly once and
// returns whatever the Future settles with.
func wireCallingWorkflow(ctx workflow.Context, n int) (int, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		TaskQueue:           "test-queue",
	})
	fut := workflow.ExecuteActivity(ctx, wireTargetActivity, n)
	var out int
	if err := fut.Get(ctx, &out); err != nil {
		return 0, err
	}
	return out, nil
}

// evStarted builds a WORKFLOW_EXECUTION_STARTED event carrying the workflow
// input arguments.
func evStarted(args ...any) map[string]any {
	return map[string]any{
		"eventId":    1,
		"eventType":  evtWorkflowStarted,
		"attributes": map[string]any{"input": args},
	}
}

// evScheduled builds an ACTIVITY_TASK_SCHEDULED{seq} event.
func evScheduled(seq int) map[string]any {
	return map[string]any{
		"eventType":  evtActivityScheduled,
		"attributes": map[string]any{"seq": seq},
	}
}

// evCompleted builds an ACTIVITY_TASK_COMPLETED{seq,result} event. result is
// stored as the raw JSON of the activity's return value, matching the engine.
func evCompleted(t *testing.T, seq int, result any) map[string]any {
	return map[string]any{
		"eventType":  evtActivityCompleted,
		"attributes": map[string]any{"seq": seq, "result": json.RawMessage(mustJSON(t, result))},
	}
}

func historyJSON(t *testing.T, events ...map[string]any) []byte {
	t.Helper()
	return mustJSON(t, events)
}

// replayEpisode runs one decision episode for wfName over history against a
// fresh worker with fns registered by register, and returns the decoded
// command envelope the worker responded with.
func replayEpisode(t *testing.T, register func(w *workerImpl), wfName string, history []byte) commandsEnvelope {
	t.Helper()
	ft := &fakeTransport{}
	w := newTestWorker(t, ft)
	register(w)
	w.dispatchWorkflowTask(context.Background(), &client.WorkflowTask{
		TaskToken:        []byte{0x01},
		WorkflowID:       "wf-replay",
		RunID:            "run-replay",
		WorkflowTypeName: wfName,
		History:          history,
	})
	if ft.workflowCompleted.Load() != 1 || ft.lastWorkflowResp == nil {
		t.Fatalf("expected exactly one RespondWorkflowTaskCompleted; got %d", ft.workflowCompleted.Load())
	}
	return decodeCommandsEnvelope(t, ft.lastWorkflowResp.Commands)
}

// TestReplay_UnresolvedActivity_SchedulesAndBlocks: with no activity result
// in history the decider emits ScheduleActivity{seq=0} and does NOT complete
// the workflow or run the activity in-process.
func TestReplay_UnresolvedActivity_SchedulesAndBlocks(t *testing.T) {
	activityLocalCallCount.Store(0)

	env := replayEpisode(t, func(w *workerImpl) {
		w.RegisterWorkflow(wireCallingWorkflow)
		w.RegisterActivity(wireTargetActivity)
	}, "wireCallingWorkflow", historyJSON(t, evStarted(42)))

	if len(env.Commands) != 1 {
		t.Fatalf("commands = %d, want 1 (scheduleActivity)", len(env.Commands))
	}
	c := env.Commands[0]
	if c.Kind != commandKindScheduleActivity {
		t.Fatalf("command kind = %d, want %d (scheduleActivity)", c.Kind, commandKindScheduleActivity)
	}
	if c.Seq != 0 {
		t.Errorf("seq = %d, want 0", c.Seq)
	}
	if c.ActivityType != "wireTargetActivity" {
		t.Errorf("activityType = %q, want wireTargetActivity", c.ActivityType)
	}
	// input must be the JSON array of args, [42].
	var args []int
	if err := json.Unmarshal(c.Input, &args); err != nil || len(args) != 1 || args[0] != 42 {
		t.Errorf("scheduled input = %q, want [42]", c.Input)
	}
	if got := activityLocalCallCount.Load(); got != 0 {
		t.Fatalf("activity fn invoked in-process %d times; the decider must not run it", got)
	}
}

// TestReplay_ResolvedActivity_ReturnsRecordedResult: with the activity's
// result in history the decider returns it WITHOUT re-dispatch and the
// workflow completes carrying the wire-recorded value.
func TestReplay_ResolvedActivity_ReturnsRecordedResult(t *testing.T) {
	activityLocalCallCount.Store(0)

	env := replayEpisode(t, func(w *workerImpl) {
		w.RegisterWorkflow(wireCallingWorkflow)
		w.RegisterActivity(wireTargetActivity)
	}, "wireCallingWorkflow", historyJSON(t,
		evStarted(42),
		evScheduled(0),
		evCompleted(t, 0, 84), // the activity worker (elsewhere) doubled n=42
	))

	if len(env.Commands) != 1 {
		t.Fatalf("commands = %d, want 1 (completeWorkflow)", len(env.Commands))
	}
	c := env.Commands[0]
	if c.Kind != commandKindCompleteWorkflow {
		t.Fatalf("command kind = %d, want %d (completeWorkflow); failure=%q", c.Kind, commandKindCompleteWorkflow, c.Failure)
	}
	var got int
	if err := json.Unmarshal(c.Result, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got != 84 {
		t.Errorf("workflow result = %d, want 84 (recorded, not recomputed)", got)
	}
	if n := activityLocalCallCount.Load(); n != 0 {
		t.Fatalf("activity fn invoked in-process %d times; recorded result must be reused", n)
	}
}

// -------- 2-activity determinism ------------------------------------------

var stepCalls atomic.Int32

func stepAActivity(ctx context.Context) (string, error) { stepCalls.Add(1); return "A", nil }
func stepBActivity(ctx context.Context) (string, error) { stepCalls.Add(1); return "B", nil }

func twoStepWorkflow(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Second})
	var a string
	if err := workflow.ExecuteActivity(ctx, stepAActivity).Get(ctx, &a); err != nil {
		return "", err
	}
	var b string
	if err := workflow.ExecuteActivity(ctx, stepBActivity).Get(ctx, &b); err != nil {
		return "", err
	}
	return a + b, nil
}

// TestReplay_Determinism_TwoActivities: with only activity 0 resolved in
// history, replay schedules ONLY activity 1 (activity 0 returns its recorded
// result, no re-dispatch); and the same history yields an identical command
// sequence across repeated replays.
func TestReplay_Determinism_TwoActivities(t *testing.T) {
	stepCalls.Store(0)

	history := historyJSON(t,
		evStarted(),
		evScheduled(0),
		evCompleted(t, 0, "A"),
	)
	reg := func(w *workerImpl) {
		w.RegisterWorkflow(twoStepWorkflow)
		w.RegisterActivity(stepAActivity)
		w.RegisterActivity(stepBActivity)
	}

	first := replayEpisode(t, reg, "twoStepWorkflow", history)
	if len(first.Commands) != 1 {
		t.Fatalf("commands = %d, want 1 (only activity 1 scheduled)", len(first.Commands))
	}
	c := first.Commands[0]
	if c.Kind != commandKindScheduleActivity || c.Seq != 1 || c.ActivityType != "stepBActivity" {
		t.Fatalf("first replay command = %+v; want scheduleActivity seq=1 stepBActivity", c)
	}

	// Determinism: replay the identical history again → identical command seq.
	second := replayEpisode(t, reg, "twoStepWorkflow", history)
	if len(second.Commands) != len(first.Commands) {
		t.Fatalf("non-deterministic: first %d commands, second %d", len(first.Commands), len(second.Commands))
	}
	for i := range first.Commands {
		if first.Commands[i].Kind != second.Commands[i].Kind || first.Commands[i].Seq != second.Commands[i].Seq ||
			first.Commands[i].ActivityType != second.Commands[i].ActivityType {
			t.Fatalf("non-deterministic command %d: %+v vs %+v", i, first.Commands[i], second.Commands[i])
		}
	}

	if stepCalls.Load() != 0 {
		t.Fatalf("activity fns invoked in-process %d times; the decider must not run them", stepCalls.Load())
	}
}

// mustJSON is a test helper that json.Marshals v or fails the test.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// decodeCommandsEnvelope parses the producer-side wire shape for
// assertion. Shared across the worker dispatch tests.
func decodeCommandsEnvelope(t *testing.T, b []byte) commandsEnvelope {
	t.Helper()
	var e commandsEnvelope
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("decode envelope: %v (raw=%q)", err, b)
	}
	return e
}
