// Copyright © 2026 Hanzo AI. MIT License.

package client

import "context"

// Opcodes owned by the worker <-> matching surface. Allocation:
//
//	0x0050-0x005F  pkg/tasks one-shot/schedule (legacy)
//	0x0060-0x006F  client workflow lifecycle (startWorkflow, signal, ...)
//	0x0070-0x007F  client schedule ops (createSchedule, ...)
//	0x0080-0x008F  client namespace ops (registerNamespace, ...)
//	0x0090-0x009F  client health + meta
//	0x00A0-0x00AF  worker subscribe / respond + server-pushed delivery
//
// Server-push wire model (replaces long-poll, 2026-04-28):
//
//   Workers Subscribe once per (namespace, taskQueue, kind); the server
//   pushes Deliver{Workflow,Activity}Task as work arrives, and pushes
//   DeliverActivityResult to the workflow's worker when an activity
//   completes. There is no polling. The authoritative wire shapes live
//   in pkg/tasks/dispatch.go (workflowTaskDeliveryJSON,
//   activityTaskDeliveryJSON, activityResultDeliveryJSON).
//
// Append-only across releases; never reuse a value. 0x00A0 and 0x00A1
// were repurposed from Poll{Workflow,Activity}Task to
// Subscribe{Workflow,Activity}Tasks — the old long-poll opcodes are
// retired and not served by the native frontend.
const (
	// Worker → server (Call). Subscribe returns a sub-id used by
	// Unsubscribe.
	OpcodeSubscribeWorkflowTasks uint16 = 0x00A0
	OpcodeSubscribeActivityTasks uint16 = 0x00A1

	// Worker → server (Call). Respond / heartbeat replies to a
	// previously delivered task.
	OpcodeRespondWorkflowTaskCompleted uint16 = 0x00A2
	OpcodeRespondActivityTaskCompleted uint16 = 0x00A3
	OpcodeRespondActivityTaskFailed    uint16 = 0x00A4
	OpcodeRecordActivityTaskHeartbeat  uint16 = 0x00A5

	// Worker → server (Call). Tear down a subscription.
	OpcodeUnsubscribeTasks uint16 = 0x00A6

	// Server → worker (Send). Worker registers a Handle for each.
	OpcodeDeliverWorkflowTask   uint16 = 0x00B0
	OpcodeDeliverActivityTask   uint16 = 0x00B1
	OpcodeDeliverActivityResult uint16 = 0x00B2

	// In-workflow activity scheduling + child workflows (see
	// schema/tasks.zap § "Worker → server: in-workflow activity
	// scheduling"). 0x006C (WaitActivityResult) was retired —
	// activity results are pushed via OpcodeDeliverActivityResult.
	OpcodeScheduleActivity   uint16 = 0x006B
	OpcodeStartChildWorkflow uint16 = 0x006D

	// OpcodeError is the generic error response. Any handler can
	// return this shape regardless of the request opcode.
	OpcodeError uint16 = 0x00FF
)

// Wire field offsets for the poll/respond request and response objects.
// These stay in one place so the client and the server-side handler
// decode the same layout. Worker-only — the client envelope uses the
// fields declared in client.go (envelopeBody / envelopeStatus / ...).
const (
	// Poll request fields (both workflow + activity).
	FieldNamespace     = 0
	FieldTaskQueueName = 8
	FieldTaskQueueKind = 16
	FieldIdentity      = 24
	FieldWorkerBuildID = 32

	// WorkflowTask response fields.
	FieldTaskToken         = 0
	FieldWorkflowID        = 8
	FieldRunID             = 16
	FieldWorkflowTypeName  = 24
	FieldHistoryBytes      = 32
	FieldNextPageToken     = 40

	// ActivityTask response fields.
	FieldActivityID            = 8
	FieldActivityTypeName      = 16
	FieldInputBytes            = 24
	FieldScheduledTimeMs       = 32
	FieldStartToCloseTimeoutMs = 40
	FieldHeartbeatTimeoutMs    = 48
	FieldActivityWorkflowID    = 56
	FieldActivityRunID         = 64

	// Respond request fields.
	FieldCommandsBytes = 8  // workflow task completed
	FieldResultBytes   = 8  // activity task completed
	FieldFailureBytes  = 8  // activity task failed
	FieldDetailsBytes  = 8  // heartbeat

	// Response fields.
	FieldRespStatus          = 0
	FieldRespError           = 4
	FieldRespCancelRequested = 8
)

// WorkerTransport is the wire abstraction the Worker package depends on.
// Production implementations are backed by luxfi/zap; tests inject an
// in-memory fake satisfying this same interface.
//
// Wire model (server-push, 2026-04-28):
//
//   The worker subscribes once per (namespace, taskQueue, kind) and
//   registers OnWorkflowTask / OnActivityTask / OnActivityResult
//   callbacks. The server pushes work as it arrives. The worker
//   responds via Respond{Workflow,Activity}Task{Completed,Failed} and
//   RecordActivityTaskHeartbeat — same as before. There is no polling.
type WorkerTransport interface {
	// Close tears down the underlying node. Safe to call more than
	// once.
	Close() error

	// SubscribeWorkflowTasks registers this worker to receive workflow
	// tasks pushed by the server (opcode 0x00A0). Returns the server-
	// minted subscription ID, used by Unsubscribe on Stop. Call once
	// per (namespace, taskQueue) the worker handles.
	SubscribeWorkflowTasks(ctx context.Context, req PollWorkflowTaskRequest) (subID string, err error)

	// SubscribeActivityTasks is the activity counterpart of
	// SubscribeWorkflowTasks (opcode 0x00A1).
	SubscribeActivityTasks(ctx context.Context, req PollActivityTaskRequest) (subID string, err error)

	// Unsubscribe tears down a subscription returned by
	// Subscribe{Workflow,Activity}Tasks (opcode 0x00A6). Idempotent.
	Unsubscribe(ctx context.Context, subID string) error

	// OnWorkflowTask installs the callback fired when the server
	// pushes OpcodeDeliverWorkflowTask. Must be called before
	// SubscribeWorkflowTasks so no delivery is dropped between
	// subscribe and handler installation.
	OnWorkflowTask(fn func(*WorkflowTask))

	// OnActivityTask installs the callback fired when the server
	// pushes OpcodeDeliverActivityTask.
	OnActivityTask(fn func(*ActivityTask))

	// OnActivityResult installs the callback fired when the server
	// pushes OpcodeDeliverActivityResult — i.e. an activity scheduled
	// from this workflow has completed (or failed). The workerEnv
	// matches activityID to the pending future and settles it.
	OnActivityResult(fn func(activityID string, result, failure []byte))

	// RespondWorkflowTaskCompleted uploads the worker's commands
	// (encoded command list) for a workflow task.
	RespondWorkflowTaskCompleted(ctx context.Context, req RespondWorkflowTaskCompletedRequest) error

	// RespondActivityTaskCompleted reports a successful activity
	// result.
	RespondActivityTaskCompleted(ctx context.Context, req RespondActivityTaskCompletedRequest) error

	// RespondActivityTaskFailed reports an activity failure.
	RespondActivityTaskFailed(ctx context.Context, req RespondActivityTaskFailedRequest) error

	// RecordActivityTaskHeartbeat signals liveness for a long-running
	// activity. Returns cancelRequested=true if the server wants
	// the activity to stop.
	RecordActivityTaskHeartbeat(ctx context.Context, req RecordActivityTaskHeartbeatRequest) (bool, error)

	// ScheduleActivity asks the frontend to mint an activity task for
	// the given type + input and return a stable id (schema/tasks.zap
	// opcode 0x006B). The workerEnv in pkg/sdk/worker uses this from
	// inside a workflow to dispatch activities; the eventual result
	// arrives via OpcodeDeliverActivityResult.
	ScheduleActivity(ctx context.Context, req ScheduleActivityRequest) (*ScheduleActivityResponse, error)

	// StartChildWorkflow asks the server to start a workflow whose
	// parent is recorded for linkage (opcode 0x006D). Returns the
	// run id; Phase-1 does not wait for child completion — the
	// caller uses DescribeWorkflow or a follow-up
	// WaitChildWorkflowResult when Phase-2 lands.
	StartChildWorkflow(ctx context.Context, req StartChildWorkflowRequest) (*StartChildWorkflowResponse, error)
}

// ScheduleActivityRequest mirrors schema/tasks.zap:ScheduleActivityRequest.
type ScheduleActivityRequest struct {
	Namespace      string
	WorkflowID     string
	RunID          string
	TaskQueue      string
	ActivityType   string
	Input          []byte
	StartToCloseMs int64
	HeartbeatMs    int64
	RetryPolicy    *RetryPolicyJSON
}

// RetryPolicyJSON is the Go-side retry-policy shape passed to schedule
// RPCs. Milliseconds on the wire; zero means "SDK default".
type RetryPolicyJSON struct {
	InitialIntervalMs      int64    `json:"initial_interval_ms,omitempty"`
	BackoffCoefficient     float64  `json:"backoff_coefficient,omitempty"`
	MaximumIntervalMs      int64    `json:"maximum_interval_ms,omitempty"`
	MaximumAttempts        int32    `json:"maximum_attempts,omitempty"`
	NonRetryableErrorTypes []string `json:"non_retryable_error_types,omitempty"`
}

// ScheduleActivityResponse mirrors schema/tasks.zap:ScheduleActivityResponse.
type ScheduleActivityResponse struct {
	ActivityTaskID string
	// TaskToken is the server-minted, HMAC-signed opaque token the
	// worker must present on RespondActivityTaskCompleted/Failed. Empty
	// on error, populated on success.
	TaskToken []byte
}

// StartChildWorkflowRequest mirrors schema/tasks.zap StartChildWorkflowRequest.
type StartChildWorkflowRequest struct {
	Namespace    string
	ParentID     string
	ParentRunID  string
	WorkflowID   string
	WorkflowType string
	TaskQueue    string
	Input        []any
	RetryPolicy  *RetryPolicyJSON
	TimeoutsMs   TimeoutsJSON
}

// TimeoutsJSON is the timeout triple in ms.
type TimeoutsJSON struct {
	WorkflowExecutionMs int64 `json:"workflow_execution_ms,omitempty"`
	WorkflowRunMs       int64 `json:"workflow_run_ms,omitempty"`
	WorkflowTaskMs      int64 `json:"workflow_task_ms,omitempty"`
}

// StartChildWorkflowResponse mirrors schema/tasks.zap.
type StartChildWorkflowResponse struct {
	RunID string
}

// PollWorkflowTaskRequest mirrors schema/tasks.zap:PollWorkflowTaskRequest.
type PollWorkflowTaskRequest struct {
	Namespace      string
	TaskQueueName  string
	TaskQueueKind  int8
	Identity       string
	WorkerBuildID  string
}

// PollActivityTaskRequest mirrors schema/tasks.zap:PollActivityTaskRequest.
type PollActivityTaskRequest struct {
	Namespace     string
	TaskQueueName string
	TaskQueueKind int8
	Identity      string
}

// WorkflowTask mirrors schema/tasks.zap:WorkflowTask.
type WorkflowTask struct {
	TaskToken        []byte
	WorkflowID       string
	RunID            string
	WorkflowTypeName string
	History          []byte
	NextPageToken    []byte
}

// ActivityTask mirrors schema/tasks.zap:ActivityTask. Input is the raw
// encoded Payloads; the worker's activity dispatcher decodes it into
// the registered function's arg types.
type ActivityTask struct {
	TaskToken              []byte
	WorkflowID             string
	RunID                  string
	ActivityID             string
	ActivityTypeName       string
	Input                  []byte
	ScheduledTimeMs        int64
	StartToCloseTimeoutMs  int64
	HeartbeatTimeoutMs     int64
}

// RespondWorkflowTaskCompletedRequest carries a serialised command list.
type RespondWorkflowTaskCompletedRequest struct {
	TaskToken []byte
	Commands  []byte
}

// RespondActivityTaskCompletedRequest carries a serialised result payload.
type RespondActivityTaskCompletedRequest struct {
	TaskToken []byte
	Result    []byte
}

// RespondActivityTaskFailedRequest carries a serialised failure.
type RespondActivityTaskFailedRequest struct {
	TaskToken []byte
	Failure   []byte
}

// RecordActivityTaskHeartbeatRequest carries opaque details bytes.
type RecordActivityTaskHeartbeatRequest struct {
	TaskToken []byte
	Details   []byte
}
