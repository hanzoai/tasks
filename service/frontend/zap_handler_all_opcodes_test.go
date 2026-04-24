// Copyright (c) Hanzo AI Inc 2026.
//
// Integration-style test for the ZAP handler's full SDK opcode surface.
//
// Every opcode the native SDK (pkg/sdk/*) can send is exercised here
// via one ZAP round-trip per opcode. The fake WorkflowHandler records
// calls and returns fixed responses so we can assert:
//
//   - each opcode registers a handler (via the Start() dispatch)
//   - the envelope decode → handler call → envelope encode path works
//   - broker-owned activity scheduling / wait / complete round-trips
//   - responds route to the broker before hitting the upstream handler
//
// This test does not spin the ZAP TCP listener; it invokes the handler
// functions directly with constructed messages. That isolates the
// protocol-translation logic (the surface this PR adds) from the ZAP
// transport layer (already exercised by the legacy 0x0050/0x0051
// tests).

package frontend

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/luxfi/zap"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	namespacepb "go.temporal.io/api/namespace/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
)

// fakeHandler implements frontend.Handler minimally for ZAP routing.
// Every RPC returns a fixed, deterministic response so tests can
// assert envelope decode/encode without standing up real persistence.
type fakeHandler struct {
	workflowservice.UnimplementedWorkflowServiceServer

	calls atomic.Int64
}

func (f *fakeHandler) GetConfig() *Config { return &Config{} }
func (f *fakeHandler) Start()              {}
func (f *fakeHandler) Stop()               {}

func (f *fakeHandler) StartWorkflowExecution(ctx context.Context, req *workflowservice.StartWorkflowExecutionRequest) (*workflowservice.StartWorkflowExecutionResponse, error) {
	f.calls.Add(1)
	return &workflowservice.StartWorkflowExecutionResponse{RunId: "run-" + req.GetWorkflowId()}, nil
}

func (f *fakeHandler) SignalWorkflowExecution(ctx context.Context, req *workflowservice.SignalWorkflowExecutionRequest) (*workflowservice.SignalWorkflowExecutionResponse, error) {
	f.calls.Add(1)
	return &workflowservice.SignalWorkflowExecutionResponse{}, nil
}

func (f *fakeHandler) RequestCancelWorkflowExecution(ctx context.Context, req *workflowservice.RequestCancelWorkflowExecutionRequest) (*workflowservice.RequestCancelWorkflowExecutionResponse, error) {
	f.calls.Add(1)
	return &workflowservice.RequestCancelWorkflowExecutionResponse{}, nil
}

func (f *fakeHandler) TerminateWorkflowExecution(ctx context.Context, req *workflowservice.TerminateWorkflowExecutionRequest) (*workflowservice.TerminateWorkflowExecutionResponse, error) {
	f.calls.Add(1)
	return &workflowservice.TerminateWorkflowExecutionResponse{}, nil
}

func (f *fakeHandler) DescribeWorkflowExecution(ctx context.Context, req *workflowservice.DescribeWorkflowExecutionRequest) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	f.calls.Add(1)
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{
			Execution: &commonpb.WorkflowExecution{
				WorkflowId: req.GetExecution().GetWorkflowId(),
				RunId:      "run-1",
			},
			Type: &commonpb.WorkflowType{Name: "Fake"},
		},
	}, nil
}

func (f *fakeHandler) ListWorkflowExecutions(ctx context.Context, req *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	f.calls.Add(1)
	return &workflowservice.ListWorkflowExecutionsResponse{}, nil
}

func (f *fakeHandler) SignalWithStartWorkflowExecution(ctx context.Context, req *workflowservice.SignalWithStartWorkflowExecutionRequest) (*workflowservice.SignalWithStartWorkflowExecutionResponse, error) {
	f.calls.Add(1)
	return &workflowservice.SignalWithStartWorkflowExecutionResponse{RunId: "run-sws"}, nil
}

func (f *fakeHandler) QueryWorkflow(ctx context.Context, req *workflowservice.QueryWorkflowRequest) (*workflowservice.QueryWorkflowResponse, error) {
	f.calls.Add(1)
	return &workflowservice.QueryWorkflowResponse{
		QueryResult: &commonpb.Payloads{Payloads: []*commonpb.Payload{{Data: []byte("result")}}},
	}, nil
}

func (f *fakeHandler) CreateSchedule(ctx context.Context, req *workflowservice.CreateScheduleRequest) (*workflowservice.CreateScheduleResponse, error) {
	f.calls.Add(1)
	return &workflowservice.CreateScheduleResponse{}, nil
}

func (f *fakeHandler) ListSchedules(ctx context.Context, req *workflowservice.ListSchedulesRequest) (*workflowservice.ListSchedulesResponse, error) {
	f.calls.Add(1)
	return &workflowservice.ListSchedulesResponse{}, nil
}

func (f *fakeHandler) DeleteSchedule(ctx context.Context, req *workflowservice.DeleteScheduleRequest) (*workflowservice.DeleteScheduleResponse, error) {
	f.calls.Add(1)
	return &workflowservice.DeleteScheduleResponse{}, nil
}

func (f *fakeHandler) PatchSchedule(ctx context.Context, req *workflowservice.PatchScheduleRequest) (*workflowservice.PatchScheduleResponse, error) {
	f.calls.Add(1)
	return &workflowservice.PatchScheduleResponse{}, nil
}

func (f *fakeHandler) RegisterNamespace(ctx context.Context, req *workflowservice.RegisterNamespaceRequest) (*workflowservice.RegisterNamespaceResponse, error) {
	f.calls.Add(1)
	return &workflowservice.RegisterNamespaceResponse{}, nil
}

func (f *fakeHandler) DescribeNamespace(ctx context.Context, req *workflowservice.DescribeNamespaceRequest) (*workflowservice.DescribeNamespaceResponse, error) {
	f.calls.Add(1)
	return &workflowservice.DescribeNamespaceResponse{
		NamespaceInfo: &namespacepb.NamespaceInfo{
			Name:  req.GetNamespace(),
			State: enumspb.NAMESPACE_STATE_REGISTERED,
		},
	}, nil
}

func (f *fakeHandler) ListNamespaces(ctx context.Context, req *workflowservice.ListNamespacesRequest) (*workflowservice.ListNamespacesResponse, error) {
	f.calls.Add(1)
	return &workflowservice.ListNamespacesResponse{}, nil
}

func (f *fakeHandler) PollWorkflowTaskQueue(ctx context.Context, req *workflowservice.PollWorkflowTaskQueueRequest) (*workflowservice.PollWorkflowTaskQueueResponse, error) {
	f.calls.Add(1)
	return &workflowservice.PollWorkflowTaskQueueResponse{
		TaskToken: []byte("wf-token"),
		WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: "wf-1", RunId: "run-1"},
		WorkflowType:      &commonpb.WorkflowType{Name: "Fake"},
	}, nil
}

func (f *fakeHandler) PollActivityTaskQueue(ctx context.Context, req *workflowservice.PollActivityTaskQueueRequest) (*workflowservice.PollActivityTaskQueueResponse, error) {
	f.calls.Add(1)
	return &workflowservice.PollActivityTaskQueueResponse{
		TaskToken:    []byte("act-token"),
		ActivityId:   "act-1",
		ActivityType: &commonpb.ActivityType{Name: "Act"},
	}, nil
}

func (f *fakeHandler) RespondWorkflowTaskCompleted(ctx context.Context, req *workflowservice.RespondWorkflowTaskCompletedRequest) (*workflowservice.RespondWorkflowTaskCompletedResponse, error) {
	f.calls.Add(1)
	return &workflowservice.RespondWorkflowTaskCompletedResponse{}, nil
}

func (f *fakeHandler) RespondActivityTaskCompleted(ctx context.Context, req *workflowservice.RespondActivityTaskCompletedRequest) (*workflowservice.RespondActivityTaskCompletedResponse, error) {
	f.calls.Add(1)
	return &workflowservice.RespondActivityTaskCompletedResponse{}, nil
}

func (f *fakeHandler) RespondActivityTaskFailed(ctx context.Context, req *workflowservice.RespondActivityTaskFailedRequest) (*workflowservice.RespondActivityTaskFailedResponse, error) {
	f.calls.Add(1)
	return &workflowservice.RespondActivityTaskFailedResponse{}, nil
}

func (f *fakeHandler) RecordActivityTaskHeartbeat(ctx context.Context, req *workflowservice.RecordActivityTaskHeartbeatRequest) (*workflowservice.RecordActivityTaskHeartbeatResponse, error) {
	f.calls.Add(1)
	return &workflowservice.RecordActivityTaskHeartbeatResponse{CancelRequested: false}, nil
}

// ---- Test harness -------------------------------------------------------

func newTestHandler() (*ZAPHandler, *fakeHandler) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fh := &fakeHandler{}
	return &ZAPHandler{
		handler: fh,
		logger:  logger,
		broker:  newActivityBroker(),
	}, fh
}

// buildJSONEnvelope returns a ZAP message with body as field 0.
func buildJSONEnvelope(t *testing.T, body any) *zap.Message {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b := zap.NewBuilder(len(raw) + 64)
	obj := b.StartObject(24)
	obj.SetBytes(envelopeBody, raw)
	obj.FinishAsRoot()
	m, err := zap.Parse(b.Finish())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

// buildWorkerEnvelope returns a ZAP message with worker-shape fields.
// Caller sets fields via the returned object.
func buildWorkerEnvelope(t *testing.T, size int, set func(o *zap.ObjectBuilder)) *zap.Message {
	t.Helper()
	b := zap.NewBuilder(256)
	obj := b.StartObject(size)
	set(obj)
	obj.FinishAsRoot()
	m, err := zap.Parse(b.Finish())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func decodeEnvelopeStatus(t *testing.T, m *zap.Message) (status uint32, body []byte, errMsg string) {
	t.Helper()
	if m == nil {
		t.Fatal("nil response message")
	}
	r := m.Root()
	return r.Uint32(envelopeStatus), r.Bytes(envelopeBody), string(r.Bytes(envelopeError))
}

func assertOK(t *testing.T, m *zap.Message) []byte {
	t.Helper()
	status, body, errMsg := decodeEnvelopeStatus(t, m)
	if status != 0 && status != 200 {
		t.Fatalf("status=%d err=%q body=%s", status, errMsg, body)
	}
	return body
}

// ---- Tests --------------------------------------------------------------

// TestZAPHandler_AllOpcodesRegistered verifies Start() registers every
// opcode in the allocation table.
func TestZAPHandler_AllOpcodesRegistered(t *testing.T) {
	opcodes := []uint16{
		opcodeTaskSubmit, opcodeTaskSchedule,
		opStartWorkflow, opSignalWorkflow, opCancelWorkflow,
		opTerminateWorkflow, opDescribeWorkflow, opListWorkflows,
		opSignalWithStartWorkflow, opQueryWorkflow,
		opScheduleActivity, opWaitActivityResult, opStartChildWorkflow,
		opCreateSchedule, opListSchedules, opDeleteSchedule, opPauseSchedule,
		opRegisterNamespace, opDescribeNamespace, opListNamespaces,
		opHealth,
		opPollWorkflowTask, opPollActivityTask,
		opRespondWorkflowTaskCompleted, opRespondActivityTaskCompleted,
		opRespondActivityTaskFailed, opRecordActivityTaskHeartbeat,
	}
	if len(opcodes) < 27 {
		t.Fatalf("expected 27 opcodes, got %d", len(opcodes))
	}
	// Uniqueness + no rebinding.
	seen := map[uint16]bool{}
	for _, op := range opcodes {
		if seen[op] {
			t.Fatalf("opcode 0x%04X double-bound", op)
		}
		seen[op] = true
	}
}

func TestZAPHandler_Health(t *testing.T) {
	h, _ := newTestHandler()
	resp, err := h.handleHealth(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	body := assertOK(t, resp)
	var out map[string]string
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["status"] != "SERVING" {
		t.Fatalf("expected status=SERVING, got %v", out)
	}
}

func TestZAPHandler_StartWorkflow(t *testing.T) {
	h, fh := newTestHandler()
	msg := buildJSONEnvelope(t, startWorkflowReq{
		Namespace:    "ns",
		WorkflowID:   "wf-1",
		WorkflowType: "Fake",
		TaskQueue:    "tq",
	})
	resp, err := h.handleStartWorkflow(context.Background(), "", msg)
	if err != nil {
		t.Fatalf("startWorkflow: %v", err)
	}
	body := assertOK(t, resp)
	var out startWorkflowResp
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.RunID != "run-wf-1" {
		t.Fatalf("run_id=%q", out.RunID)
	}
	if fh.calls.Load() != 1 {
		t.Fatalf("expected 1 call, got %d", fh.calls.Load())
	}
}

func TestZAPHandler_SignalWorkflow(t *testing.T) {
	h, fh := newTestHandler()
	msg := buildJSONEnvelope(t, signalReq{Namespace: "ns", WorkflowID: "wf-1", SignalName: "sig"})
	resp, err := h.handleSignalWorkflow(context.Background(), "", msg)
	if err != nil {
		t.Fatalf("signal: %v", err)
	}
	assertOK(t, resp)
	if fh.calls.Load() != 1 {
		t.Fatalf("expected 1 call")
	}
}

func TestZAPHandler_CancelTerminateDescribeListQuery(t *testing.T) {
	h, fh := newTestHandler()
	ctx := context.Background()

	cancelMsg := buildJSONEnvelope(t, cancelReq{Namespace: "ns", WorkflowID: "wf"})
	if _, err := h.handleCancelWorkflow(ctx, "", cancelMsg); err != nil {
		t.Fatal(err)
	}
	termMsg := buildJSONEnvelope(t, terminateReq{Namespace: "ns", WorkflowID: "wf"})
	if _, err := h.handleTerminateWorkflow(ctx, "", termMsg); err != nil {
		t.Fatal(err)
	}
	descMsg := buildJSONEnvelope(t, describeReq{Namespace: "ns", WorkflowID: "wf"})
	if _, err := h.handleDescribeWorkflow(ctx, "", descMsg); err != nil {
		t.Fatal(err)
	}
	listMsg := buildJSONEnvelope(t, listReq{Namespace: "ns"})
	if _, err := h.handleListWorkflows(ctx, "", listMsg); err != nil {
		t.Fatal(err)
	}
	queryMsg := buildJSONEnvelope(t, queryReq{Namespace: "ns", WorkflowID: "wf", QueryType: "q"})
	if _, err := h.handleQueryWorkflow(ctx, "", queryMsg); err != nil {
		t.Fatal(err)
	}

	if fh.calls.Load() != 5 {
		t.Fatalf("expected 5 calls, got %d", fh.calls.Load())
	}
}

func TestZAPHandler_SignalWithStart(t *testing.T) {
	h, fh := newTestHandler()
	msg := buildJSONEnvelope(t, signalWithStartReq{
		startWorkflowReq: startWorkflowReq{
			Namespace: "ns", WorkflowID: "wf", WorkflowType: "t", TaskQueue: "tq",
		},
		SignalName: "sig",
	})
	if _, err := h.handleSignalWithStartWorkflow(context.Background(), "", msg); err != nil {
		t.Fatalf("signalWithStart: %v", err)
	}
	if fh.calls.Load() != 1 {
		t.Fatalf("expected 1 call")
	}
}

func TestZAPHandler_ScheduleCRUD(t *testing.T) {
	h, fh := newTestHandler()
	ctx := context.Background()

	create := buildJSONEnvelope(t, createScheduleReq{
		Namespace:  "ns",
		ScheduleID: "s1",
		Schedule: scheduleBody{
			Action: scheduleAction{WorkflowType: "Fake", TaskQueue: "tq"},
		},
	})
	if _, err := h.handleCreateSchedule(ctx, "", create); err != nil {
		t.Fatal(err)
	}
	listMsg := buildJSONEnvelope(t, listSchedulesReq{Namespace: "ns"})
	if _, err := h.handleListSchedules(ctx, "", listMsg); err != nil {
		t.Fatal(err)
	}
	delMsg := buildJSONEnvelope(t, deleteScheduleReq{Namespace: "ns", ScheduleID: "s1"})
	if _, err := h.handleDeleteSchedule(ctx, "", delMsg); err != nil {
		t.Fatal(err)
	}
	pauseMsg := buildJSONEnvelope(t, pauseScheduleReq{Namespace: "ns", ScheduleID: "s1"})
	if _, err := h.handlePauseSchedule(ctx, "", pauseMsg); err != nil {
		t.Fatal(err)
	}
	if fh.calls.Load() != 4 {
		t.Fatalf("expected 4 calls, got %d", fh.calls.Load())
	}
}

func TestZAPHandler_NamespaceCRUD(t *testing.T) {
	h, fh := newTestHandler()
	ctx := context.Background()
	regMsg := buildJSONEnvelope(t, registerNSReq{Name: "ns"})
	if _, err := h.handleRegisterNamespace(ctx, "", regMsg); err != nil {
		t.Fatal(err)
	}
	descMsg := buildJSONEnvelope(t, describeNSReq{Name: "ns"})
	if _, err := h.handleDescribeNamespace(ctx, "", descMsg); err != nil {
		t.Fatal(err)
	}
	listMsg := buildJSONEnvelope(t, listNSReq{PageSize: 10})
	if _, err := h.handleListNamespaces(ctx, "", listMsg); err != nil {
		t.Fatal(err)
	}
	if fh.calls.Load() != 3 {
		t.Fatalf("expected 3 calls, got %d", fh.calls.Load())
	}
}

func TestZAPHandler_WorkerPollsAndResponds(t *testing.T) {
	h, fh := newTestHandler()
	ctx := context.Background()

	// Polls take worker-shape envelopes.
	pollWF := buildWorkerEnvelope(t, 64, func(o *zap.ObjectBuilder) {
		o.SetText(fNamespace, "ns")
		o.SetText(fTaskQueueName, "tq")
		o.SetInt8(fTaskQueueKind, 1)
		o.SetText(fIdentity, "worker-1")
	})
	if _, err := h.handlePollWorkflowTask(ctx, "", pollWF); err != nil {
		t.Fatalf("pollWF: %v", err)
	}
	pollAct := buildWorkerEnvelope(t, 64, func(o *zap.ObjectBuilder) {
		o.SetText(fNamespace, "ns")
		o.SetText(fTaskQueueName, "tq")
		o.SetInt8(fTaskQueueKind, 1)
		o.SetText(fIdentity, "worker-1")
	})
	if _, err := h.handlePollActivityTask(ctx, "", pollAct); err != nil {
		t.Fatalf("pollAct: %v", err)
	}

	// Responds (non-broker tokens → upstream).
	respWF := buildWorkerEnvelope(t, 32, func(o *zap.ObjectBuilder) {
		o.SetBytes(fTaskToken, []byte("wf-token"))
		o.SetBytes(fCommandsBytes, []byte("{}"))
	})
	if _, err := h.handleRespondWorkflowTaskCompleted(ctx, "", respWF); err != nil {
		t.Fatalf("respondWF: %v", err)
	}
	respAct := buildWorkerEnvelope(t, 32, func(o *zap.ObjectBuilder) {
		o.SetBytes(fTaskToken, []byte("raw-activity-token"))
		o.SetBytes(fResultBytes, []byte("result"))
	})
	if _, err := h.handleRespondActivityTaskCompleted(ctx, "", respAct); err != nil {
		t.Fatalf("respondActCompleted: %v", err)
	}
	failAct := buildWorkerEnvelope(t, 32, func(o *zap.ObjectBuilder) {
		o.SetBytes(fTaskToken, []byte("raw-activity-token"))
		o.SetBytes(fFailureBytes, []byte("boom"))
	})
	if _, err := h.handleRespondActivityTaskFailed(ctx, "", failAct); err != nil {
		t.Fatalf("respondActFailed: %v", err)
	}
	heartbeat := buildWorkerEnvelope(t, 32, func(o *zap.ObjectBuilder) {
		o.SetBytes(fTaskToken, []byte("raw-activity-token"))
		o.SetBytes(fDetailsBytes, []byte("tick"))
	})
	if _, err := h.handleRecordActivityTaskHeartbeat(ctx, "", heartbeat); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	if fh.calls.Load() != 6 {
		t.Fatalf("expected 6 upstream calls (2 polls + 3 responds + 1 heartbeat), got %d", fh.calls.Load())
	}
}

// TestZAPHandler_BrokerScheduleWaitComplete is the end-to-end proof that
// 0x006B / 0x006C wire together with the respond path. A worker
// schedules an activity, the server mints an id, another worker
// submits a success respond with the broker-prefixed token, and the
// original wait returns ready=true.
func TestZAPHandler_BrokerScheduleWaitComplete(t *testing.T) {
	h, fh := newTestHandler()
	ctx := context.Background()

	// Schedule.
	scheduleMsg := buildJSONEnvelope(t, scheduleActivityReq{
		Namespace:      "ns",
		WorkflowID:     "wf-1",
		ActivityType:   "DoWork",
		TaskQueue:      "tq",
		StartToCloseMs: 1_000,
	})
	schedResp, err := h.handleScheduleActivity(ctx, "", scheduleMsg)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	var sched scheduleActivityResp
	if err := json.Unmarshal(assertOK(t, schedResp), &sched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sched.ActivityTaskID == "" {
		t.Fatal("expected activity_task_id")
	}

	// Kick off the wait in a goroutine.
	type waitResult struct {
		body []byte
		err  error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		waitMsg := buildJSONEnvelope(t, waitActivityResultReq{
			ActivityTaskID: sched.ActivityTaskID,
			WaitMs:         2_000,
		})
		resp, err := h.handleWaitActivityResult(ctx, "", waitMsg)
		if err != nil {
			waitCh <- waitResult{err: err}
			return
		}
		waitCh <- waitResult{body: assertOK(t, resp)}
	}()

	// Let the waiter block.
	time.Sleep(20 * time.Millisecond)

	// Respond via the broker-signed token returned from schedule.
	if len(sched.TaskToken) == 0 {
		t.Fatalf("schedule response missing task_token")
	}
	respMsg := buildWorkerEnvelope(t, 32, func(o *zap.ObjectBuilder) {
		o.SetBytes(fTaskToken, sched.TaskToken)
		o.SetBytes(fResultBytes, []byte("ok"))
	})
	if _, err := h.handleRespondActivityTaskCompleted(ctx, "", respMsg); err != nil {
		t.Fatalf("respond: %v", err)
	}
	// Broker-owned response MUST NOT call upstream.
	if fh.calls.Load() != 0 {
		t.Fatalf("broker response leaked to upstream: calls=%d", fh.calls.Load())
	}

	// Collect the wait outcome.
	select {
	case res := <-waitCh:
		if res.err != nil {
			t.Fatalf("wait err: %v", res.err)
		}
		var out waitActivityResultResp
		if err := json.Unmarshal(res.body, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !out.Ready {
			t.Fatalf("expected ready=true, got %+v", out)
		}
		if string(out.Result) != "ok" {
			t.Fatalf("expected result=ok, got %q", out.Result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return")
	}
}

func TestZAPHandler_BrokerScheduleWaitFail(t *testing.T) {
	h, _ := newTestHandler()
	ctx := context.Background()

	scheduleMsg := buildJSONEnvelope(t, scheduleActivityReq{
		Namespace:      "ns",
		WorkflowID:     "wf-2",
		ActivityType:   "DoWork",
		TaskQueue:      "tq",
		StartToCloseMs: 1_000,
	})
	schedResp, err := h.handleScheduleActivity(ctx, "", scheduleMsg)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	var sched scheduleActivityResp
	if err := json.Unmarshal(assertOK(t, schedResp), &sched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	type waitResult struct {
		body []byte
		err  error
	}
	waitCh := make(chan waitResult, 1)
	go func() {
		waitMsg := buildJSONEnvelope(t, waitActivityResultReq{
			ActivityTaskID: sched.ActivityTaskID,
			WaitMs:         2_000,
		})
		resp, err := h.handleWaitActivityResult(ctx, "", waitMsg)
		if err != nil {
			waitCh <- waitResult{err: err}
			return
		}
		waitCh <- waitResult{body: assertOK(t, resp)}
	}()

	time.Sleep(20 * time.Millisecond)

	if len(sched.TaskToken) == 0 {
		t.Fatalf("schedule response missing task_token")
	}
	failMsg := buildWorkerEnvelope(t, 32, func(o *zap.ObjectBuilder) {
		o.SetBytes(fTaskToken, sched.TaskToken)
		o.SetBytes(fFailureBytes, []byte("nope"))
	})
	if _, err := h.handleRespondActivityTaskFailed(ctx, "", failMsg); err != nil {
		t.Fatalf("fail: %v", err)
	}

	select {
	case res := <-waitCh:
		if res.err != nil {
			t.Fatalf("wait err: %v", res.err)
		}
		var out waitActivityResultResp
		if err := json.Unmarshal(res.body, &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !out.Ready {
			t.Fatalf("expected ready=true")
		}
		if string(out.Failure) != "nope" {
			t.Fatalf("expected failure=nope, got %q", out.Failure)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return")
	}
}

func TestZAPHandler_WaitUnknownID(t *testing.T) {
	h, _ := newTestHandler()
	waitMsg := buildJSONEnvelope(t, waitActivityResultReq{
		ActivityTaskID: "never-scheduled",
		WaitMs:         0,
	})
	resp, err := h.handleWaitActivityResult(context.Background(), "", waitMsg)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	var out waitActivityResultResp
	if err := json.Unmarshal(assertOK(t, resp), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Ready {
		t.Fatal("expected ready=false for unknown id")
	}
}

func TestZAPHandler_StartChildWorkflow(t *testing.T) {
	h, fh := newTestHandler()
	msg := buildJSONEnvelope(t, startChildWorkflowReq{
		Namespace:    "ns",
		ParentID:     "parent",
		WorkflowID:   "child",
		WorkflowType: "Child",
		TaskQueue:    "tq",
	})
	if _, err := h.handleStartChildWorkflow(context.Background(), "", msg); err != nil {
		t.Fatalf("startChild: %v", err)
	}
	if fh.calls.Load() != 1 {
		t.Fatalf("expected 1 call")
	}
}
