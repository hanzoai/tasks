// Copyright © 2026 Hanzo AI. MIT License.

package client

import "context"

// Opcodes owned by the worker <-> matching surface. Opcodes 0x0070-0x007F
// were originally reserved here for worker poll/respond RPCs, but the
// canonical opcode allocation is:
//
//	0x0050-0x005F  pkg/tasks one-shot/schedule (legacy)
//	0x0060-0x006F  client workflow lifecycle (startWorkflow, signal, ...)
//	0x0070-0x007F  client schedule ops (createSchedule, ...)
//	0x0080-0x008F  client namespace ops (registerNamespace, ...)
//	0x0090-0x009F  client health + meta
//	0x00A0-0x00AF  worker poll / respond (see below)
//
// Worker opcodes moved up to 0x00A0 to avoid collision with the client
// schedule ops at 0x0070-0x0073. Append-only across releases; never
// reuse a value.
const (
	OpcodePollWorkflowTask              uint16 = 0x00A0
	OpcodePollActivityTask              uint16 = 0x00A1
	OpcodeRespondWorkflowTaskCompleted  uint16 = 0x00A2
	OpcodeRespondActivityTaskCompleted  uint16 = 0x00A3
	OpcodeRespondActivityTaskFailed     uint16 = 0x00A4
	OpcodeRecordActivityTaskHeartbeat   uint16 = 0x00A5

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
// One method per logical RPC that the worker issues against the Hanzo
// Tasks frontend. Production implementations are backed by luxfi/zap;
// tests inject an in-memory fake satisfying this same interface.
//
// Return values are pre-parsed native Go types — callers never own a
// buffer whose backing bytes belong to the transport layer.
type WorkerTransport interface {
	// Close tears down the underlying node. Safe to call more than
	// once.
	Close() error

	// PollWorkflowTask issues a long-poll for the next workflow
	// task on the given queue. The ctx carries the long-poll
	// deadline; returning a nil task and nil error signals "idle,
	// try again".
	PollWorkflowTask(ctx context.Context, req PollWorkflowTaskRequest) (*WorkflowTask, error)

	// PollActivityTask is the activity counterpart of
	// PollWorkflowTask. Same idle semantics.
	PollActivityTask(ctx context.Context, req PollActivityTaskRequest) (*ActivityTask, error)

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
