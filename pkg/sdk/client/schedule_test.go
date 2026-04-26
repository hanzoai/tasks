// Copyright © 2026 Hanzo AI. MIT License.

package client

import (
	"context"
	"testing"
	"time"
)

// TestUpdateScheduleEmitsCorrectOpcodeAndBody asserts UpdateSchedule
// hits opcode 0x0074 with the new schedule body.
func TestUpdateScheduleEmitsCorrectOpcodeAndBody(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, err := Dial(Options{Namespace: "default", Transport: st})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	err = c.UpdateSchedule(context.Background(), UpdateScheduleOptions{
		ID: "sched-1",
		Schedule: Schedule{
			ID: "sched-1",
			Spec: ScheduleSpec{
				Interval: 30 * time.Second,
			},
			Action: ScheduleAction{
				WorkflowType: "Tick",
				TaskQueue:    "ticks",
			},
		},
	})
	if err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	if st.gotOp != opUpdateSchedule {
		t.Fatalf("opcode = 0x%04x, want 0x%04x", st.gotOp, opUpdateSchedule)
	}
	var req map[string]any
	if err := decodeStubRequestBody(st.gotBody, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req["schedule_id"] != "sched-1" {
		t.Fatalf("schedule_id missing: %v", req["schedule_id"])
	}
}

// TestTriggerScheduleEmitsCorrectOpcodeAndPolicy asserts TriggerSchedule
// hits opcode 0x0075 and propagates the overlap policy.
func TestTriggerScheduleEmitsCorrectOpcodeAndPolicy(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()

	if err := c.TriggerSchedule(context.Background(), "sched-2", "skip"); err != nil {
		t.Fatalf("TriggerSchedule: %v", err)
	}
	if st.gotOp != opTriggerSchedule {
		t.Fatalf("opcode = 0x%04x, want 0x%04x", st.gotOp, opTriggerSchedule)
	}
	var req map[string]any
	if err := decodeStubRequestBody(st.gotBody, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req["schedule_id"] != "sched-2" {
		t.Fatalf("schedule_id: %v", req["schedule_id"])
	}
	if req["overlap_policy"] != "skip" {
		t.Fatalf("overlap_policy: %v", req["overlap_policy"])
	}
}

// TestDescribeScheduleEmitsCorrectOpcodeAndDecodesResponse asserts the
// round trip on opcode 0x0076 and the response wire shape.
func TestDescribeScheduleEmitsCorrectOpcodeAndDecodesResponse(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{
		"schedule": map[string]any{
			"id":     "sched-3",
			"paused": true,
			"action": map[string]any{
				"workflow_type": "Tick",
				"task_queue":    "ticks",
			},
		},
		"info": map[string]any{
			"action_count":    int64(7),
			"overlap_skipped": int64(2),
		},
	}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()

	resp, err := c.DescribeSchedule(context.Background(), "sched-3")
	if err != nil {
		t.Fatalf("DescribeSchedule: %v", err)
	}
	if st.gotOp != opDescribeSchedule {
		t.Fatalf("opcode = 0x%04x, want 0x%04x", st.gotOp, opDescribeSchedule)
	}
	if resp.Schedule.ID != "sched-3" {
		t.Fatalf("schedule.id = %q, want sched-3", resp.Schedule.ID)
	}
	if !resp.Schedule.Paused {
		t.Fatalf("schedule.paused must be true")
	}
	if resp.Info.ActionCount != 7 {
		t.Fatalf("info.action_count = %d, want 7", resp.Info.ActionCount)
	}
	if resp.Info.OverlapSkipped != 2 {
		t.Fatalf("info.overlap_skipped = %d, want 2", resp.Info.OverlapSkipped)
	}
}

// TestUnpauseScheduleAlias asserts UnpauseSchedule lowers to PauseSchedule
// with paused=false.
func TestUnpauseScheduleAlias(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()

	if err := c.UnpauseSchedule(context.Background(), "sched-4"); err != nil {
		t.Fatalf("UnpauseSchedule: %v", err)
	}
	if st.gotOp != opPauseSchedule {
		t.Fatalf("opcode = 0x%04x, want 0x%04x", st.gotOp, opPauseSchedule)
	}
	var req map[string]any
	if err := decodeStubRequestBody(st.gotBody, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req["paused"] != false {
		t.Fatalf("paused must be false: %v", req["paused"])
	}
}

// TestUpdateScheduleRequiresID rejects empty IDs.
func TestUpdateScheduleRequiresID(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()

	if err := c.UpdateSchedule(context.Background(), UpdateScheduleOptions{}); err == nil {
		t.Fatalf("expected error on empty ID")
	}
	if st.callCount != 0 {
		t.Fatalf("expected no transport calls; got %d", st.callCount)
	}
}

// TestTriggerScheduleRequiresID rejects empty IDs.
func TestTriggerScheduleRequiresID(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()

	if err := c.TriggerSchedule(context.Background(), "", "skip"); err == nil {
		t.Fatalf("expected error on empty ID")
	}
	if st.callCount != 0 {
		t.Fatalf("expected no transport calls; got %d", st.callCount)
	}
}

// TestDescribeScheduleRequiresID rejects empty IDs.
func TestDescribeScheduleRequiresID(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()

	if _, err := c.DescribeSchedule(context.Background(), ""); err == nil {
		t.Fatalf("expected error on empty ID")
	}
	if st.callCount != 0 {
		t.Fatalf("expected no transport calls; got %d", st.callCount)
	}
}
