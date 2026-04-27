// Copyright © 2026 Hanzo AI. MIT License.

package examples

import (
	"context"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
)

// SignalResult captures the lifecycle of a signal-driven workflow.
type SignalResult struct {
	WorkflowID    string
	RunID         string
	HistoryBefore int64
	HistoryAfter  int64
	FinalStatus   int
}

// Signal drives a workflow + a signal + a terminate. The engine doesn't
// run the workflow's signal channel goroutine yet, but the history
// length must increment on each signal.
func Signal(ctx context.Context, c client.Client, _ string) (*SignalResult, error) {
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "signal-" + time.Now().UTC().Format("150405.000000"),
		TaskQueue: "signals",
	}, "WaitingWorker")
	if err != nil {
		return nil, err
	}
	id, runID := run.GetID(), run.GetRunID()
	before, err := c.DescribeWorkflow(ctx, id, runID)
	if err != nil {
		return nil, err
	}

	if err := c.SignalWorkflow(ctx, id, runID, "go", map[string]any{"step": 1}); err != nil {
		return nil, err
	}
	after, err := c.DescribeWorkflow(ctx, id, runID)
	if err != nil {
		return nil, err
	}

	if err := c.TerminateWorkflow(ctx, id, runID, "test cleanup"); err != nil {
		return nil, err
	}
	final, err := c.DescribeWorkflow(ctx, id, runID)
	if err != nil {
		return nil, err
	}
	return &SignalResult{
		WorkflowID:    id,
		RunID:         runID,
		HistoryBefore: before.HistoryLength,
		HistoryAfter:  after.HistoryLength,
		FinalStatus:   int(final.Status),
	}, nil
}
