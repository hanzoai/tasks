// Copyright © 2026 Hanzo AI. MIT License.

// Package examples — workflow patterns that exercise the Hanzo Tasks
// engine end-to-end via the native ZAP SDK at pkg/sdk/client. Zero
// go.temporal.io/* and zero google.golang.org/grpc imports.
//
// The samples run against an embedded tasksd boot (see _test.go) so
// they double as the integration test corpus for every shipped
// release.
package examples

import (
	"context"
	"errors"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
)

// Hello starts a no-input workflow and confirms its execution record
// is round-trippable across the full client surface: ExecuteWorkflow →
// DescribeWorkflow → ListWorkflows → CancelWorkflow → DescribeWorkflow.
//
// The returned Result captures the lifecycle so tests can assert on
// each transition without re-issuing the calls.
type HelloResult struct {
	StartedID    string
	StartedRunID string
	BeforeStatus int
	AfterStatus  int
	ListedCount  int
}

// Hello drives the basic workflow lifecycle. It never blocks on
// workflow completion (the engine doesn't execute user code yet);
// it only verifies the control-plane round-trips end-to-end.
func Hello(ctx context.Context, c client.Client, namespace string) (*HelloResult, error) {
	if c == nil {
		return nil, errors.New("examples: client is nil")
	}
	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "hello-" + time.Now().UTC().Format("150405.000000"),
		TaskQueue: "default",
	}, "Hello")
	if err != nil {
		return nil, err
	}
	id, runID := run.GetID(), run.GetRunID()
	res := &HelloResult{StartedID: id, StartedRunID: runID}

	info, err := c.DescribeWorkflow(ctx, id, runID)
	if err != nil {
		return nil, err
	}
	res.BeforeStatus = int(info.Status)

	list, err := c.ListWorkflows(ctx, "", 100, nil)
	if err != nil {
		return nil, err
	}
	for _, e := range list.Executions {
		if e.WorkflowID == id {
			res.ListedCount++
		}
	}

	if err := c.CancelWorkflow(ctx, id, runID); err != nil {
		return nil, err
	}
	info2, err := c.DescribeWorkflow(ctx, id, runID)
	if err != nil {
		return nil, err
	}
	res.AfterStatus = int(info2.Status)

	return res, nil
}
