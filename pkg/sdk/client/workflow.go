package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// WorkflowRun is the handle returned by ExecuteWorkflow. Get blocks until
// the workflow terminates and decodes the result into `valuePtr`.
type WorkflowRun interface {
	Get(ctx context.Context, valuePtr any) error
	GetID() string
	GetRunID() string
}

// WorkflowExecutionInfo mirrors schema/tasks.zap WorkflowExecutionInfo
// for the v1 JSON wire. Exported so callers of DescribeWorkflow /
// ListWorkflows can inspect the result without pulling upstream types.
type WorkflowExecutionInfo struct {
	WorkflowID    string            `json:"workflow_id"`
	RunID         string            `json:"run_id"`
	WorkflowType  string            `json:"workflow_type"`
	StartTime     time.Time         `json:"start_time,omitempty"`
	CloseTime     time.Time         `json:"close_time,omitempty"`
	Status        WorkflowStatus    `json:"status"`
	HistoryLength int64             `json:"history_length"`
	TaskQueue     string            `json:"task_queue,omitempty"`
	Memo          map[string]any    `json:"memo,omitempty"`
}

// WorkflowStatus mirrors the `status` Int8 in schema/tasks.zap.
type WorkflowStatus int8

const (
	WorkflowStatusUnspecified     WorkflowStatus = 0
	WorkflowStatusRunning         WorkflowStatus = 1
	WorkflowStatusCompleted       WorkflowStatus = 2
	WorkflowStatusFailed          WorkflowStatus = 3
	WorkflowStatusCanceled        WorkflowStatus = 4
	WorkflowStatusTerminated      WorkflowStatus = 5
	WorkflowStatusContinuedAsNew  WorkflowStatus = 6
	WorkflowStatusTimedOut        WorkflowStatus = 7
)

// ListWorkflowsResponse is returned by ListWorkflows.
type ListWorkflowsResponse struct {
	Executions    []WorkflowExecutionInfo `json:"executions"`
	NextPageToken []byte                  `json:"next_page_token,omitempty"`
}

// startWorkflowRequest is the v1 JSON shape mapped onto schema/tasks.zap
// StartWorkflowRequest. Native ZAP serde replaces JSON in a follow-up.
type startWorkflowRequest struct {
	Namespace    string       `json:"namespace"`
	WorkflowID   string       `json:"workflow_id"`
	WorkflowType string       `json:"workflow_type"`
	TaskQueue    string       `json:"task_queue"`
	Input        []any        `json:"input,omitempty"`
	RetryPolicy  *retryPolicy `json:"retry_policy,omitempty"`
	Timeouts     timeouts     `json:"timeouts,omitempty"`
	Memo         map[string]any `json:"memo,omitempty"`
	CronSchedule string       `json:"cron_schedule,omitempty"`
	Identity     string       `json:"identity,omitempty"`
}

type retryPolicy struct {
	InitialIntervalMs      int64    `json:"initial_interval_ms,omitempty"`
	BackoffCoefficient     float64  `json:"backoff_coefficient,omitempty"`
	MaximumIntervalMs      int64    `json:"maximum_interval_ms,omitempty"`
	MaximumAttempts        int32    `json:"maximum_attempts,omitempty"`
	NonRetryableErrorTypes []string `json:"non_retryable_error_types,omitempty"`
}

type timeouts struct {
	WorkflowExecutionMs int64 `json:"workflow_execution_ms,omitempty"`
	WorkflowRunMs       int64 `json:"workflow_run_ms,omitempty"`
	WorkflowTaskMs      int64 `json:"workflow_task_ms,omitempty"`
}

type startWorkflowResponse struct {
	RunID string `json:"run_id"`
}

type signalWorkflowRequest struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	SignalName string `json:"signal_name"`
	Input      any    `json:"input,omitempty"`
}

type cancelWorkflowRequest struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type terminateWorkflowRequest struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type describeWorkflowRequest struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
}

type describeWorkflowResponse struct {
	Info WorkflowExecutionInfo `json:"info"`
}

type listWorkflowsRequest struct {
	Namespace     string `json:"namespace"`
	Query         string `json:"query,omitempty"`
	PageSize      int32  `json:"page_size,omitempty"`
	NextPageToken []byte `json:"next_page_token,omitempty"`
}

// ExecuteWorkflow implements Client.
func (c *clientImpl) ExecuteWorkflow(
	ctx context.Context,
	opts StartWorkflowOptions,
	workflow any,
	args ...any,
) (WorkflowRun, error) {
	if opts.TaskQueue == "" {
		return nil, errors.New("hanzo/tasks/client: StartWorkflowOptions.TaskQueue is required")
	}
	wfType, err := workflowTypeName(workflow)
	if err != nil {
		return nil, err
	}

	req := startWorkflowRequest{
		Namespace:    c.namespace,
		WorkflowID:   opts.ID,
		WorkflowType: wfType,
		TaskQueue:    opts.TaskQueue,
		Input:        args,
		CronSchedule: opts.CronSchedule,
		Memo:         opts.Memo,
		Identity:     c.identity,
		Timeouts: timeouts{
			WorkflowExecutionMs: opts.WorkflowExecutionTimeout.Milliseconds(),
			WorkflowRunMs:       opts.WorkflowRunTimeout.Milliseconds(),
			WorkflowTaskMs:      opts.WorkflowTaskTimeout.Milliseconds(),
		},
	}
	if rp := opts.RetryPolicy; rp != nil {
		req.RetryPolicy = &retryPolicy{
			InitialIntervalMs:      rp.InitialInterval.Milliseconds(),
			BackoffCoefficient:     rp.BackoffCoefficient,
			MaximumIntervalMs:      rp.MaximumInterval.Milliseconds(),
			MaximumAttempts:        rp.MaximumAttempts,
			NonRetryableErrorTypes: append([]string(nil), rp.NonRetryableErrorTypes...),
		}
	}

	var resp startWorkflowResponse
	if err := c.roundTrip(ctx, opStartWorkflow, req, &resp); err != nil {
		return nil, err
	}
	id := opts.ID
	if id == "" {
		// Server generated the ID; we have no way to learn it from the
		// current response shape. The runId is authoritative either way.
		id = resp.RunID
	}
	return &workflowRunImpl{id: id, runID: resp.RunID, client: c}, nil
}

// SignalWorkflow implements Client.
func (c *clientImpl) SignalWorkflow(
	ctx context.Context,
	workflowID, runID, signalName string,
	arg any,
) error {
	if workflowID == "" {
		return errors.New("hanzo/tasks/client: workflowID is required")
	}
	if signalName == "" {
		return errors.New("hanzo/tasks/client: signalName is required")
	}
	req := signalWorkflowRequest{
		Namespace:  c.namespace,
		WorkflowID: workflowID,
		RunID:      runID,
		SignalName: signalName,
		Input:      arg,
	}
	return c.roundTrip(ctx, opSignalWorkflow, req, nil)
}

// CancelWorkflow implements Client.
func (c *clientImpl) CancelWorkflow(ctx context.Context, workflowID, runID string) error {
	if workflowID == "" {
		return errors.New("hanzo/tasks/client: workflowID is required")
	}
	req := cancelWorkflowRequest{
		Namespace:  c.namespace,
		WorkflowID: workflowID,
		RunID:      runID,
	}
	return c.roundTrip(ctx, opCancelWorkflow, req, nil)
}

// TerminateWorkflow implements Client.
func (c *clientImpl) TerminateWorkflow(ctx context.Context, workflowID, runID, reason string) error {
	if workflowID == "" {
		return errors.New("hanzo/tasks/client: workflowID is required")
	}
	req := terminateWorkflowRequest{
		Namespace:  c.namespace,
		WorkflowID: workflowID,
		RunID:      runID,
		Reason:     reason,
	}
	return c.roundTrip(ctx, opTerminateWorkflow, req, nil)
}

// DescribeWorkflow implements Client.
func (c *clientImpl) DescribeWorkflow(ctx context.Context, workflowID, runID string) (*WorkflowExecutionInfo, error) {
	if workflowID == "" {
		return nil, errors.New("hanzo/tasks/client: workflowID is required")
	}
	req := describeWorkflowRequest{
		Namespace:  c.namespace,
		WorkflowID: workflowID,
		RunID:      runID,
	}
	var resp describeWorkflowResponse
	if err := c.roundTrip(ctx, opDescribeWorkflow, req, &resp); err != nil {
		return nil, err
	}
	out := resp.Info
	return &out, nil
}

// ListWorkflows implements Client.
func (c *clientImpl) ListWorkflows(ctx context.Context, query string, pageSize int32, nextPageToken []byte) (*ListWorkflowsResponse, error) {
	req := listWorkflowsRequest{
		Namespace:     c.namespace,
		Query:         query,
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}
	var resp ListWorkflowsResponse
	if err := c.roundTrip(ctx, opListWorkflows, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// workflowRunImpl is the handle returned by ExecuteWorkflow.
type workflowRunImpl struct {
	id     string
	runID  string
	client *clientImpl
}

// GetID returns the workflow ID.
func (r *workflowRunImpl) GetID() string { return r.id }

// GetRunID returns the run ID.
func (r *workflowRunImpl) GetRunID() string { return r.runID }

// Get blocks until the workflow terminates and decodes the result into
// valuePtr.
//
// NOTE (v1 gap, tracked in schema/tasks.zap): the schema does not yet
// declare a history-fetch RPC (opcode 0x006A reserved). Until pkg/sdk/worker
// lands with a GetWorkflowHistory / GetWorkflowResult RPC, the best we can
// do is describe the workflow, wait for a terminal status, and decode the
// result payload if the server ships one in the DescribeWorkflowResponse.
// The describe struct does not carry `result` today, so Get returns
// ErrNotImplementedLocally when a result was requested.
//
// Callers that only need to block until termination should use ctx with
// a deadline; errors other than ErrNotImplementedLocally indicate the
// workflow did not reach a successful terminal state.
func (r *workflowRunImpl) Get(ctx context.Context, valuePtr any) error {
	// Poll DescribeWorkflow until terminal. The cadence is modest to
	// avoid hammering the server; the server push-history RPC will
	// replace this once pkg/sdk/worker exists.
	backoff := 250 * time.Millisecond
	const maxBackoff = 5 * time.Second

	for {
		info, err := r.client.DescribeWorkflow(ctx, r.id, r.runID)
		if err != nil {
			return err
		}
		switch info.Status {
		case WorkflowStatusCompleted:
			if valuePtr == nil {
				return nil
			}
			// v1 schema: no result field on DescribeWorkflowResponse.
			// Surface the gap explicitly rather than silently returning
			// a zero value.
			return ErrNotImplementedLocally
		case WorkflowStatusFailed:
			return fmt.Errorf("workflow failed: %s", info.WorkflowID)
		case WorkflowStatusCanceled:
			return fmt.Errorf("workflow canceled: %s", info.WorkflowID)
		case WorkflowStatusTerminated:
			return fmt.Errorf("workflow terminated: %s", info.WorkflowID)
		case WorkflowStatusTimedOut:
			return fmt.Errorf("workflow timed out: %s", info.WorkflowID)
		case WorkflowStatusContinuedAsNew:
			return fmt.Errorf("workflow continued as new: %s", info.WorkflowID)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			if backoff < maxBackoff {
				backoff *= 2
			}
		}
	}
}

// workflowTypeName returns the wire name for a workflow registered with
// ExecuteWorkflow. String arguments are used as-is; function arguments
// are reflected down to their qualified Go name (pkg.FuncName) and then
// trimmed to the final identifier, matching Temporal's historical
// default and what our server's registry indexes on.
func workflowTypeName(workflow any) (string, error) {
	if workflow == nil {
		return "", errors.New("hanzo/tasks/client: workflow is nil")
	}
	if name, ok := workflow.(string); ok {
		if name == "" {
			return "", errors.New("hanzo/tasks/client: empty workflow type")
		}
		return name, nil
	}
	v := reflect.ValueOf(workflow)
	if v.Kind() != reflect.Func {
		return "", fmt.Errorf("hanzo/tasks/client: workflow must be a string or func, got %T", workflow)
	}
	full := runtime.FuncForPC(v.Pointer()).Name()
	// Strip to short identifier: github.com/foo/pkg.Fn -> Fn.
	if idx := strings.LastIndex(full, "."); idx >= 0 {
		return full[idx+1:], nil
	}
	return full, nil
}

// Compile-time check.
var _ = json.Marshal
