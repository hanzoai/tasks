package frontend

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/luxfi/zap"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	querypb "go.temporal.io/api/query/v1"
	schedulepb "go.temporal.io/api/schedule/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ZAP opcode allocation (see pkg/sdk/client/client.go + worker_transport).
//
// 0x0050-0x005F  pkg/tasks legacy one-shot/schedule
// 0x0060-0x006F  client workflow lifecycle + worker-side in-workflow ops
// 0x0070-0x007F  client schedule ops
// 0x0080-0x008F  namespace ops
// 0x0090-0x009F  health / meta
// 0x00A0-0x00AF  worker poll / respond
//
// Append-only: never rebind a value.
const (
	// 0x0050-0x0051: legacy pkg/tasks (kept for back-compat).
	opcodeTaskSubmit   uint16 = 0x0050
	opcodeTaskSchedule uint16 = 0x0051

	// 0x0060-0x006F: workflow lifecycle (client) + in-workflow (worker).
	opStartWorkflow            uint16 = 0x0060
	opSignalWorkflow           uint16 = 0x0061
	opCancelWorkflow           uint16 = 0x0062
	opTerminateWorkflow        uint16 = 0x0063
	opDescribeWorkflow         uint16 = 0x0064
	opListWorkflows            uint16 = 0x0065
	opSignalWithStartWorkflow  uint16 = 0x0066
	opQueryWorkflow            uint16 = 0x0067
	opScheduleActivity         uint16 = 0x006B
	opWaitActivityResult       uint16 = 0x006C
	opStartChildWorkflow       uint16 = 0x006D

	// 0x0070-0x007F: schedule ops.
	opCreateSchedule   uint16 = 0x0070
	opListSchedules    uint16 = 0x0071
	opDeleteSchedule   uint16 = 0x0072
	opPauseSchedule    uint16 = 0x0073
	opUpdateSchedule   uint16 = 0x0074
	opTriggerSchedule  uint16 = 0x0075
	opDescribeSchedule uint16 = 0x0076

	// 0x0080-0x008F: namespace ops.
	opRegisterNamespace uint16 = 0x0080
	opDescribeNamespace uint16 = 0x0081
	opListNamespaces    uint16 = 0x0082

	// 0x0090-0x009F: health.
	opHealth uint16 = 0x0090

	// 0x00A0-0x00AF: worker poll / respond.
	opPollWorkflowTask              uint16 = 0x00A0
	opPollActivityTask              uint16 = 0x00A1
	opRespondWorkflowTaskCompleted  uint16 = 0x00A2
	opRespondActivityTaskCompleted  uint16 = 0x00A3
	opRespondActivityTaskFailed     uint16 = 0x00A4
	opRecordActivityTaskHeartbeat   uint16 = 0x00A5
)

// ZAP envelope field offsets (client v1 wire: single JSON body + status + error).
// Worker opcodes use a native object layout defined in pkg/sdk/client/transport.go;
// the handlers decode both shapes with zap.Parse on the root.
const (
	zapFieldTaskType = 0 // legacy 0x0050 field: task type string
	zapFieldPayload  = 8 // legacy 0x0050 field: payload bytes
	zapFieldInterval = 16 // legacy 0x0051 field: interval string

	envelopeBody   = 0  // Bytes — JSON body
	envelopeStatus = 8  // Uint32 — status code (0/200 = ok)
	envelopeError  = 12 // Bytes — error detail

	zapRespStatus = 0 // legacy 0x0050/0x0051 response status
	zapRespBody   = 4 // legacy 0x0050/0x0051 response body

	// Worker poll request fields (match pkg/sdk/client/transport.go).
	fNamespace     = 0
	fTaskQueueName = 8
	fTaskQueueKind = 16
	fIdentity      = 24
	fWorkerBuildID = 32

	// Worker workflow-task response fields.
	fTaskToken        = 0
	fWorkflowID       = 8
	fRunID            = 16
	fWorkflowTypeName = 24
	fHistoryBytes     = 32
	fNextPageToken    = 40

	// Worker activity-task response fields.
	fActivityID            = 8
	fActivityTypeName      = 16
	fInputBytes            = 24
	fScheduledTimeMs       = 32
	fStartToCloseTimeoutMs = 40
	fHeartbeatTimeoutMs    = 48
	fActivityWorkflowID    = 56
	fActivityRunID         = 64

	// Respond/heartbeat request fields.
	fCommandsBytes = 8
	fResultBytes   = 8
	fFailureBytes  = 8
	fDetailsBytes  = 8

	fRespCancelRequested = 8
)

// ZAPHandler accepts SDK + worker RPCs over ZAP binary transport.
//
// Two wire shapes coexist:
//
//   - User-facing client RPCs (opStartWorkflow .. opHealth) use the
//     JSON-in-envelope shape declared above. Both request and
//     response carry field 0 (JSON body) + field 8 (status) + field
//     12 (error detail).
//
//   - Worker poll / respond RPCs (0x00A0-0x00A5) use the native
//     object layout declared in pkg/sdk/client/transport.go. Fields
//     are typed (Text / Bytes / Int64) so the worker can decode
//     without a JSON round-trip.
//
// In-process callers reach the same handlers via Dispatch — bypassing
// the ZAP node, the network listener, the encoder, and the loopback
// dial. The dispatch table is the single source of truth: both the
// network handler registration in Start() and the in-process Dispatch
// path read from `handlers`.
type ZAPHandler struct {
	handler  Handler
	node     *zap.Node
	port     int
	logger   *slog.Logger
	broker   *frontendActivityBroker
	handlers map[uint16]zapOpcodeHandler
}

// zapOpcodeHandler is the function shape every opcode handler shares.
// `from` is the originating peer ID (empty for in-process calls).
type zapOpcodeHandler func(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error)

// NewZAPHandler creates a ZAP handler backed by the frontend workflow handler.
func NewZAPHandler(handler Handler, logger *slog.Logger) *ZAPHandler {
	port := 0
	if p := os.Getenv("ZAP_SDK_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	if port == 0 {
		port = 9999
	}

	nodeID, _ := os.Hostname()
	if nodeID == "" {
		nodeID = "tasks-zap"
	}

	z := &ZAPHandler{
		handler: handler,
		port:    port,
		logger:  logger,
		broker:  newActivityBroker(),
		node: zap.NewNode(zap.NodeConfig{
			NodeID:      nodeID + "-sdk",
			ServiceType: "_tasks._tcp",
			Port:        port,
			Logger:      logger,
			NoDiscovery: true,
		}),
	}
	z.buildHandlerTable()
	return z
}

// buildHandlerTable populates z.handlers with the canonical opcode →
// handler mapping. Called once at construction so both Start (network
// listener) and Dispatch (in-process callers) read the same table.
func (z *ZAPHandler) buildHandlerTable() {
	z.handlers = map[uint16]zapOpcodeHandler{
		// Legacy 0x0050/0x0051.
		opcodeTaskSubmit:   z.handleSubmit,
		opcodeTaskSchedule: z.handleSchedule,

		// Workflow lifecycle.
		opStartWorkflow:           z.handleStartWorkflow,
		opSignalWorkflow:          z.handleSignalWorkflow,
		opCancelWorkflow:          z.handleCancelWorkflow,
		opTerminateWorkflow:       z.handleTerminateWorkflow,
		opDescribeWorkflow:        z.handleDescribeWorkflow,
		opListWorkflows:           z.handleListWorkflows,
		opSignalWithStartWorkflow: z.handleSignalWithStartWorkflow,
		opQueryWorkflow:           z.handleQueryWorkflow,

		// In-workflow activity scheduling.
		opScheduleActivity:   z.handleScheduleActivity,
		opWaitActivityResult: z.handleWaitActivityResult,
		opStartChildWorkflow: z.handleStartChildWorkflow,

		// Schedules.
		opCreateSchedule:   z.handleCreateSchedule,
		opListSchedules:    z.handleListSchedules,
		opDeleteSchedule:   z.handleDeleteSchedule,
		opPauseSchedule:    z.handlePauseSchedule,
		opUpdateSchedule:   z.handleUpdateSchedule,
		opTriggerSchedule:  z.handleTriggerSchedule,
		opDescribeSchedule: z.handleDescribeSchedule,

		// Namespaces.
		opRegisterNamespace: z.handleRegisterNamespace,
		opDescribeNamespace: z.handleDescribeNamespace,
		opListNamespaces:    z.handleListNamespaces,

		// Health.
		opHealth: z.handleHealth,

		// Worker poll / respond.
		opPollWorkflowTask:             z.handlePollWorkflowTask,
		opPollActivityTask:             z.handlePollActivityTask,
		opRespondWorkflowTaskCompleted: z.handleRespondWorkflowTaskCompleted,
		opRespondActivityTaskCompleted: z.handleRespondActivityTaskCompleted,
		opRespondActivityTaskFailed:    z.handleRespondActivityTaskFailed,
		opRecordActivityTaskHeartbeat:  z.handleRecordActivityTaskHeartbeat,
	}
}

// Start registers handlers and starts the ZAP listener.
func (z *ZAPHandler) Start() error {
	if z.handlers == nil {
		z.buildHandlerTable()
	}
	for opcode, fn := range z.handlers {
		z.node.Handle(opcode, zap.Handler(fn))
	}

	if err := z.node.Start(); err != nil {
		return fmt.Errorf("zap sdk handler start: %w", err)
	}

	z.logger.Info("ZAP SDK handler listening", "port", z.port)
	return nil
}

// Dispatch invokes the registered handler for opcode synchronously,
// bypassing the ZAP node, encoder, and listener. The caller passes the
// already-parsed *zap.Message; the response is the *zap.Message the
// handler produced. Used by pkg/sdk/inproc to give in-process workers
// and admin handlers a zero-copy path that does not depend on the
// loopback ZAP listener being bound.
//
// Returns ErrUnknownOpcode if no handler is registered for opcode.
func (z *ZAPHandler) Dispatch(ctx context.Context, opcode uint16, msg *zap.Message) (*zap.Message, error) {
	if z.handlers == nil {
		z.buildHandlerTable()
	}
	fn, ok := z.handlers[opcode]
	if !ok {
		return nil, fmt.Errorf("hanzo/tasks/zap: %w 0x%04x", ErrUnknownOpcode, opcode)
	}
	return fn(ctx, "", msg)
}

// ErrUnknownOpcode is returned by Dispatch when no handler is registered.
var ErrUnknownOpcode = fmt.Errorf("unknown opcode")

// Stop shuts down the ZAP listener.
func (z *ZAPHandler) Stop() {
	if z.node != nil {
		z.node.Stop()
	}
}

// ---- Legacy handlers (0x0050 / 0x0051) ----------------------------------

// handleSubmit processes a one-shot task submission (opcode 0x0050).
func (z *ZAPHandler) handleSubmit(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	taskType := root.Text(zapFieldTaskType)
	payloadBytes := root.Bytes(zapFieldPayload)

	if taskType == "" {
		return z.legacyErrorResponse(400, "task type required")
	}

	z.logger.Debug("zap: task submit", "type", taskType, "from", from, "payloadLen", len(payloadBytes))

	req := &workflowservice.StartWorkflowExecutionRequest{
		Namespace:    "default",
		WorkflowId:   fmt.Sprintf("%s-%d", taskType, time.Now().UnixNano()),
		WorkflowType: &commonpb.WorkflowType{Name: taskType},
		TaskQueue:    &taskqueuepb.TaskQueue{Name: "default", Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
		Input:        &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: payloadBytes}}},
		RequestId:    fmt.Sprintf("zap-%d", time.Now().UnixNano()),
	}

	resp, err := z.handler.StartWorkflowExecution(ctx, req)
	if err != nil {
		z.logger.Warn("zap: workflow start failed", "type", taskType, "error", err)
		return z.legacyErrorResponse(500, err.Error())
	}

	body, _ := json.Marshal(map[string]string{
		"run_id": resp.GetRunId(),
	})
	return z.legacyOKResponse(body)
}

// handleSchedule processes a recurring schedule creation (opcode 0x0051).
func (z *ZAPHandler) handleSchedule(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	name := root.Text(zapFieldTaskType)
	intervalStr := root.Text(zapFieldInterval)

	if name == "" {
		return z.legacyErrorResponse(400, "schedule name required")
	}

	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return z.legacyErrorResponse(400, fmt.Sprintf("invalid interval %q: %v", intervalStr, err))
	}

	z.logger.Debug("zap: schedule create", "name", name, "interval", interval, "from", from)

	req := &workflowservice.CreateScheduleRequest{
		Namespace:  "default",
		ScheduleId: name,
		Schedule: &schedulepb.Schedule{
			Spec: &schedulepb.ScheduleSpec{
				Interval: []*schedulepb.IntervalSpec{
					{Interval: durationpb.New(interval)},
				},
			},
			Action: &schedulepb.ScheduleAction{
				Action: &schedulepb.ScheduleAction_StartWorkflow{
					StartWorkflow: &workflowpb.NewWorkflowExecutionInfo{
						WorkflowId:   name,
						WorkflowType: &commonpb.WorkflowType{Name: name},
						TaskQueue:    &taskqueuepb.TaskQueue{Name: "default", Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
					},
				},
			},
		},
		RequestId: fmt.Sprintf("zap-sched-%d", time.Now().UnixNano()),
	}

	_, err = z.handler.CreateSchedule(ctx, req)
	if err != nil {
		z.logger.Warn("zap: schedule create failed", "name", name, "error", err)
		return z.legacyErrorResponse(500, err.Error())
	}

	body, _ := json.Marshal(map[string]string{"schedule_id": name})
	return z.legacyOKResponse(body)
}

// ---- Envelope helpers ---------------------------------------------------

// envelopeBodyBytes pulls the JSON body out of the v1 envelope.
func envelopeBodyBytes(msg *zap.Message) []byte {
	return msg.Root().Bytes(envelopeBody)
}

// okEnvelope wraps a JSON body in the success envelope (status=200).
func (z *ZAPHandler) okEnvelope(body []byte) (*zap.Message, error) {
	return z.buildEnvelope(200, "", body)
}

// errEnvelope wraps an error in the failure envelope.
func (z *ZAPHandler) errEnvelope(status uint32, err error) (*zap.Message, error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	return z.buildEnvelope(status, msg, nil)
}

// errEnvelopeMsg wraps a string error in the failure envelope.
func (z *ZAPHandler) errEnvelopeMsg(status uint32, msg string) (*zap.Message, error) {
	return z.buildEnvelope(status, msg, nil)
}

// buildEnvelope encodes (status, errMsg, body) in the v1 wire shape.
func (z *ZAPHandler) buildEnvelope(status uint32, errMsg string, body []byte) (*zap.Message, error) {
	b := zap.NewBuilder(len(body) + len(errMsg) + 64)
	obj := b.StartObject(24)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, status)
	if errMsg != "" {
		obj.SetBytes(envelopeError, []byte(errMsg))
	}
	obj.FinishAsRoot()
	data := b.Finish()
	return zap.Parse(data)
}

// roundTripEnvelope decodes the JSON body into req, runs fn, and
// marshals its return into the success envelope.
func (z *ZAPHandler) roundTripEnvelope(msg *zap.Message, req any, fn func() (any, error)) (*zap.Message, error) {
	if req != nil {
		body := envelopeBodyBytes(msg)
		if len(body) > 0 {
			if err := json.Unmarshal(body, req); err != nil {
				return z.errEnvelope(400, fmt.Errorf("decode request: %w", err))
			}
		}
	}
	resp, err := fn()
	if err != nil {
		return z.errEnvelope(500, err)
	}
	if resp == nil {
		return z.okEnvelope(nil)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return z.errEnvelope(500, fmt.Errorf("encode response: %w", err))
	}
	return z.okEnvelope(out)
}

// legacyOKResponse builds the legacy 0x0050/0x0051 success envelope
// (status at field 0, body at field 4).
func (z *ZAPHandler) legacyOKResponse(body []byte) (*zap.Message, error) {
	return z.legacyBuildResponse(200, body)
}

func (z *ZAPHandler) legacyErrorResponse(status uint32, message string) (*zap.Message, error) {
	body, _ := json.Marshal(map[string]string{"error": message})
	return z.legacyBuildResponse(status, body)
}

func (z *ZAPHandler) legacyBuildResponse(status uint32, body []byte) (*zap.Message, error) {
	b := zap.NewBuilder(len(body) + 64)
	obj := b.StartObject(16)
	obj.SetUint32(zapRespStatus, status)
	obj.SetBytes(zapRespBody, body)
	obj.FinishAsRoot()
	data := b.Finish()
	return zap.Parse(data)
}

// ---- JSON request/response shapes ---------------------------------------

type startWorkflowReq struct {
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

type startWorkflowResp struct {
	RunID string `json:"run_id"`
}

type signalReq struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	SignalName string `json:"signal_name"`
	Input      any    `json:"input,omitempty"`
}

type cancelReq struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type terminateReq struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

type describeReq struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
}

type describeResp struct {
	Info wfInfo `json:"info"`
}

type wfInfo struct {
	WorkflowID    string         `json:"workflow_id"`
	RunID         string         `json:"run_id"`
	WorkflowType  string         `json:"workflow_type"`
	StartTime     time.Time      `json:"start_time,omitempty"`
	CloseTime     time.Time      `json:"close_time,omitempty"`
	Status        int8           `json:"status"`
	HistoryLength int64          `json:"history_length"`
	TaskQueue     string         `json:"task_queue,omitempty"`
	Memo          map[string]any `json:"memo,omitempty"`
}

type listReq struct {
	Namespace     string `json:"namespace"`
	Query         string `json:"query,omitempty"`
	PageSize      int32  `json:"page_size,omitempty"`
	NextPageToken []byte `json:"next_page_token,omitempty"`
}

type listResp struct {
	Executions    []wfInfo `json:"executions"`
	NextPageToken []byte   `json:"next_page_token,omitempty"`
}

type signalWithStartReq struct {
	startWorkflowReq
	SignalName  string `json:"signal_name"`
	SignalInput any    `json:"signal_input,omitempty"`
}

type queryReq struct {
	Namespace  string `json:"namespace"`
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id,omitempty"`
	QueryType  string `json:"query_type"`
	QueryArgs  []any  `json:"query_args,omitempty"`
}

type queryResp struct {
	Result []byte `json:"result,omitempty"`
}

type registerNSReq struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	RetentionMs int64  `json:"retention_ms,omitempty"`
}

type describeNSReq struct {
	Name string `json:"name"`
}

type describeNSResp struct {
	Namespace nsShape `json:"namespace"`
}

type nsShape struct {
	Info   nsInfo   `json:"info"`
	Config nsConfig `json:"config"`
}

type nsInfo struct {
	Name        string `json:"name"`
	State       int8   `json:"state"`
	Description string `json:"description,omitempty"`
	OwnerEmail  string `json:"owner_email,omitempty"`
	ID          string `json:"id,omitempty"`
}

type nsConfig struct {
	RetentionMs int64 `json:"retention_ms,omitempty"`
}

type listNSReq struct {
	PageSize      int32  `json:"page_size,omitempty"`
	NextPageToken []byte `json:"next_page_token,omitempty"`
}

type listNSResp struct {
	Namespaces    []nsShape `json:"namespaces"`
	NextPageToken []byte    `json:"next_page_token,omitempty"`
}

type createScheduleReq struct {
	Namespace  string       `json:"namespace"`
	ScheduleID string       `json:"schedule_id"`
	Schedule   scheduleBody `json:"schedule"`
}

type scheduleBody struct {
	ID     string         `json:"id"`
	Spec   scheduleSpec   `json:"spec"`
	Action scheduleAction `json:"action"`
	Paused bool           `json:"paused,omitempty"`
}

type scheduleSpec struct {
	Cron        []string `json:"cron,omitempty"`
	IntervalMs  int64    `json:"interval_ms,omitempty"`
	StartTimeMs int64    `json:"start_time_ms,omitempty"`
	EndTimeMs   int64    `json:"end_time_ms,omitempty"`
	JitterMs    int64    `json:"jitter_ms,omitempty"`
	Timezone    string   `json:"timezone,omitempty"`
}

type scheduleAction struct {
	WorkflowID   string `json:"workflow_id,omitempty"`
	WorkflowType string `json:"workflow_type"`
	TaskQueue    string `json:"task_queue"`
	Input        []any  `json:"input,omitempty"`
}

type listSchedulesReq struct {
	Namespace     string `json:"namespace"`
	PageSize      int32  `json:"page_size,omitempty"`
	NextPageToken []byte `json:"next_page_token,omitempty"`
}

type listSchedulesResp struct {
	Schedules     []scheduleBody `json:"schedules"`
	NextPageToken []byte         `json:"next_page_token,omitempty"`
}

type deleteScheduleReq struct {
	Namespace  string `json:"namespace"`
	ScheduleID string `json:"schedule_id"`
}

type pauseScheduleReq struct {
	Namespace  string `json:"namespace"`
	ScheduleID string `json:"schedule_id"`
	Paused     bool   `json:"paused"`
}

type updateScheduleReq struct {
	Namespace  string       `json:"namespace"`
	ScheduleID string       `json:"schedule_id"`
	Schedule   scheduleBody `json:"schedule"`
}

type triggerScheduleReq struct {
	Namespace     string `json:"namespace"`
	ScheduleID    string `json:"schedule_id"`
	OverlapPolicy string `json:"overlap_policy,omitempty"`
}

type describeScheduleReq struct {
	Namespace  string `json:"namespace"`
	ScheduleID string `json:"schedule_id"`
}

type describeScheduleResp struct {
	Schedule scheduleBody     `json:"schedule"`
	Info     scheduleInfoResp `json:"info"`
}

type scheduleInfoResp struct {
	ActionCount        int64                  `json:"action_count"`
	MissedCatchupCount int64                  `json:"missed_catchup_count"`
	OverlapSkipped     int64                  `json:"overlap_skipped"`
	BufferDropped      int64                  `json:"buffer_dropped"`
	BufferSize         int64                  `json:"buffer_size"`
	RunningWorkflows   []scheduleRunningEntry `json:"running_workflows,omitempty"`
}

type scheduleRunningEntry struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}

type scheduleActivityReq struct {
	Namespace      string       `json:"namespace"`
	WorkflowID     string       `json:"workflow_id"`
	RunID          string       `json:"run_id,omitempty"`
	TaskQueue      string       `json:"task_queue"`
	ActivityType   string       `json:"activity_type"`
	Input          []byte       `json:"input,omitempty"`
	StartToCloseMs int64        `json:"start_to_close_ms,omitempty"`
	HeartbeatMs    int64        `json:"heartbeat_ms,omitempty"`
	RetryPolicy    *retryPolicy `json:"retry_policy,omitempty"`
}

type scheduleActivityResp struct {
	ActivityTaskID string `json:"activity_task_id"`
	// TaskToken is the opaque HMAC-signed token the worker must present
	// on RespondActivityTaskCompleted/Failed. It binds the respond call
	// to this specific workflow's scope; any other caller that learns
	// only ActivityTaskID cannot settle the activity.
	TaskToken []byte `json:"task_token,omitempty"`
}

type waitActivityResultReq struct {
	ActivityTaskID string `json:"activity_task_id"`
	WaitMs         int64  `json:"wait_ms,omitempty"`
}

type waitActivityResultResp struct {
	Ready   bool   `json:"ready"`
	Result  []byte `json:"result,omitempty"`
	Failure []byte `json:"failure,omitempty"`
}

type startChildWorkflowReq struct {
	Namespace    string       `json:"namespace"`
	ParentID     string       `json:"parent_id"`
	ParentRunID  string       `json:"parent_run_id,omitempty"`
	WorkflowID   string       `json:"workflow_id"`
	WorkflowType string       `json:"workflow_type"`
	TaskQueue    string       `json:"task_queue"`
	Input        []any        `json:"input,omitempty"`
	RetryPolicy  *retryPolicy `json:"retry_policy,omitempty"`
	Timeouts     timeouts     `json:"timeouts,omitempty"`
}

type startChildWorkflowResp struct {
	RunID string `json:"run_id"`
}

// ---- Workflow lifecycle handlers ----------------------------------------

func (z *ZAPHandler) handleStartWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req startWorkflowReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, fmt.Errorf("decode request: %w", err))
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if req.WorkflowID == "" {
		req.WorkflowID = fmt.Sprintf("%s-%d", req.WorkflowType, time.Now().UnixNano())
	}

	inputPayloads, err := encodeArgsPayloads(req.Input)
	if err != nil {
		return z.errEnvelope(400, err)
	}

	preq := &workflowservice.StartWorkflowExecutionRequest{
		Namespace:                req.Namespace,
		WorkflowId:               req.WorkflowID,
		WorkflowType:             &commonpb.WorkflowType{Name: req.WorkflowType},
		TaskQueue:                &taskqueuepb.TaskQueue{Name: req.TaskQueue, Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
		Input:                    inputPayloads,
		WorkflowExecutionTimeout: msDuration(req.Timeouts.WorkflowExecutionMs),
		WorkflowRunTimeout:       msDuration(req.Timeouts.WorkflowRunMs),
		WorkflowTaskTimeout:      msDuration(req.Timeouts.WorkflowTaskMs),
		Identity:                 req.Identity,
		RequestId:                fmt.Sprintf("zap-%d", time.Now().UnixNano()),
		CronSchedule:             req.CronSchedule,
		RetryPolicy:              retryPolicyToProto(req.RetryPolicy),
	}

	resp, err := z.handler.StartWorkflowExecution(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	out, _ := json.Marshal(startWorkflowResp{RunID: resp.GetRunId()})
	return z.okEnvelope(out)
}

func (z *ZAPHandler) handleSignalWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req signalReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	payloads, err := encodeArgsPayloads([]any{req.Input})
	if err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.SignalWorkflowExecutionRequest{
		Namespace:         defaultIfEmpty(req.Namespace),
		WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: req.WorkflowID, RunId: req.RunID},
		SignalName:        req.SignalName,
		Input:             payloads,
		RequestId:         fmt.Sprintf("zap-sig-%d", time.Now().UnixNano()),
	}
	if _, err := z.handler.SignalWorkflowExecution(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

func (z *ZAPHandler) handleCancelWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req cancelReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.RequestCancelWorkflowExecutionRequest{
		Namespace:         defaultIfEmpty(req.Namespace),
		WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: req.WorkflowID, RunId: req.RunID},
		Reason:            req.Reason,
		RequestId:         fmt.Sprintf("zap-cancel-%d", time.Now().UnixNano()),
	}
	if _, err := z.handler.RequestCancelWorkflowExecution(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

func (z *ZAPHandler) handleTerminateWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req terminateReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.TerminateWorkflowExecutionRequest{
		Namespace:         defaultIfEmpty(req.Namespace),
		WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: req.WorkflowID, RunId: req.RunID},
		Reason:            req.Reason,
	}
	if _, err := z.handler.TerminateWorkflowExecution(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

func (z *ZAPHandler) handleDescribeWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req describeReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.DescribeWorkflowExecutionRequest{
		Namespace: defaultIfEmpty(req.Namespace),
		Execution: &commonpb.WorkflowExecution{WorkflowId: req.WorkflowID, RunId: req.RunID},
	}
	resp, err := z.handler.DescribeWorkflowExecution(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	info := resp.GetWorkflowExecutionInfo()
	out, _ := json.Marshal(describeResp{Info: wfInfoFromProto(info)})
	return z.okEnvelope(out)
}

func (z *ZAPHandler) handleListWorkflows(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req listReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	preq := &workflowservice.ListWorkflowExecutionsRequest{
		Namespace:     defaultIfEmpty(req.Namespace),
		Query:         req.Query,
		PageSize:      req.PageSize,
		NextPageToken: req.NextPageToken,
	}
	resp, err := z.handler.ListWorkflowExecutions(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	execs := make([]wfInfo, 0, len(resp.GetExecutions()))
	for _, e := range resp.GetExecutions() {
		execs = append(execs, wfInfoFromProto(e))
	}
	out, _ := json.Marshal(listResp{Executions: execs, NextPageToken: resp.GetNextPageToken()})
	return z.okEnvelope(out)
}

func (z *ZAPHandler) handleSignalWithStartWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req signalWithStartReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if req.WorkflowID == "" {
		req.WorkflowID = fmt.Sprintf("%s-%d", req.WorkflowType, time.Now().UnixNano())
	}
	inputPayloads, err := encodeArgsPayloads(req.Input)
	if err != nil {
		return z.errEnvelope(400, err)
	}
	signalPayloads, err := encodeArgsPayloads([]any{req.SignalInput})
	if err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.SignalWithStartWorkflowExecutionRequest{
		Namespace:                req.Namespace,
		WorkflowId:               req.WorkflowID,
		WorkflowType:             &commonpb.WorkflowType{Name: req.WorkflowType},
		TaskQueue:                &taskqueuepb.TaskQueue{Name: req.TaskQueue, Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
		Input:                    inputPayloads,
		SignalName:               req.SignalName,
		SignalInput:              signalPayloads,
		WorkflowExecutionTimeout: msDuration(req.Timeouts.WorkflowExecutionMs),
		WorkflowRunTimeout:       msDuration(req.Timeouts.WorkflowRunMs),
		WorkflowTaskTimeout:      msDuration(req.Timeouts.WorkflowTaskMs),
		Identity:                 req.Identity,
		RequestId:                fmt.Sprintf("zap-sws-%d", time.Now().UnixNano()),
		CronSchedule:             req.CronSchedule,
		RetryPolicy:              retryPolicyToProto(req.RetryPolicy),
	}
	resp, err := z.handler.SignalWithStartWorkflowExecution(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	out, _ := json.Marshal(startWorkflowResp{RunID: resp.GetRunId()})
	return z.okEnvelope(out)
}

func (z *ZAPHandler) handleQueryWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req queryReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	argsPayloads, err := encodeArgsPayloads(req.QueryArgs)
	if err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.QueryWorkflowRequest{
		Namespace: defaultIfEmpty(req.Namespace),
		Execution: &commonpb.WorkflowExecution{WorkflowId: req.WorkflowID, RunId: req.RunID},
		Query: &querypb.WorkflowQuery{
			QueryType: req.QueryType,
			QueryArgs: argsPayloads,
		},
	}
	resp, err := z.handler.QueryWorkflow(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	var result []byte
	if rp := resp.GetQueryResult(); rp != nil && len(rp.GetPayloads()) > 0 {
		result = rp.GetPayloads()[0].GetData()
	}
	out, _ := json.Marshal(queryResp{Result: result})
	return z.okEnvelope(out)
}

// ---- In-workflow activity / child ops (Phase-1 stubs) -------------------

func (z *ZAPHandler) handleScheduleActivity(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	// Mint a stable activityTaskId and register it with the broker.
	// The worker executes the activity out-of-band and delivers the
	// result by calling RespondActivityTaskCompleted/Failed with a
	// TaskToken that has the "zapact:<id>" prefix (see
	// frontendActivityBroker).
	var req scheduleActivityReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	id := fmt.Sprintf("%s/%s/%d", req.WorkflowID, req.ActivityType, time.Now().UnixNano())
	ttl := time.Duration(req.StartToCloseMs) * time.Millisecond
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	token := z.broker.Register(id, activityScope{
		Namespace:  req.Namespace,
		TaskQueue:  req.TaskQueue,
		WorkflowID: req.WorkflowID,
		RunID:      req.RunID,
	}, ttl)
	resp := scheduleActivityResp{ActivityTaskID: id, TaskToken: token}
	out, _ := json.Marshal(resp)
	return z.okEnvelope(out)
}

func (z *ZAPHandler) handleWaitActivityResult(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	// Long-poll the broker for the activityTaskId's outcome. WaitMs=0
	// means single-poll (non-blocking). WaitCtx honors the incoming
	// ZAP ctx deadline / cancellation so a caller that times out or
	// disconnects unblocks the goroutine promptly instead of waiting
	// out WaitMs.
	var req waitActivityResultReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	wait := time.Duration(req.WaitMs) * time.Millisecond
	ready, result, failure, werr := z.broker.WaitCtx(ctx, req.ActivityTaskID, wait)
	if werr != nil {
		// Context cancellation: surface as a 499-style client error so
		// the caller sees why the wait aborted. Unlike a 5xx, the
		// entry is not dropped — a subsequent Wait with a live ctx
		// can still settle it.
		return z.errEnvelope(499, werr)
	}
	out, _ := json.Marshal(waitActivityResultResp{
		Ready:   ready,
		Result:  result,
		Failure: failure,
	})
	return z.okEnvelope(out)
}

func (z *ZAPHandler) handleStartChildWorkflow(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	// Phase-1: a child-workflow start is just a StartWorkflow with the
	// parent's WorkflowID recorded in memo. Follow-up will add real
	// parent/child linkage via history-service linkage events.
	var req startChildWorkflowReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	inputPayloads, err := encodeArgsPayloads(req.Input)
	if err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.StartWorkflowExecutionRequest{
		Namespace:                defaultIfEmpty(req.Namespace),
		WorkflowId:               req.WorkflowID,
		WorkflowType:             &commonpb.WorkflowType{Name: req.WorkflowType},
		TaskQueue:                &taskqueuepb.TaskQueue{Name: req.TaskQueue, Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
		Input:                    inputPayloads,
		WorkflowExecutionTimeout: msDuration(req.Timeouts.WorkflowExecutionMs),
		WorkflowRunTimeout:       msDuration(req.Timeouts.WorkflowRunMs),
		WorkflowTaskTimeout:      msDuration(req.Timeouts.WorkflowTaskMs),
		RequestId:                fmt.Sprintf("zap-child-%d", time.Now().UnixNano()),
		RetryPolicy:              retryPolicyToProto(req.RetryPolicy),
	}
	resp, err := z.handler.StartWorkflowExecution(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	out, _ := json.Marshal(startChildWorkflowResp{RunID: resp.GetRunId()})
	return z.okEnvelope(out)
}

// ---- Schedule handlers --------------------------------------------------

func (z *ZAPHandler) handleCreateSchedule(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req createScheduleReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	spec := &schedulepb.ScheduleSpec{}
	for _, c := range req.Schedule.Spec.Cron {
		spec.CronString = append(spec.CronString, c)
	}
	if req.Schedule.Spec.IntervalMs > 0 {
		spec.Interval = []*schedulepb.IntervalSpec{{
			Interval: durationpb.New(msToDuration(req.Schedule.Spec.IntervalMs)),
		}}
	}
	if req.Schedule.Spec.StartTimeMs > 0 {
		spec.StartTime = timestamppb.New(time.UnixMilli(req.Schedule.Spec.StartTimeMs))
	}
	if req.Schedule.Spec.EndTimeMs > 0 {
		spec.EndTime = timestamppb.New(time.UnixMilli(req.Schedule.Spec.EndTimeMs))
	}
	if req.Schedule.Spec.JitterMs > 0 {
		spec.Jitter = durationpb.New(msToDuration(req.Schedule.Spec.JitterMs))
	}
	spec.TimezoneName = req.Schedule.Spec.Timezone

	actInput, err := encodeArgsPayloads(req.Schedule.Action.Input)
	if err != nil {
		return z.errEnvelope(400, err)
	}

	preq := &workflowservice.CreateScheduleRequest{
		Namespace:  req.Namespace,
		ScheduleId: req.ScheduleID,
		Schedule: &schedulepb.Schedule{
			Spec: spec,
			Action: &schedulepb.ScheduleAction{
				Action: &schedulepb.ScheduleAction_StartWorkflow{
					StartWorkflow: &workflowpb.NewWorkflowExecutionInfo{
						WorkflowId:   req.Schedule.Action.WorkflowID,
						WorkflowType: &commonpb.WorkflowType{Name: req.Schedule.Action.WorkflowType},
						TaskQueue:    &taskqueuepb.TaskQueue{Name: req.Schedule.Action.TaskQueue, Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
						Input:        actInput,
					},
				},
			},
			State: &schedulepb.ScheduleState{Paused: req.Schedule.Paused},
		},
		RequestId: fmt.Sprintf("zap-cs-%d", time.Now().UnixNano()),
	}
	if _, err := z.handler.CreateSchedule(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

func (z *ZAPHandler) handleListSchedules(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req listSchedulesReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	preq := &workflowservice.ListSchedulesRequest{
		Namespace:       defaultIfEmpty(req.Namespace),
		MaximumPageSize: req.PageSize,
		NextPageToken:   req.NextPageToken,
	}
	resp, err := z.handler.ListSchedules(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	out := listSchedulesResp{NextPageToken: resp.GetNextPageToken()}
	for _, s := range resp.GetSchedules() {
		out.Schedules = append(out.Schedules, scheduleBody{
			ID: s.GetScheduleId(),
		})
	}
	outBytes, _ := json.Marshal(out)
	return z.okEnvelope(outBytes)
}

func (z *ZAPHandler) handleDeleteSchedule(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req deleteScheduleReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.DeleteScheduleRequest{
		Namespace:  defaultIfEmpty(req.Namespace),
		ScheduleId: req.ScheduleID,
	}
	if _, err := z.handler.DeleteSchedule(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

func (z *ZAPHandler) handlePauseSchedule(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req pauseScheduleReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	patch := &schedulepb.SchedulePatch{}
	if req.Paused {
		patch.Pause = "paused via zap SDK"
	} else {
		patch.Unpause = "unpaused via zap SDK"
	}
	preq := &workflowservice.PatchScheduleRequest{
		Namespace:  defaultIfEmpty(req.Namespace),
		ScheduleId: req.ScheduleID,
		Patch:      patch,
		RequestId:  fmt.Sprintf("zap-ps-%d", time.Now().UnixNano()),
	}
	if _, err := z.handler.PatchSchedule(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

// handleUpdateSchedule replaces a schedule's definition. Mirrors
// CreateSchedule's body decoding so spec / action transforms are
// shared.
func (z *ZAPHandler) handleUpdateSchedule(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req updateScheduleReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.ScheduleID == "" {
		return z.errEnvelope(400, fmt.Errorf("schedule_id required"))
	}
	spec := &schedulepb.ScheduleSpec{}
	for _, c := range req.Schedule.Spec.Cron {
		spec.CronString = append(spec.CronString, c)
	}
	if req.Schedule.Spec.IntervalMs > 0 {
		spec.Interval = []*schedulepb.IntervalSpec{{
			Interval: durationpb.New(msToDuration(req.Schedule.Spec.IntervalMs)),
		}}
	}
	if req.Schedule.Spec.StartTimeMs > 0 {
		spec.StartTime = timestamppb.New(time.UnixMilli(req.Schedule.Spec.StartTimeMs))
	}
	if req.Schedule.Spec.EndTimeMs > 0 {
		spec.EndTime = timestamppb.New(time.UnixMilli(req.Schedule.Spec.EndTimeMs))
	}
	if req.Schedule.Spec.JitterMs > 0 {
		spec.Jitter = durationpb.New(msToDuration(req.Schedule.Spec.JitterMs))
	}
	spec.TimezoneName = req.Schedule.Spec.Timezone

	actInput, err := encodeArgsPayloads(req.Schedule.Action.Input)
	if err != nil {
		return z.errEnvelope(400, err)
	}

	preq := &workflowservice.UpdateScheduleRequest{
		Namespace:  defaultIfEmpty(req.Namespace),
		ScheduleId: req.ScheduleID,
		Schedule: &schedulepb.Schedule{
			Spec: spec,
			Action: &schedulepb.ScheduleAction{
				Action: &schedulepb.ScheduleAction_StartWorkflow{
					StartWorkflow: &workflowpb.NewWorkflowExecutionInfo{
						WorkflowId:   req.Schedule.Action.WorkflowID,
						WorkflowType: &commonpb.WorkflowType{Name: req.Schedule.Action.WorkflowType},
						TaskQueue:    &taskqueuepb.TaskQueue{Name: req.Schedule.Action.TaskQueue, Kind: enumspb.TASK_QUEUE_KIND_NORMAL},
						Input:        actInput,
					},
				},
			},
			State: &schedulepb.ScheduleState{Paused: req.Schedule.Paused},
		},
		RequestId: fmt.Sprintf("zap-us-%d", time.Now().UnixNano()),
	}
	if _, err := z.handler.UpdateSchedule(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

// handleTriggerSchedule fires a schedule once immediately, ignoring
// its spec. The optional overlap_policy maps to schedulepb's enum.
func (z *ZAPHandler) handleTriggerSchedule(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req triggerScheduleReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.ScheduleID == "" {
		return z.errEnvelope(400, fmt.Errorf("schedule_id required"))
	}
	patch := &schedulepb.SchedulePatch{
		TriggerImmediately: &schedulepb.TriggerImmediatelyRequest{
			OverlapPolicy: triggerOverlapEnum(req.OverlapPolicy),
		},
	}
	preq := &workflowservice.PatchScheduleRequest{
		Namespace:  defaultIfEmpty(req.Namespace),
		ScheduleId: req.ScheduleID,
		Patch:      patch,
		RequestId:  fmt.Sprintf("zap-ts-%d", time.Now().UnixNano()),
	}
	if _, err := z.handler.PatchSchedule(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

// handleDescribeSchedule returns the current schedule + runtime info.
func (z *ZAPHandler) handleDescribeSchedule(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req describeScheduleReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.ScheduleID == "" {
		return z.errEnvelope(400, fmt.Errorf("schedule_id required"))
	}
	preq := &workflowservice.DescribeScheduleRequest{
		Namespace:  defaultIfEmpty(req.Namespace),
		ScheduleId: req.ScheduleID,
	}
	resp, err := z.handler.DescribeSchedule(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	out := describeScheduleResp{
		Schedule: scheduleBody{ID: req.ScheduleID},
	}
	if s := resp.GetSchedule(); s != nil {
		// Reflect schedule state into the wire body.
		if state := s.GetState(); state != nil {
			out.Schedule.Paused = state.GetPaused()
		}
		if act := s.GetAction(); act != nil {
			if sw := act.GetStartWorkflow(); sw != nil {
				out.Schedule.Action.WorkflowID = sw.GetWorkflowId()
				out.Schedule.Action.WorkflowType = sw.GetWorkflowType().GetName()
				out.Schedule.Action.TaskQueue = sw.GetTaskQueue().GetName()
			}
		}
		if spec := s.GetSpec(); spec != nil {
			out.Schedule.Spec.Cron = append([]string(nil), spec.GetCronString()...)
			if iv := spec.GetInterval(); len(iv) > 0 {
				out.Schedule.Spec.IntervalMs = iv[0].GetInterval().AsDuration().Milliseconds()
			}
			if t := spec.GetStartTime(); t != nil {
				out.Schedule.Spec.StartTimeMs = t.AsTime().UnixMilli()
			}
			if t := spec.GetEndTime(); t != nil {
				out.Schedule.Spec.EndTimeMs = t.AsTime().UnixMilli()
			}
			if j := spec.GetJitter(); j != nil {
				out.Schedule.Spec.JitterMs = j.AsDuration().Milliseconds()
			}
			out.Schedule.Spec.Timezone = spec.GetTimezoneName()
		}
	}
	if info := resp.GetInfo(); info != nil {
		out.Info.ActionCount = info.GetActionCount()
		out.Info.MissedCatchupCount = info.GetMissedCatchupWindow()
		out.Info.OverlapSkipped = info.GetOverlapSkipped()
		out.Info.BufferDropped = info.GetBufferDropped()
		out.Info.BufferSize = info.GetBufferSize()
		for _, rw := range info.GetRunningWorkflows() {
			out.Info.RunningWorkflows = append(out.Info.RunningWorkflows, scheduleRunningEntry{
				WorkflowID: rw.GetWorkflowId(),
				RunID:      rw.GetRunId(),
			})
		}
	}
	body, _ := json.Marshal(out)
	return z.okEnvelope(body)
}

// triggerOverlapEnum maps the wire-side string to schedulepb's enum.
// Unknown values fall back to UNSPECIFIED so the schedule's configured
// policy applies.
func triggerOverlapEnum(s string) enumspb.ScheduleOverlapPolicy {
	switch s {
	case "skip":
		return enumspb.SCHEDULE_OVERLAP_POLICY_SKIP
	case "buffer-one":
		return enumspb.SCHEDULE_OVERLAP_POLICY_BUFFER_ONE
	case "buffer-all":
		return enumspb.SCHEDULE_OVERLAP_POLICY_BUFFER_ALL
	case "cancel-other":
		return enumspb.SCHEDULE_OVERLAP_POLICY_CANCEL_OTHER
	case "terminate-other":
		return enumspb.SCHEDULE_OVERLAP_POLICY_TERMINATE_OTHER
	case "allow-all":
		return enumspb.SCHEDULE_OVERLAP_POLICY_ALLOW_ALL
	default:
		return enumspb.SCHEDULE_OVERLAP_POLICY_UNSPECIFIED
	}
}

// ---- Namespace handlers -------------------------------------------------

func (z *ZAPHandler) handleRegisterNamespace(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req registerNSReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.RegisterNamespaceRequest{
		Namespace:                        req.Name,
		Description:                      req.Description,
		OwnerEmail:                       req.OwnerEmail,
		WorkflowExecutionRetentionPeriod: msDuration(req.RetentionMs),
	}
	if _, err := z.handler.RegisterNamespace(ctx, preq); err != nil {
		return z.errEnvelope(500, err)
	}
	return z.okEnvelope(nil)
}

func (z *ZAPHandler) handleDescribeNamespace(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req describeNSReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	preq := &workflowservice.DescribeNamespaceRequest{Namespace: req.Name}
	resp, err := z.handler.DescribeNamespace(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	ns := describeNSResp{Namespace: nsShapeFromProto(resp)}
	out, _ := json.Marshal(ns)
	return z.okEnvelope(out)
}

func (z *ZAPHandler) handleListNamespaces(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	var req listNSReq
	if err := json.Unmarshal(envelopeBodyBytes(msg), &req); err != nil {
		return z.errEnvelope(400, err)
	}
	if req.PageSize == 0 {
		req.PageSize = 100
	}
	preq := &workflowservice.ListNamespacesRequest{
		PageSize:      req.PageSize,
		NextPageToken: req.NextPageToken,
	}
	resp, err := z.handler.ListNamespaces(ctx, preq)
	if err != nil {
		return z.errEnvelope(500, err)
	}
	out := listNSResp{NextPageToken: resp.GetNextPageToken()}
	for _, n := range resp.GetNamespaces() {
		out.Namespaces = append(out.Namespaces, nsShapeFromDescribe(n))
	}
	outBytes, _ := json.Marshal(out)
	return z.okEnvelope(outBytes)
}

// ---- Health -------------------------------------------------------------

func (z *ZAPHandler) handleHealth(_ context.Context, _ string, _ *zap.Message) (*zap.Message, error) {
	out, _ := json.Marshal(map[string]string{"service": "hanzo-tasks", "status": "SERVING"})
	return z.okEnvelope(out)
}

// ---- Worker poll / respond handlers -------------------------------------

func (z *ZAPHandler) handlePollWorkflowTask(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	ns := root.Text(fNamespace)
	tqName := root.Text(fTaskQueueName)
	tqKind := root.Int8(fTaskQueueKind)
	identity := root.Text(fIdentity)
	build := root.Text(fWorkerBuildID)

	preq := &workflowservice.PollWorkflowTaskQueueRequest{
		Namespace:     defaultIfEmpty(ns),
		TaskQueue:     &taskqueuepb.TaskQueue{Name: tqName, Kind: taskQueueKind(tqKind)},
		Identity:      identity,
		WorkerVersionCapabilities: &commonpb.WorkerVersionCapabilities{BuildId: build},
	}
	resp, err := z.handler.PollWorkflowTaskQueue(ctx, preq)
	if err != nil {
		return z.workerErrEnvelope(err)
	}
	return z.encodeWorkflowTask(resp), nil
}

func (z *ZAPHandler) handlePollActivityTask(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	ns := root.Text(fNamespace)
	tqName := root.Text(fTaskQueueName)
	tqKind := root.Int8(fTaskQueueKind)
	identity := root.Text(fIdentity)

	preq := &workflowservice.PollActivityTaskQueueRequest{
		Namespace: defaultIfEmpty(ns),
		TaskQueue: &taskqueuepb.TaskQueue{Name: tqName, Kind: taskQueueKind(tqKind)},
		Identity:  identity,
	}
	resp, err := z.handler.PollActivityTaskQueue(ctx, preq)
	if err != nil {
		return z.workerErrEnvelope(err)
	}
	return z.encodeActivityTask(resp), nil
}

func (z *ZAPHandler) handleRespondWorkflowTaskCompleted(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	token := root.Bytes(fTaskToken)
	commands := root.Bytes(fCommandsBytes)

	// Phase-1: the worker ships a JSON-envelope of commands. We don't
	// decode commands here — history service will. We forward the
	// completion with empty RespondWorkflowTaskCompletedRequest.Commands
	// (the real commands path decodes separately).
	_ = commands
	preq := &workflowservice.RespondWorkflowTaskCompletedRequest{
		TaskToken: token,
	}
	if _, err := z.handler.RespondWorkflowTaskCompleted(ctx, preq); err != nil {
		return z.workerErrEnvelope(err)
	}
	return z.workerOKEnvelope(), nil
}

func (z *ZAPHandler) handleRespondActivityTaskCompleted(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	token := root.Bytes(fTaskToken)
	result := root.Bytes(fResultBytes)

	// Broker-owned token: deliver to the waiter and short-circuit. No
	// upstream call because there is no server-side activity record
	// for broker-scheduled activities.
	if z.broker.Complete(token, result) {
		return z.workerOKEnvelope(), nil
	}

	preq := &workflowservice.RespondActivityTaskCompletedRequest{
		TaskToken: token,
		Result:    &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: result}}},
	}
	if _, err := z.handler.RespondActivityTaskCompleted(ctx, preq); err != nil {
		return z.workerErrEnvelope(err)
	}
	return z.workerOKEnvelope(), nil
}

func (z *ZAPHandler) handleRespondActivityTaskFailed(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	token := root.Bytes(fTaskToken)
	failure := root.Bytes(fFailureBytes)

	// Broker-owned token: deliver the failure bytes and stop.
	if z.broker.Fail(token, failure) {
		return z.workerOKEnvelope(), nil
	}

	// failure is a serialised temporal.*Error envelope; the history
	// service decodes it. We forward as a Failure with Message=raw
	// so the server sees something non-empty.
	preq := &workflowservice.RespondActivityTaskFailedRequest{
		TaskToken: token,
		Failure: &failurepb.Failure{
			Message: string(failure),
		},
	}
	if _, err := z.handler.RespondActivityTaskFailed(ctx, preq); err != nil {
		return z.workerErrEnvelope(err)
	}
	return z.workerOKEnvelope(), nil
}

func (z *ZAPHandler) handleRecordActivityTaskHeartbeat(ctx context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	token := root.Bytes(fTaskToken)
	details := root.Bytes(fDetailsBytes)

	preq := &workflowservice.RecordActivityTaskHeartbeatRequest{
		TaskToken: token,
		Details:   &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: details}}},
	}
	resp, err := z.handler.RecordActivityTaskHeartbeat(ctx, preq)
	if err != nil {
		return z.workerErrEnvelope(err)
	}
	return z.workerHeartbeatResponse(resp.GetCancelRequested()), nil
}

// ---- Worker wire-frame encoders ----------------------------------------

func (z *ZAPHandler) encodeWorkflowTask(resp *workflowservice.PollWorkflowTaskQueueResponse) *zap.Message {
	b := zap.NewBuilder(256)
	obj := b.StartObject(64)
	if resp == nil {
		obj.FinishAsRoot()
		data := b.Finish()
		m, _ := zap.Parse(data)
		return m
	}
	obj.SetBytes(fTaskToken, resp.GetTaskToken())
	exec := resp.GetWorkflowExecution()
	if exec != nil {
		obj.SetText(fWorkflowID, exec.GetWorkflowId())
		obj.SetText(fRunID, exec.GetRunId())
	}
	if wt := resp.GetWorkflowType(); wt != nil {
		obj.SetText(fWorkflowTypeName, wt.GetName())
	}
	if hist := resp.GetHistory(); hist != nil {
		// Phase-1: forward the raw history marshaled bytes. The worker
		// in Phase-1 reads task.History as the workflow's input JSON;
		// until the history-bytes path is separate, we leave empty so
		// the worker falls back to the zero-arg decode.
		_ = hist
	}
	obj.SetBytes(fNextPageToken, resp.GetNextPageToken())
	obj.FinishAsRoot()
	data := b.Finish()
	m, _ := zap.Parse(data)
	return m
}

func (z *ZAPHandler) encodeActivityTask(resp *workflowservice.PollActivityTaskQueueResponse) *zap.Message {
	b := zap.NewBuilder(256)
	obj := b.StartObject(96)
	if resp == nil {
		obj.FinishAsRoot()
		data := b.Finish()
		m, _ := zap.Parse(data)
		return m
	}
	obj.SetBytes(fTaskToken, resp.GetTaskToken())
	obj.SetText(fActivityID, resp.GetActivityId())
	if at := resp.GetActivityType(); at != nil {
		obj.SetText(fActivityTypeName, at.GetName())
	}
	if input := resp.GetInput(); input != nil && len(input.GetPayloads()) > 0 {
		obj.SetBytes(fInputBytes, input.GetPayloads()[0].GetData())
	}
	if t := resp.GetScheduledTime(); t != nil {
		obj.SetInt64(fScheduledTimeMs, t.AsTime().UnixMilli())
	}
	if d := resp.GetStartToCloseTimeout(); d != nil {
		obj.SetInt64(fStartToCloseTimeoutMs, d.AsDuration().Milliseconds())
	}
	if d := resp.GetHeartbeatTimeout(); d != nil {
		obj.SetInt64(fHeartbeatTimeoutMs, d.AsDuration().Milliseconds())
	}
	if exec := resp.GetWorkflowExecution(); exec != nil {
		obj.SetText(fActivityWorkflowID, exec.GetWorkflowId())
		obj.SetText(fActivityRunID, exec.GetRunId())
	}
	obj.FinishAsRoot()
	data := b.Finish()
	m, _ := zap.Parse(data)
	return m
}

func (z *ZAPHandler) workerOKEnvelope() *zap.Message {
	b := zap.NewBuilder(32)
	obj := b.StartObject(16)
	obj.FinishAsRoot()
	data := b.Finish()
	m, _ := zap.Parse(data)
	return m
}

func (z *ZAPHandler) workerHeartbeatResponse(cancelRequested bool) *zap.Message {
	b := zap.NewBuilder(32)
	obj := b.StartObject(16)
	obj.SetBool(fRespCancelRequested, cancelRequested)
	obj.FinishAsRoot()
	data := b.Finish()
	m, _ := zap.Parse(data)
	return m
}

func (z *ZAPHandler) workerErrEnvelope(err error) (*zap.Message, error) {
	// Worker RPCs use the same generic envelope but populate
	// envelopeStatus + envelopeError so the client surfaces the error
	// without mis-parsing the task fields.
	return z.errEnvelope(500, err)
}

// ---- helpers ------------------------------------------------------------

func defaultIfEmpty(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func msDuration(ms int64) *durationpb.Duration {
	if ms <= 0 {
		return nil
	}
	return durationpb.New(time.Duration(ms) * time.Millisecond)
}

func msToDuration(ms int64) time.Duration {
	return time.Duration(ms) * time.Millisecond
}

func taskQueueKind(k int8) enumspb.TaskQueueKind {
	switch k {
	case 1:
		return enumspb.TASK_QUEUE_KIND_STICKY
	default:
		return enumspb.TASK_QUEUE_KIND_NORMAL
	}
}

func encodeArgsPayloads(args []any) (*commonpb.Payloads, error) {
	payloads := &commonpb.Payloads{}
	for _, a := range args {
		data, err := json.Marshal(a)
		if err != nil {
			return nil, fmt.Errorf("encode arg: %w", err)
		}
		payloads.Payloads = append(payloads.Payloads, &commonpb.Payload{
			Metadata: map[string][]byte{"encoding": []byte("json/plain")},
			Data:     data,
		})
	}
	return payloads, nil
}

func wfInfoFromProto(info *workflowpb.WorkflowExecutionInfo) wfInfo {
	out := wfInfo{}
	if info == nil {
		return out
	}
	if e := info.GetExecution(); e != nil {
		out.WorkflowID = e.GetWorkflowId()
		out.RunID = e.GetRunId()
	}
	if t := info.GetType(); t != nil {
		out.WorkflowType = t.GetName()
	}
	if t := info.GetStartTime(); t != nil {
		out.StartTime = t.AsTime()
	}
	if t := info.GetCloseTime(); t != nil {
		out.CloseTime = t.AsTime()
	}
	out.Status = statusFromProto(info.GetStatus())
	out.HistoryLength = info.GetHistoryLength()
	if tq := info.GetTaskQueue(); tq != "" {
		out.TaskQueue = tq
	}
	return out
}

// statusFromProto maps the upstream WorkflowExecutionStatus to the
// compact Int8 enum used by the SDK's schema.
func statusFromProto(s enumspb.WorkflowExecutionStatus) int8 {
	switch s {
	case enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
		return 1
	case enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED:
		return 2
	case enumspb.WORKFLOW_EXECUTION_STATUS_FAILED:
		return 3
	case enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED:
		return 4
	case enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED:
		return 5
	case enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW:
		return 6
	case enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT:
		return 7
	default:
		return 0
	}
}

func nsShapeFromProto(r *workflowservice.DescribeNamespaceResponse) nsShape {
	out := nsShape{}
	if r == nil {
		return out
	}
	if info := r.GetNamespaceInfo(); info != nil {
		out.Info.Name = info.GetName()
		out.Info.State = nsStateFromProto(info.GetState())
		out.Info.Description = info.GetDescription()
		out.Info.OwnerEmail = info.GetOwnerEmail()
		out.Info.ID = info.GetId()
	}
	if cfg := r.GetConfig(); cfg != nil {
		if d := cfg.GetWorkflowExecutionRetentionTtl(); d != nil {
			out.Config.RetentionMs = d.AsDuration().Milliseconds()
		}
	}
	return out
}

func nsShapeFromDescribe(d *workflowservice.DescribeNamespaceResponse) nsShape {
	return nsShapeFromProto(d)
}

func nsStateFromProto(s enumspb.NamespaceState) int8 {
	switch s {
	case enumspb.NAMESPACE_STATE_REGISTERED:
		return 1
	case enumspb.NAMESPACE_STATE_DEPRECATED:
		return 2
	case enumspb.NAMESPACE_STATE_DELETED:
		return 3
	default:
		return 0
	}
}

func retryPolicyToProto(rp *retryPolicy) *commonpb.RetryPolicy {
	if rp == nil {
		return nil
	}
	out := &commonpb.RetryPolicy{
		BackoffCoefficient:     rp.BackoffCoefficient,
		MaximumAttempts:        rp.MaximumAttempts,
		NonRetryableErrorTypes: rp.NonRetryableErrorTypes,
	}
	if rp.InitialIntervalMs > 0 {
		out.InitialInterval = durationpb.New(time.Duration(rp.InitialIntervalMs) * time.Millisecond)
	}
	if rp.MaximumIntervalMs > 0 {
		out.MaximumInterval = durationpb.New(time.Duration(rp.MaximumIntervalMs) * time.Millisecond)
	}
	return out
}
