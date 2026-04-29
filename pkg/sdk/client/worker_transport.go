// Copyright © 2026 Hanzo AI. MIT License.

package client

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/luxfi/zap"
)

// NewWorkerTransport returns a typed wrapper over the generic Transport
// that issues the worker subscribe / respond RPCs and routes server-pushed
// task deliveries to user-installed callbacks.
//
// The wire layout for Subscribe / Respond / Heartbeat / Schedule follows
// the constants declared in transport.go. Server-push deliveries
// (OpcodeDeliverWorkflowTask / ActivityTask / ActivityResult) carry
// JSON bodies matching the workflowTaskDeliveryJSON /
// activityTaskDeliveryJSON / activityResultDeliveryJSON shapes in
// pkg/tasks/dispatch.go — those are authoritative.
func NewWorkerTransport(t Transport) WorkerTransport {
	return &workerTransport{t: t}
}

// workerTransport is the default WorkerTransport implementation. It
// wraps a generic Transport, encodes worker calls, and demultiplexes
// server-pushed deliveries into per-kind callbacks.
type workerTransport struct {
	t Transport

	mu        sync.RWMutex
	onWFTask  func(*WorkflowTask)
	onActTask func(*ActivityTask)
	onActRes  func(activityID string, result, failure []byte)

	// installed tracks which delivery opcodes already have a Transport
	// Handle registered. We register lazily on the first On* call so a
	// Transport that does not need pushes (a Client doing user-facing
	// RPCs only) never installs handlers.
	installed map[uint16]bool
}

// Close releases the underlying transport.
func (w *workerTransport) Close() error {
	if w.t == nil {
		return nil
	}
	return w.t.Close()
}

// ── subscriptions ──────────────────────────────────────────────────────

// SubscribeWorkflowTasks registers this worker for workflow-task pushes.
// The server returns a sub-id used to Unsubscribe on Stop.
func (w *workerTransport) SubscribeWorkflowTasks(ctx context.Context, req PollWorkflowTaskRequest) (string, error) {
	body, err := json.Marshal(struct {
		Namespace     string `json:"namespace"`
		TaskQueue     string `json:"task_queue"`
		TaskQueueKind int8   `json:"task_queue_kind,omitempty"`
		Identity      string `json:"identity,omitempty"`
		WorkerBuildID string `json:"worker_build_id,omitempty"`
	}{
		Namespace:     req.Namespace,
		TaskQueue:     req.TaskQueueName,
		TaskQueueKind: req.TaskQueueKind,
		Identity:      req.Identity,
		WorkerBuildID: req.WorkerBuildID,
	})
	if err != nil {
		return "", fmt.Errorf("subscribe workflow tasks: marshal: %w", err)
	}
	respFrame, err := w.t.Call(ctx, OpcodeSubscribeWorkflowTasks, body)
	if err != nil {
		return "", fmt.Errorf("subscribe workflow tasks: %w", err)
	}
	return decodeSubscribeResp(respFrame)
}

// SubscribeActivityTasks registers this worker for activity-task pushes.
func (w *workerTransport) SubscribeActivityTasks(ctx context.Context, req PollActivityTaskRequest) (string, error) {
	body, err := json.Marshal(struct {
		Namespace     string `json:"namespace"`
		TaskQueue     string `json:"task_queue"`
		TaskQueueKind int8   `json:"task_queue_kind,omitempty"`
		Identity      string `json:"identity,omitempty"`
	}{
		Namespace:     req.Namespace,
		TaskQueue:     req.TaskQueueName,
		TaskQueueKind: req.TaskQueueKind,
		Identity:      req.Identity,
	})
	if err != nil {
		return "", fmt.Errorf("subscribe activity tasks: marshal: %w", err)
	}
	respFrame, err := w.t.Call(ctx, OpcodeSubscribeActivityTasks, body)
	if err != nil {
		return "", fmt.Errorf("subscribe activity tasks: %w", err)
	}
	return decodeSubscribeResp(respFrame)
}

// Unsubscribe tears down a subscription. Idempotent on the server side —
// an unknown sub-id is treated as already gone.
func (w *workerTransport) Unsubscribe(ctx context.Context, subID string) error {
	body, err := json.Marshal(struct {
		SubID string `json:"subscription_id"`
	}{SubID: subID})
	if err != nil {
		return fmt.Errorf("unsubscribe: marshal: %w", err)
	}
	if _, err := w.t.Call(ctx, OpcodeUnsubscribeTasks, body); err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	return nil
}

// decodeSubscribeResp pulls the sub-id out of the standard envelope.
func decodeSubscribeResp(frame []byte) (string, error) {
	status, detail, payload, err := parseEnvelope(frame)
	if err != nil {
		return "", fmt.Errorf("subscribe decode: %w", err)
	}
	if status != 0 && status != 200 {
		return "", fmt.Errorf("subscribe: status %d: %s", status, detail)
	}
	var resp struct {
		SubID string `json:"subscription_id"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &resp); err != nil {
			return "", fmt.Errorf("subscribe body: %w", err)
		}
	}
	return resp.SubID, nil
}

// ── server-pushed delivery handlers ────────────────────────────────────

// OnWorkflowTask installs fn as the workflow-task delivery callback and
// (lazily) registers a Transport.Handle for OpcodeDeliverWorkflowTask.
func (w *workerTransport) OnWorkflowTask(fn func(*WorkflowTask)) {
	w.mu.Lock()
	w.onWFTask = fn
	already := w.installed[OpcodeDeliverWorkflowTask]
	if w.installed == nil {
		w.installed = make(map[uint16]bool)
	}
	w.installed[OpcodeDeliverWorkflowTask] = true
	w.mu.Unlock()
	if already || w.t == nil {
		return
	}
	w.t.Handle(OpcodeDeliverWorkflowTask, func(_ string, body []byte) {
		task := decodeWorkflowDelivery(body)
		if task == nil {
			return
		}
		w.mu.RLock()
		cb := w.onWFTask
		w.mu.RUnlock()
		if cb != nil {
			cb(task)
		}
	})
}

// OnActivityTask installs fn as the activity-task delivery callback.
func (w *workerTransport) OnActivityTask(fn func(*ActivityTask)) {
	w.mu.Lock()
	w.onActTask = fn
	already := w.installed[OpcodeDeliverActivityTask]
	if w.installed == nil {
		w.installed = make(map[uint16]bool)
	}
	w.installed[OpcodeDeliverActivityTask] = true
	w.mu.Unlock()
	if already || w.t == nil {
		return
	}
	w.t.Handle(OpcodeDeliverActivityTask, func(_ string, body []byte) {
		task := decodeActivityDelivery(body)
		if task == nil {
			return
		}
		w.mu.RLock()
		cb := w.onActTask
		w.mu.RUnlock()
		if cb != nil {
			cb(task)
		}
	})
}

// OnActivityResult installs fn as the activity-result delivery callback.
func (w *workerTransport) OnActivityResult(fn func(activityID string, result, failure []byte)) {
	w.mu.Lock()
	w.onActRes = fn
	already := w.installed[OpcodeDeliverActivityResult]
	if w.installed == nil {
		w.installed = make(map[uint16]bool)
	}
	w.installed[OpcodeDeliverActivityResult] = true
	w.mu.Unlock()
	if already || w.t == nil {
		return
	}
	w.t.Handle(OpcodeDeliverActivityResult, func(_ string, body []byte) {
		id, result, failure := decodeActivityResultDelivery(body)
		w.mu.RLock()
		cb := w.onActRes
		w.mu.RUnlock()
		if cb != nil {
			cb(id, result, failure)
		}
	})
}

// decodeWorkflowDelivery parses the JSON shape from
// pkg/tasks/dispatch.go workflowTaskDeliveryJSON. task_token and input
// arrive as plain strings on the wire (no base64); we cast to []byte.
func decodeWorkflowDelivery(body []byte) *WorkflowTask {
	var msg struct {
		TaskToken        string `json:"task_token"`
		WorkflowID       string `json:"workflow_id"`
		RunID            string `json:"run_id"`
		WorkflowTypeName string `json:"workflow_type_name"`
		Input            string `json:"input,omitempty"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.TaskToken == "" {
		return nil
	}
	return &WorkflowTask{
		TaskToken:        []byte(msg.TaskToken),
		WorkflowID:       msg.WorkflowID,
		RunID:            msg.RunID,
		WorkflowTypeName: msg.WorkflowTypeName,
		History:          []byte(msg.Input),
	}
}

// decodeActivityDelivery parses the JSON shape from
// pkg/tasks/dispatch.go activityTaskDeliveryJSON.
func decodeActivityDelivery(body []byte) *ActivityTask {
	var msg struct {
		TaskToken             string `json:"task_token"`
		WorkflowID            string `json:"workflow_id"`
		RunID                 string `json:"run_id"`
		ActivityID            string `json:"activity_id"`
		ActivityTypeName      string `json:"activity_type_name"`
		Input                 string `json:"input,omitempty"`
		ScheduledTimeMs       int64  `json:"scheduled_time_ms"`
		StartToCloseTimeoutMs int64  `json:"start_to_close_timeout_ms,omitempty"`
		HeartbeatTimeoutMs    int64  `json:"heartbeat_timeout_ms,omitempty"`
	}
	if err := json.Unmarshal(body, &msg); err != nil || msg.TaskToken == "" {
		return nil
	}
	return &ActivityTask{
		TaskToken:             []byte(msg.TaskToken),
		WorkflowID:            msg.WorkflowID,
		RunID:                 msg.RunID,
		ActivityID:            msg.ActivityID,
		ActivityTypeName:      msg.ActivityTypeName,
		Input:                 []byte(msg.Input),
		ScheduledTimeMs:       msg.ScheduledTimeMs,
		StartToCloseTimeoutMs: msg.StartToCloseTimeoutMs,
		HeartbeatTimeoutMs:    msg.HeartbeatTimeoutMs,
	}
}

// decodeActivityResultDelivery parses the JSON shape from
// pkg/tasks/dispatch.go activityResultDeliveryJSON.
func decodeActivityResultDelivery(body []byte) (string, []byte, []byte) {
	var msg struct {
		ActivityID string `json:"activity_id"`
		Result     string `json:"result,omitempty"`
		Failure    string `json:"failure,omitempty"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", nil, nil
	}
	return msg.ActivityID, []byte(msg.Result), []byte(msg.Failure)
}

// ── responses ──────────────────────────────────────────────────────────

// RespondWorkflowTaskCompleted uploads the commands list for a finished
// workflow task.
func (w *workerTransport) RespondWorkflowTaskCompleted(ctx context.Context, req RespondWorkflowTaskCompletedRequest) error {
	body := encodeRespondWorkflowCompleted(req)
	_, err := w.t.Call(ctx, OpcodeRespondWorkflowTaskCompleted, body)
	if err != nil {
		return fmt.Errorf("worker respond workflow completed: %w", err)
	}
	return nil
}

// RespondActivityTaskCompleted uploads the activity result.
func (w *workerTransport) RespondActivityTaskCompleted(ctx context.Context, req RespondActivityTaskCompletedRequest) error {
	body := encodeRespondActivityCompleted(req)
	_, err := w.t.Call(ctx, OpcodeRespondActivityTaskCompleted, body)
	if err != nil {
		return fmt.Errorf("worker respond activity completed: %w", err)
	}
	return nil
}

// RespondActivityTaskFailed uploads the activity failure.
func (w *workerTransport) RespondActivityTaskFailed(ctx context.Context, req RespondActivityTaskFailedRequest) error {
	body := encodeRespondActivityFailed(req)
	_, err := w.t.Call(ctx, OpcodeRespondActivityTaskFailed, body)
	if err != nil {
		return fmt.Errorf("worker respond activity failed: %w", err)
	}
	return nil
}

// RecordActivityTaskHeartbeat signals liveness and returns the
// server's cancel-requested flag.
func (w *workerTransport) RecordActivityTaskHeartbeat(ctx context.Context, req RecordActivityTaskHeartbeatRequest) (bool, error) {
	body := encodeHeartbeatReq(req)
	respBytes, err := w.t.Call(ctx, OpcodeRecordActivityTaskHeartbeat, body)
	if err != nil {
		return false, fmt.Errorf("worker heartbeat: %w", err)
	}
	return decodeHeartbeatResp(respBytes), nil
}

// ScheduleActivity issues opcode 0x006B. The body is a JSON document
// matching the v1 envelope used for user-facing RPCs; the frontend
// decodes it into its native schedule-activity request.
func (w *workerTransport) ScheduleActivity(ctx context.Context, req ScheduleActivityRequest) (*ScheduleActivityResponse, error) {
	bodyJSON := struct {
		Namespace      string           `json:"namespace"`
		WorkflowID     string           `json:"workflow_id"`
		RunID          string           `json:"run_id,omitempty"`
		TaskQueue      string           `json:"task_queue"`
		ActivityType   string           `json:"activity_type"`
		Input          []byte           `json:"input,omitempty"`
		StartToCloseMs int64            `json:"start_to_close_ms,omitempty"`
		HeartbeatMs    int64            `json:"heartbeat_ms,omitempty"`
		RetryPolicy    *RetryPolicyJSON `json:"retry_policy,omitempty"`
	}{
		Namespace:      req.Namespace,
		WorkflowID:     req.WorkflowID,
		RunID:          req.RunID,
		TaskQueue:      req.TaskQueue,
		ActivityType:   req.ActivityType,
		Input:          req.Input,
		StartToCloseMs: req.StartToCloseMs,
		HeartbeatMs:    req.HeartbeatMs,
		RetryPolicy:    req.RetryPolicy,
	}
	body, err := json.Marshal(bodyJSON)
	if err != nil {
		return nil, fmt.Errorf("schedule activity: marshal: %w", err)
	}
	respFrame, err := w.t.Call(ctx, OpcodeScheduleActivity, body)
	if err != nil {
		return nil, fmt.Errorf("schedule activity: %w", err)
	}
	status, detail, payload, perr := parseEnvelope(respFrame)
	if perr != nil {
		return nil, fmt.Errorf("schedule activity decode: %w", perr)
	}
	if status != 0 && status != 200 {
		return nil, fmt.Errorf("schedule activity: status %d: %s", status, detail)
	}
	var resp struct {
		ActivityTaskID string `json:"activity_task_id"`
		TaskToken      string `json:"task_token,omitempty"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &resp); err != nil {
			return nil, fmt.Errorf("schedule activity body: %w", err)
		}
	}
	return &ScheduleActivityResponse{
		ActivityTaskID: resp.ActivityTaskID,
		TaskToken:      []byte(resp.TaskToken),
	}, nil
}

// StartChildWorkflow issues opcode 0x006D.
func (w *workerTransport) StartChildWorkflow(ctx context.Context, req StartChildWorkflowRequest) (*StartChildWorkflowResponse, error) {
	bodyJSON := struct {
		Namespace    string           `json:"namespace"`
		ParentID     string           `json:"parent_id"`
		ParentRunID  string           `json:"parent_run_id,omitempty"`
		WorkflowID   string           `json:"workflow_id"`
		WorkflowType string           `json:"workflow_type"`
		TaskQueue    string           `json:"task_queue"`
		Input        []any            `json:"input,omitempty"`
		RetryPolicy  *RetryPolicyJSON `json:"retry_policy,omitempty"`
		Timeouts     TimeoutsJSON     `json:"timeouts,omitempty"`
	}{
		Namespace:    req.Namespace,
		ParentID:     req.ParentID,
		ParentRunID:  req.ParentRunID,
		WorkflowID:   req.WorkflowID,
		WorkflowType: req.WorkflowType,
		TaskQueue:    req.TaskQueue,
		Input:        req.Input,
		RetryPolicy:  req.RetryPolicy,
		Timeouts:     req.TimeoutsMs,
	}
	body, err := json.Marshal(bodyJSON)
	if err != nil {
		return nil, fmt.Errorf("start child: marshal: %w", err)
	}
	respFrame, err := w.t.Call(ctx, OpcodeStartChildWorkflow, body)
	if err != nil {
		return nil, fmt.Errorf("start child: %w", err)
	}
	status, detail, payload, perr := parseEnvelope(respFrame)
	if perr != nil {
		return nil, fmt.Errorf("start child decode: %w", perr)
	}
	if status != 0 && status != 200 {
		return nil, fmt.Errorf("start child: status %d: %s", status, detail)
	}
	var resp struct {
		RunID string `json:"run_id"`
	}
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &resp); err != nil {
			return nil, fmt.Errorf("start child body: %w", err)
		}
	}
	return &StartChildWorkflowResponse{RunID: resp.RunID}, nil
}

// parseEnvelope is worker_transport's copy of the envelope decode
// used by user-facing RPCs in client.go.
func parseEnvelope(frame []byte) (uint32, string, []byte, error) {
	msg, err := zap.Parse(frame)
	if err != nil {
		return 0, "", nil, err
	}
	root := msg.Root()
	const envBody, envStatus, envError = 0, 8, 12
	status := root.Uint32(envStatus)
	detail := string(root.Bytes(envError))
	body := root.Bytes(envBody)
	return status, detail, body, nil
}

// encodeRespondWorkflowCompleted serialises the commands blob.
func encodeRespondWorkflowCompleted(req RespondWorkflowTaskCompletedRequest) []byte {
	b := zap.NewBuilder(len(req.TaskToken) + len(req.Commands) + 64)
	obj := b.StartObject(32)
	obj.SetBytes(FieldTaskToken, req.TaskToken)
	obj.SetBytes(FieldCommandsBytes, req.Commands)
	obj.FinishAsRoot()
	return b.Finish()
}

// encodeRespondActivityCompleted serialises an activity success.
func encodeRespondActivityCompleted(req RespondActivityTaskCompletedRequest) []byte {
	b := zap.NewBuilder(len(req.TaskToken) + len(req.Result) + 64)
	obj := b.StartObject(32)
	obj.SetBytes(FieldTaskToken, req.TaskToken)
	obj.SetBytes(FieldResultBytes, req.Result)
	obj.FinishAsRoot()
	return b.Finish()
}

// encodeRespondActivityFailed serialises an activity failure.
func encodeRespondActivityFailed(req RespondActivityTaskFailedRequest) []byte {
	b := zap.NewBuilder(len(req.TaskToken) + len(req.Failure) + 64)
	obj := b.StartObject(32)
	obj.SetBytes(FieldTaskToken, req.TaskToken)
	obj.SetBytes(FieldFailureBytes, req.Failure)
	obj.FinishAsRoot()
	return b.Finish()
}

// encodeHeartbeatReq serialises a heartbeat request.
func encodeHeartbeatReq(req RecordActivityTaskHeartbeatRequest) []byte {
	b := zap.NewBuilder(len(req.TaskToken) + len(req.Details) + 64)
	obj := b.StartObject(32)
	obj.SetBytes(FieldTaskToken, req.TaskToken)
	obj.SetBytes(FieldDetailsBytes, req.Details)
	obj.FinishAsRoot()
	return b.Finish()
}

// decodeHeartbeatResp reads the cancelRequested flag from the
// heartbeat response.
func decodeHeartbeatResp(frame []byte) bool {
	msg, err := zap.Parse(frame)
	if err != nil {
		return false
	}
	root := msg.Root()
	return root.Bool(FieldRespCancelRequested)
}

// copyBytes returns an independent copy of b so the returned slice
// does not alias the transport's receive buffer.
func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// statusFromFrame extracts the uint32 status field from a response
// frame using binary.LittleEndian. Exposed for tests that need to
// assert on the raw status without parsing the whole envelope.
func statusFromFrame(frame []byte) uint32 {
	if len(frame) < zap.HeaderSize+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(frame[zap.HeaderSize:])
}
