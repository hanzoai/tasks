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

	"github.com/hanzoai/tasks/pkg/sdk/converter"
)

// WorkflowRun is the handle returned by ExecuteWorkflow /
// SignalWithStartWorkflow / GetWorkflow. Get blocks until the workflow
// terminates and decodes the result into `valuePtr`.
type WorkflowRun interface {
	// Get blocks until the workflow terminates and decodes the
	// result into valuePtr. When the workflow continues-as-new the
	// default behaviour follows the chain to the final run.
	Get(ctx context.Context, valuePtr any) error

	// GetWithOptions is the option-parameterised form of Get. When
	// opts.DisableFollowingRuns is true, Get stops at the current
	// run rather than following a continue-as-new chain to the final
	// run.
	GetWithOptions(ctx context.Context, valuePtr any, opts WorkflowRunGetOptions) error

	GetID() string
	GetRunID() string
}

// WorkflowRunGetOptions tunes WorkflowRun.GetWithOptions. Mirrors the
// upstream shape so caller code compiles unchanged after the import
// swap.
type WorkflowRunGetOptions struct {
	// DisableFollowingRuns, when true, prevents Get from walking
	// the continue-as-new chain. It returns when the current run
	// terminates even if a successor run was started.
	DisableFollowingRuns bool
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

// Get is GetWithOptions(ctx, valuePtr, zero-options). It follows the
// continue-as-new chain to the final run.
func (r *workflowRunImpl) Get(ctx context.Context, valuePtr any) error {
	return r.GetWithOptions(ctx, valuePtr, WorkflowRunGetOptions{})
}

// GetWithOptions blocks until the workflow terminates and decodes the
// result into valuePtr. Poll cadence is exponential (250ms → 5s cap) so
// the v1 wire does not need a dedicated long-poll history-fetch RPC.
//
// NOTE (v1 gap, tracked in schema/tasks.zap): DescribeWorkflowResponse
// does not yet carry a `result` field, so a completed-workflow result
// decode surfaces ErrNotImplementedLocally. Callers that only need to
// block until termination pass valuePtr=nil.
//
// opts.DisableFollowingRuns, when true, stops at a ContinuedAsNew
// terminal and surfaces it as an error. When false (default), a
// ContinuedAsNew transition is followed: the handle's runID is
// replaced by the successor and polling resumes.
func (r *workflowRunImpl) GetWithOptions(ctx context.Context, valuePtr any, opts WorkflowRunGetOptions) error {
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
			if opts.DisableFollowingRuns {
				return fmt.Errorf("workflow continued as new: %s", info.WorkflowID)
			}
			// v1 DescribeWorkflow does not include the successor run
			// id; clear it and let the next DescribeWorkflow resolve
			// the current head. Server-side uses workflowID as the
			// stable lookup key for the latest run.
			r.runID = ""
			// fallthrough: loop, re-poll
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

// signalWithStartWorkflowRequest is the v1 JSON shape for opcode
// 0x0066. It is the union of startWorkflowRequest plus signalName /
// signalInput — the frontend handler decodes it as
// signalWithStartReq in service/frontend/zap_handler.go.
type signalWithStartWorkflowRequest struct {
	Namespace    string         `json:"namespace"`
	WorkflowID   string         `json:"workflow_id"`
	WorkflowType string         `json:"workflow_type"`
	TaskQueue    string         `json:"task_queue"`
	Input        []any          `json:"input,omitempty"`
	RetryPolicy  *retryPolicy   `json:"retry_policy,omitempty"`
	Timeouts     timeouts       `json:"timeouts,omitempty"`
	Memo         map[string]any `json:"memo,omitempty"`
	CronSchedule string         `json:"cron_schedule,omitempty"`
	Identity     string         `json:"identity,omitempty"`
	SignalName   string         `json:"signal_name"`
	SignalInput  any            `json:"signal_input,omitempty"`
}

// queryWorkflowRequest is the v1 JSON shape for opcode 0x0067.
type queryWorkflowRequest struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	QueryType  string `json:"query_type"`
	QueryArgs  []any  `json:"query_args,omitempty"`
}

// queryWorkflowResponse mirrors the frontend handler's queryResp.
type queryWorkflowResponse struct {
	Result []byte `json:"result,omitempty"`
}

// SignalWithStartWorkflow implements Client. Opcode 0x0066.
func (c *clientImpl) SignalWithStartWorkflow(
	ctx context.Context,
	workflowID, signalName string,
	signalArg any,
	opts StartWorkflowOptions,
	workflow any,
	workflowArgs ...any,
) (WorkflowRun, error) {
	if workflowID == "" {
		return nil, errors.New("hanzo/tasks/client: workflowID is required")
	}
	if signalName == "" {
		return nil, errors.New("hanzo/tasks/client: signalName is required")
	}
	if opts.TaskQueue == "" {
		return nil, errors.New("hanzo/tasks/client: StartWorkflowOptions.TaskQueue is required")
	}
	wfType, err := workflowTypeName(workflow)
	if err != nil {
		return nil, err
	}

	req := signalWithStartWorkflowRequest{
		Namespace:    c.namespace,
		WorkflowID:   workflowID,
		WorkflowType: wfType,
		TaskQueue:    opts.TaskQueue,
		Input:        workflowArgs,
		CronSchedule: opts.CronSchedule,
		Memo:         opts.Memo,
		Identity:     c.identity,
		Timeouts: timeouts{
			WorkflowExecutionMs: opts.WorkflowExecutionTimeout.Milliseconds(),
			WorkflowRunMs:       opts.WorkflowRunTimeout.Milliseconds(),
			WorkflowTaskMs:      opts.WorkflowTaskTimeout.Milliseconds(),
		},
		SignalName:  signalName,
		SignalInput: signalArg,
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
	if err := c.roundTrip(ctx, opSignalWithStartWorkflow, req, &resp); err != nil {
		return nil, err
	}
	return &workflowRunImpl{id: workflowID, runID: resp.RunID, client: c}, nil
}

// GetWorkflow implements Client. No RPC is issued; the returned handle
// polls DescribeWorkflow on Get / GetWithOptions.
func (c *clientImpl) GetWorkflow(ctx context.Context, workflowID, runID string) WorkflowRun {
	// ctx is accepted to match upstream — handle construction itself
	// is local and cannot fail. Subsequent RPCs from the handle honour
	// the ctx passed at that call site.
	_ = ctx
	return &workflowRunImpl{id: workflowID, runID: runID, client: c}
}

// QueryWorkflow implements Client. Opcode 0x0067. Returns a
// converter.EncodedValue the caller decodes via Get / HasValue.
func (c *clientImpl) QueryWorkflow(
	ctx context.Context,
	workflowID, runID, queryType string,
	args ...any,
) (converter.EncodedValue, error) {
	if workflowID == "" {
		return nil, errors.New("hanzo/tasks/client: workflowID is required")
	}
	if queryType == "" {
		return nil, errors.New("hanzo/tasks/client: queryType is required")
	}
	req := queryWorkflowRequest{
		Namespace:  c.namespace,
		WorkflowID: workflowID,
		RunID:      runID,
		QueryType:  queryType,
		QueryArgs:  args,
	}
	var resp queryWorkflowResponse
	if err := c.roundTrip(ctx, opQueryWorkflow, req, &resp); err != nil {
		return nil, err
	}
	return converter.NewJSONValue(resp.Result), nil
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
