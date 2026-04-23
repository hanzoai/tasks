// Copyright © 2026 Hanzo AI. MIT License.

package client

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/luxfi/zap"
)

// NewWorkerTransport returns a typed wrapper over the generic Transport
// that issues the worker poll / respond RPCs defined in schema/tasks.zap.
//
// The wire layout (object fields) follows the constants declared in
// transport.go. v1 encodes the body portions as JSON; native ZAP serde
// replaces JSON in a follow-up without changing opcodes.
func NewWorkerTransport(t Transport) WorkerTransport {
	return &workerTransport{t: t}
}

// workerTransport is the default WorkerTransport implementation. It
// wraps a generic Transport and handles the request/response ZAP
// encoding for each worker RPC.
type workerTransport struct {
	t Transport
}

// Close releases the underlying transport.
func (w *workerTransport) Close() error {
	if w.t == nil {
		return nil
	}
	return w.t.Close()
}

// PollWorkflowTask issues OpcodePollWorkflowTask and decodes the
// returned WorkflowTask. A zero-token response signals "idle, try
// again" — the caller's loop re-polls.
func (w *workerTransport) PollWorkflowTask(ctx context.Context, req PollWorkflowTaskRequest) (*WorkflowTask, error) {
	body := encodePollWorkflowReq(req)
	respBytes, err := w.t.Call(ctx, OpcodePollWorkflowTask, body)
	if err != nil {
		return nil, fmt.Errorf("worker poll workflow task: %w", err)
	}
	task := decodeWorkflowTask(respBytes)
	if len(task.TaskToken) == 0 {
		return nil, nil
	}
	return task, nil
}

// PollActivityTask issues OpcodePollActivityTask.
func (w *workerTransport) PollActivityTask(ctx context.Context, req PollActivityTaskRequest) (*ActivityTask, error) {
	body := encodePollActivityReq(req)
	respBytes, err := w.t.Call(ctx, OpcodePollActivityTask, body)
	if err != nil {
		return nil, fmt.Errorf("worker poll activity task: %w", err)
	}
	task := decodeActivityTask(respBytes)
	if len(task.TaskToken) == 0 {
		return nil, nil
	}
	return task, nil
}

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

// encodePollWorkflowReq serialises a PollWorkflowTaskRequest into the
// object-field layout declared in transport.go.
func encodePollWorkflowReq(req PollWorkflowTaskRequest) []byte {
	b := zap.NewBuilder(256)
	obj := b.StartObject(64)
	obj.SetText(FieldNamespace, req.Namespace)
	obj.SetText(FieldTaskQueueName, req.TaskQueueName)
	obj.SetInt8(FieldTaskQueueKind, req.TaskQueueKind)
	obj.SetText(FieldIdentity, req.Identity)
	obj.SetText(FieldWorkerBuildID, req.WorkerBuildID)
	obj.FinishAsRoot()
	return b.Finish()
}

// encodePollActivityReq serialises a PollActivityTaskRequest.
func encodePollActivityReq(req PollActivityTaskRequest) []byte {
	b := zap.NewBuilder(256)
	obj := b.StartObject(48)
	obj.SetText(FieldNamespace, req.Namespace)
	obj.SetText(FieldTaskQueueName, req.TaskQueueName)
	obj.SetInt8(FieldTaskQueueKind, req.TaskQueueKind)
	obj.SetText(FieldIdentity, req.Identity)
	obj.FinishAsRoot()
	return b.Finish()
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

// decodeWorkflowTask parses a response frame into a WorkflowTask.
func decodeWorkflowTask(frame []byte) *WorkflowTask {
	msg, err := zap.Parse(frame)
	if err != nil {
		return &WorkflowTask{}
	}
	root := msg.Root()
	return &WorkflowTask{
		TaskToken:        copyBytes(root.Bytes(FieldTaskToken)),
		WorkflowID:       root.Text(FieldWorkflowID),
		RunID:            root.Text(FieldRunID),
		WorkflowTypeName: root.Text(FieldWorkflowTypeName),
		History:          copyBytes(root.Bytes(FieldHistoryBytes)),
		NextPageToken:    copyBytes(root.Bytes(FieldNextPageToken)),
	}
}

// decodeActivityTask parses a response frame into an ActivityTask.
func decodeActivityTask(frame []byte) *ActivityTask {
	msg, err := zap.Parse(frame)
	if err != nil {
		return &ActivityTask{}
	}
	root := msg.Root()
	return &ActivityTask{
		TaskToken:             copyBytes(root.Bytes(FieldTaskToken)),
		WorkflowID:            root.Text(FieldActivityWorkflowID),
		RunID:                 root.Text(FieldActivityRunID),
		ActivityID:            root.Text(FieldActivityID),
		ActivityTypeName:      root.Text(FieldActivityTypeName),
		Input:                 copyBytes(root.Bytes(FieldInputBytes)),
		ScheduledTimeMs:       root.Int64(FieldScheduledTimeMs),
		StartToCloseTimeoutMs: root.Int64(FieldStartToCloseTimeoutMs),
		HeartbeatTimeoutMs:    root.Int64(FieldHeartbeatTimeoutMs),
	}
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
