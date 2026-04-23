// Copyright © 2026 Hanzo AI. MIT License.

package workflow

import (
	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// ChildWorkflowFuture is the handle returned by ExecuteChildWorkflow.
// It embeds Future (whose Get / IsReady / ReadyCh observe the child's
// final result) and adds GetChildWorkflowExecution, a secondary Future
// that settles with the child's {WorkflowID, RunID} as soon as the
// server mints the run.
//
// Matches go.temporal.io/sdk/workflow.ChildWorkflowFuture shape-for-
// shape so caller code migrating from the upstream package compiles
// unchanged after the import swap.
type ChildWorkflowFuture interface {
	Future

	// GetChildWorkflowExecution returns a Future that settles with
	// the child's WorkflowExecution ({WorkflowID, RunID}). It is
	// settled by the worker as soon as the frontend acknowledges
	// the schedule — typically well before the child's result
	// future settles.
	GetChildWorkflowExecution() Future
}

// WorkflowExecution is the child-side identifier pair. Declared here
// (rather than in client/) so the workflow package never imports the
// client package — keeping the dependency graph a strict line:
//
//	workflow → temporal
//	worker → {workflow, client, temporal, activity}
//	client → {temporal, converter}
type WorkflowExecution struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
}

// childFuture is the default ChildWorkflowFuture implementation. The
// result future and the execution future are independent; the worker
// settles the execution future when it receives the server's schedule
// ack, and the result future when it receives the child's terminal
// event (or when the startChild RPC itself fails).
type childFuture struct {
	result    Settleable
	execution Settleable
}

// NewChildWorkflowFuture constructs an unsettled ChildWorkflowFuture.
// The worker owns both halves; user code consumes the returned Future.
func NewChildWorkflowFuture() (ChildWorkflowFuture, Settleable, Settleable) {
	result := NewFuture()
	execution := NewFuture()
	cf := &childFuture{result: result, execution: execution}
	return cf, result, execution
}

// Get blocks on the result future.
func (f *childFuture) Get(ctx Context, valPtr any) error {
	return f.result.Get(ctx, valPtr)
}

func (f *childFuture) IsReady() bool                       { return f.result.IsReady() }
func (f *childFuture) ReadyCh() <-chan struct{}            { return f.result.ReadyCh() }
func (f *childFuture) GetChildWorkflowExecution() Future   { return f.execution }

// ExecuteChildWorkflow is the workflow-code entry point. It delegates
// to CoroutineEnv.ExecuteChildWorkflow, which in workerEnv forwards
// the request over ZAP. A misconfigured env (nil / StubEnv with no
// child registered) surfaces a typed error on result.Get.
func ExecuteChildWorkflow(ctx Context, childWorkflow any, args ...any) ChildWorkflowFuture {
	if ctx == nil {
		cf, result, exec := NewChildWorkflowFuture()
		result.Settle(nil, errNilContext)
		exec.Settle(nil, errNilContext)
		return cf
	}
	if childWorkflow == nil {
		cf, result, exec := NewChildWorkflowFuture()
		err := temporal.NewError("nil childWorkflow", temporal.CodeApplication, true)
		result.Settle(nil, err)
		exec.Settle(nil, err)
		return cf
	}
	return ctx.env().ExecuteChildWorkflow(childWorkflow, args)
}
