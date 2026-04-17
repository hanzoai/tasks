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
	"go.temporal.io/api/enums/v1"
	schedulepb "go.temporal.io/api/schedule/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// ZAP opcodes for SDK task submission (must match pkg/tasks/client.go).
const (
	opcodeTaskSubmit   uint16 = 0x0050
	opcodeTaskSchedule uint16 = 0x0051
)

// ZAP field offsets (must match pkg/tasks/client.go).
const (
	zapFieldTaskType = 0
	zapFieldPayload  = 8
	zapFieldInterval = 16
	zapRespStatus    = 0
	zapRespBody      = 4
)

// ZAPHandler accepts task submissions over ZAP binary transport.
// It translates JSON-over-ZAP messages into workflow service calls.
type ZAPHandler struct {
	handler Handler
	node    *zap.Node
	port    int
	logger  *slog.Logger
}

// NewZAPHandler creates a ZAP handler backed by the frontend workflow handler.
func NewZAPHandler(handler Handler, logger *slog.Logger) *ZAPHandler {
	port := 0
	if p := os.Getenv("ZAP_SDK_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	if port == 0 {
		port = 9652
	}

	nodeID, _ := os.Hostname()
	if nodeID == "" {
		nodeID = "tasks-zap"
	}

	return &ZAPHandler{
		handler: handler,
		port:    port,
		logger:  logger,
		node: zap.NewNode(zap.NodeConfig{
			NodeID:      nodeID + "-sdk",
			ServiceType: "_tasks-sdk._tcp",
			Port:        port,
			Logger:      logger,
			NoDiscovery: true,
		}),
	}
}

// Start registers handlers and starts the ZAP listener.
func (z *ZAPHandler) Start() error {
	z.node.Handle(opcodeTaskSubmit, z.handleSubmit)
	z.node.Handle(opcodeTaskSchedule, z.handleSchedule)

	if err := z.node.Start(); err != nil {
		return fmt.Errorf("zap sdk handler start: %w", err)
	}

	z.logger.Info("ZAP SDK handler listening", "port", z.port)
	return nil
}

// Stop shuts down the ZAP listener.
func (z *ZAPHandler) Stop() {
	if z.node != nil {
		z.node.Stop()
	}
}

// handleSubmit processes a one-shot task submission (opcode 0x0050).
func (z *ZAPHandler) handleSubmit(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	taskType := root.Text(zapFieldTaskType)
	payloadBytes := root.Bytes(zapFieldPayload)

	if taskType == "" {
		return z.errorResponse(400, "task type required")
	}

	z.logger.Debug("zap: task submit", "type", taskType, "from", from, "payloadLen", len(payloadBytes))

	// Build StartWorkflowExecution request.
	req := &workflowservice.StartWorkflowExecutionRequest{
		Namespace:    "default",
		WorkflowId:   fmt.Sprintf("%s-%d", taskType, time.Now().UnixNano()),
		WorkflowType: &commonpb.WorkflowType{Name: taskType},
		TaskQueue:    &taskqueuepb.TaskQueue{Name: "default", Kind: enums.TASK_QUEUE_KIND_NORMAL},
		Input:        &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: payloadBytes}}},
		RequestId:    fmt.Sprintf("zap-%d", time.Now().UnixNano()),
	}

	resp, err := z.handler.StartWorkflowExecution(ctx, req)
	if err != nil {
		z.logger.Warn("zap: workflow start failed", "type", taskType, "error", err)
		return z.errorResponse(500, err.Error())
	}

	body, _ := json.Marshal(map[string]string{
		"run_id": resp.GetRunId(),
	})
	return z.okResponse(body)
}

// handleSchedule processes a recurring schedule creation (opcode 0x0051).
func (z *ZAPHandler) handleSchedule(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
	root := msg.Root()
	name := root.Text(zapFieldTaskType) // field 0 = schedule name
	intervalStr := root.Text(zapFieldInterval)

	if name == "" {
		return z.errorResponse(400, "schedule name required")
	}

	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		return z.errorResponse(400, fmt.Sprintf("invalid interval %q: %v", intervalStr, err))
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
						TaskQueue:    &taskqueuepb.TaskQueue{Name: "default", Kind: enums.TASK_QUEUE_KIND_NORMAL},
					},
				},
			},
		},
		RequestId: fmt.Sprintf("zap-sched-%d", time.Now().UnixNano()),
	}

	_, err = z.handler.CreateSchedule(ctx, req)
	if err != nil {
		z.logger.Warn("zap: schedule create failed", "name", name, "error", err)
		return z.errorResponse(500, err.Error())
	}

	body, _ := json.Marshal(map[string]string{"schedule_id": name})
	return z.okResponse(body)
}

func (z *ZAPHandler) okResponse(body []byte) (*zap.Message, error) {
	return z.buildResponse(200, body)
}

func (z *ZAPHandler) errorResponse(status uint32, message string) (*zap.Message, error) {
	body, _ := json.Marshal(map[string]string{"error": message})
	return z.buildResponse(status, body)
}

func (z *ZAPHandler) buildResponse(status uint32, body []byte) (*zap.Message, error) {
	b := zap.NewBuilder(len(body) + 64)
	obj := b.StartObject(16)
	obj.SetUint32(zapRespStatus, status)
	obj.SetBytes(zapRespBody, body)
	obj.FinishAsRoot()
	data := b.Finish()
	msg, err := zap.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("zap build response: %w", err)
	}
	return msg, nil
}

