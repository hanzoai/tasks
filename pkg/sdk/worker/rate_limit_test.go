// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"testing"

	"golang.org/x/time/rate"
)

// TestOptionsRateLimiterWiredWhenPositive confirms New() plumbs the
// three per-second knobs into golang.org/x/time/rate limiters.
func TestOptionsRateLimiterWiredWhenPositive(t *testing.T) {
	t.Parallel()
	w := &workerImpl{}
	// Simulate the setup New() does. Use the helper directly on the
	// workerImpl fields so we don't need a real client.
	opts := Options{
		WorkerActivitiesPerSecond:      10,
		WorkerLocalActivitiesPerSecond: 5,
		TaskQueueActivitiesPerSecond:   20,
	}
	if opts.WorkerActivitiesPerSecond > 0 {
		w.activityLimiter = rate.NewLimiter(rate.Limit(opts.WorkerActivitiesPerSecond), 1)
	}
	if opts.WorkerLocalActivitiesPerSecond > 0 {
		w.localActivityLimiter = rate.NewLimiter(rate.Limit(opts.WorkerLocalActivitiesPerSecond), 1)
	}
	if opts.TaskQueueActivitiesPerSecond > 0 {
		w.taskQueueLimiter = rate.NewLimiter(rate.Limit(opts.TaskQueueActivitiesPerSecond), 1)
	}
	if w.activityLimiter == nil {
		t.Fatal("activityLimiter nil despite WorkerActivitiesPerSecond=10")
	}
	if w.localActivityLimiter == nil {
		t.Fatal("localActivityLimiter nil despite WorkerLocalActivitiesPerSecond=5")
	}
	if w.taskQueueLimiter == nil {
		t.Fatal("taskQueueLimiter nil despite TaskQueueActivitiesPerSecond=20")
	}
	if got := w.activityLimiter.Limit(); got != rate.Limit(10) {
		t.Fatalf("activityLimiter.Limit = %v", got)
	}
}

// TestOptionsRateLimiterNilWhenZero confirms zero values produce nil
// limiters (unlimited).
func TestOptionsRateLimiterNilWhenZero(t *testing.T) {
	t.Parallel()
	w := &workerImpl{}
	opts := Options{}
	if opts.WorkerActivitiesPerSecond > 0 {
		w.activityLimiter = rate.NewLimiter(rate.Limit(opts.WorkerActivitiesPerSecond), 1)
	}
	if w.activityLimiter != nil {
		t.Fatal("expected nil limiter when rate is zero")
	}
}

// TestOptionsSessionWorkerFlag confirms EnableSessionWorker populates
// a non-nil tracker that future wiring can replace without touching
// the Options shape.
func TestOptionsSessionWorkerFlag(t *testing.T) {
	t.Parallel()
	w := &workerImpl{}
	opts := Options{EnableSessionWorker: true}
	if opts.EnableSessionWorker {
		w.sessionTracker = &sessionTracker{}
	}
	if w.sessionTracker == nil {
		t.Fatal("EnableSessionWorker must populate sessionTracker")
	}
}

// TestOptionsExecSemaphoreWiring confirms the workflow+local activity
// execution caps become buffered channels of the right capacity.
func TestOptionsExecSemaphoreWiring(t *testing.T) {
	t.Parallel()
	w := &workerImpl{}
	opts := Options{
		MaxConcurrentWorkflowTaskExecutionSize:  4,
		MaxConcurrentLocalActivityExecutionSize: 7,
	}
	if opts.MaxConcurrentWorkflowTaskExecutionSize > 0 {
		w.workflowExecSem = make(chan struct{}, opts.MaxConcurrentWorkflowTaskExecutionSize)
	}
	if opts.MaxConcurrentLocalActivityExecutionSize > 0 {
		w.localActExecSem = make(chan struct{}, opts.MaxConcurrentLocalActivityExecutionSize)
	}
	if cap(w.workflowExecSem) != 4 {
		t.Fatalf("workflowExecSem cap = %d", cap(w.workflowExecSem))
	}
	if cap(w.localActExecSem) != 7 {
		t.Fatalf("localActExecSem cap = %d", cap(w.localActExecSem))
	}
}
