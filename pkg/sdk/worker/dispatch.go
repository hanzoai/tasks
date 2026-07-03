// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime/debug"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/activity"
	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	luxlog "github.com/luxfi/log"
)

// dispatchWorkflowTask replays the registered workflow function against the
// task's event-sourced history for one decision episode and ships the
// resulting commands back to the server.
//
//  1. Look up the fn by task.WorkflowTypeName.
//  2. Parse task.History into events; seed a replay env from them.
//  3. Decode the workflow input from WORKFLOW_EXECUTION_STARTED.
//  4. Run the fn: ExecuteActivity resolves from history by seq, or emits a
//     ScheduleActivity command and blocks the episode.
//  5. Batch the new commands (schedule* + terminal complete/fail) into a
//     CommandsEnvelope and RespondWorkflowTaskCompleted.
//
// The fn is re-run from the top on every task — it MUST be pure w.r.t. its
// inputs + history: same arguments and history ⇒ same command sequence.
func (w *workerImpl) dispatchWorkflowTask(ctx context.Context, task *client.WorkflowTask) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("workflow task panic",
				"workflow_type", task.WorkflowTypeName,
				"workflow_id", task.WorkflowID,
				"run_id", task.RunID,
				"recover", r,
				"stack", string(debug.Stack()),
			)
		}
	}()

	fn, ok := w.registry.workflowFn(task.WorkflowTypeName)
	if !ok {
		w.logger.Warn("no workflow registered",
			"workflow_type", task.WorkflowTypeName,
			"workflow_id", task.WorkflowID,
		)
		_ = w.transport.RespondWorkflowTaskCompleted(ctx,
			client.RespondWorkflowTaskCompletedRequest{
				TaskToken: task.TaskToken,
				Commands:  emptyCommandsJSON,
			})
		return
	}

	events, histErr := parseHistory(task.History)
	if histErr != nil {
		w.logger.Error("workflow history parse failed",
			"workflow_type", task.WorkflowTypeName, "err", histErr)
		w.respondWorkflow(ctx, task.TaskToken, []rawCommand{failCommand(histErr)})
		return
	}

	info := workflow.Info{
		WorkflowID:   task.WorkflowID,
		RunID:        task.RunID,
		WorkflowType: task.WorkflowTypeName,
		TaskQueue:    w.taskQueue,
		Namespace:    w.namespace,
		Attempt:      1,
	}
	env := newWorkerEnv(ctx, w.transport, info, w.taskQueue, w.logger)
	env.loadHistory(events)
	defer env.cancelAll()
	ctx2 := workflow.NewContextFromEnv(env)

	// Decode the workflow input (recorded on WORKFLOW_EXECUTION_STARTED)
	// into the fn's arg types. First argument is always workflow.Context.
	args, decodeErr := decodeWorkflowArgs(fn, ctx2, workflowInputFromHistory(events))
	if decodeErr != nil {
		w.logger.Error("workflow input decode failed",
			"workflow_type", task.WorkflowTypeName, "err", decodeErr)
		w.respondWorkflow(ctx, task.TaskToken, []rawCommand{failCommand(decodeErr)})
		return
	}

	result, runErr, blocked := runWorkflowEpisode(fn, args)
	commands := env.drainCommands() // ScheduleActivity commands emitted this episode
	switch {
	case blocked:
		// Episode incomplete: only the schedule commands. The run advances
		// when the server delivers the next task with the results appended.
	case runErr != nil:
		commands = append(commands, failCommand(runErr))
	default:
		commands = append(commands, completeCommand(result))
	}
	w.respondWorkflow(ctx, task.TaskToken, commands)
}

// respondWorkflow ships a command batch back for a workflow task.
func (w *workerImpl) respondWorkflow(ctx context.Context, token []byte, commands []rawCommand) {
	if err := w.transport.RespondWorkflowTaskCompleted(ctx,
		client.RespondWorkflowTaskCompletedRequest{
			TaskToken: token,
			Commands:  marshalCommands(commands),
		}); err != nil {
		w.logger.Error("respond workflow completed", "err", err)
	}
}

// marshalCommands wraps a command batch in the v1 CommandsEnvelope.
func marshalCommands(cmds []rawCommand) []byte {
	out, _ := json.Marshal(commandsEnvelope{Version: 1, Commands: cmds})
	return out
}

// failCommand encodes a FailWorkflow command (kind=1) carrying err.
func failCommand(err error) rawCommand {
	failureBytes, _ := temporal.Encode(err)
	return rawCommand{Kind: commandKindFailWorkflow, Failure: failureBytes}
}

// completeCommand encodes a CompleteWorkflow command (kind=0) carrying the
// fn's JSON-encoded return value (nil if it returned only an error).
func completeCommand(result any) rawCommand {
	var resultBytes []byte
	if result != nil {
		if enc, err := json.Marshal(result); err == nil {
			resultBytes = enc
		}
	}
	return rawCommand{Kind: commandKindCompleteWorkflow, Result: resultBytes}
}

// dispatchActivityTask runs the registered activity function for an
// ActivityTask. Flow:
//
//  1. Look up fn by task.ActivityTypeName.
//  2. Build activity.Scope + inject via activity.NewContext.
//  3. Start a background heartbeat ticker if the task carries a
//     HeartbeatTimeout.
//  4. Decode input JSON into fn arg types.
//  5. Invoke fn(ctx, args...).
//  6. Marshal result + Respond{Completed|Failed}.
func (w *workerImpl) dispatchActivityTask(ctx context.Context, task *client.ActivityTask) {
	defer func() {
		if r := recover(); r != nil {
			w.logger.Error("activity task panic",
				"activity_type", task.ActivityTypeName,
				"activity_id", task.ActivityID,
				"recover", r,
				"stack", string(debug.Stack()),
			)
			// Ship a failure so the server's retry loop gets a hit.
			failure := encodeFailure(temporal.NewError(
				fmt.Sprintf("activity panic: %v", r),
				"PanicError", true,
			))
			_ = w.transport.RespondActivityTaskFailed(ctx,
				client.RespondActivityTaskFailedRequest{
					TaskToken: task.TaskToken,
					Failure:   failure,
				})
		}
	}()

	fn, ok := w.registry.activityFn(task.ActivityTypeName)
	if !ok {
		w.logger.Warn("no activity registered",
			"activity_type", task.ActivityTypeName,
			"activity_id", task.ActivityID,
		)
		failure := encodeFailure(temporal.NewError(
			fmt.Sprintf("activity %q not registered", task.ActivityTypeName),
			"NotFoundError", true,
		))
		_ = w.transport.RespondActivityTaskFailed(ctx,
			client.RespondActivityTaskFailedRequest{
				TaskToken: task.TaskToken,
				Failure:   failure,
			})
		return
	}

	// Build the activity scope. The Heartbeater wires scope's
	// HeartbeatSink through the transport so activity code calling
	// activity.RecordHeartbeat hits the frontend.
	now := time.Now()
	scope := &activity.Scope{
		Info: activity.Info{
			TaskToken:         copyBytes(task.TaskToken),
			WorkflowExecution: activity.WorkflowExecution{WorkflowID: task.WorkflowID, RunID: task.RunID},
			ActivityID:        task.ActivityID,
			ActivityType:      task.ActivityTypeName,
			TaskQueue:         w.taskQueue,
			Attempt:           1,
			ScheduledTime:     time.UnixMilli(task.ScheduledTimeMs),
			StartedTime:       now,
		},
		Logger: bindActivityLogger(w.logger, task),
	}
	// Wire the heartbeat sink to the transport. Runs in the same
	// goroutine as the activity so the caller's ctx deadline applies.
	scope.HeartbeatSink = func(details ...any) {
		payload, _ := json.Marshal(details)
		if _, err := w.transport.RecordActivityTaskHeartbeat(ctx,
			client.RecordActivityTaskHeartbeatRequest{
				TaskToken: task.TaskToken,
				Details:   payload,
			}); err != nil {
			w.logger.Debug("heartbeat error",
				"activity_id", task.ActivityID, "err", err)
		}
	}

	actCtx := activity.NewContext(ctx, scope)

	// Start the auto-heartbeat goroutine if the server configured a
	// heartbeat timeout. We emit at half the timeout so one dropped
	// heartbeat doesn't immediately fail the task.
	stopHB := make(chan struct{})
	if task.HeartbeatTimeoutMs > 0 {
		interval := time.Duration(task.HeartbeatTimeoutMs) * time.Millisecond / 2
		if interval < 100*time.Millisecond {
			interval = 100 * time.Millisecond
		}
		go w.autoHeartbeat(ctx, task.TaskToken, interval, stopHB)
	}

	args, decodeErr := decodeActivityArgs(fn, actCtx, task.Input)
	if decodeErr != nil {
		close(stopHB)
		failure := encodeFailure(temporal.NewError(
			fmt.Sprintf("input decode: %v", decodeErr),
			"DecodeError", true,
		))
		_ = w.transport.RespondActivityTaskFailed(ctx,
			client.RespondActivityTaskFailedRequest{
				TaskToken: task.TaskToken,
				Failure:   failure,
			})
		return
	}

	result, err := invokeActivityFunc(fn, args)
	close(stopHB)

	if err != nil {
		failure := encodeFailure(err)
		if respErr := w.transport.RespondActivityTaskFailed(ctx,
			client.RespondActivityTaskFailedRequest{
				TaskToken: task.TaskToken,
				Failure:   failure,
			}); respErr != nil {
			w.logger.Error("respond activity failed",
				"activity_id", task.ActivityID, "err", respErr)
		}
		return
	}

	resultBytes, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		failure := encodeFailure(temporal.NewError(
			fmt.Sprintf("result marshal: %v", marshalErr),
			"MarshalError", true,
		))
		_ = w.transport.RespondActivityTaskFailed(ctx,
			client.RespondActivityTaskFailedRequest{
				TaskToken: task.TaskToken,
				Failure:   failure,
			})
		return
	}

	if respErr := w.transport.RespondActivityTaskCompleted(ctx,
		client.RespondActivityTaskCompletedRequest{
			TaskToken: task.TaskToken,
			Result:    resultBytes,
		}); respErr != nil {
		w.logger.Error("respond activity completed",
			"activity_id", task.ActivityID, "err", respErr)
	}
}

// autoHeartbeat emits a heartbeat every interval until stop closes or
// ctx is canceled. It is a best-effort liveness signal; user code can
// also emit heartbeats with its own details via activity.RecordHeartbeat.
func (w *workerImpl) autoHeartbeat(ctx context.Context, token []byte, interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if _, err := w.transport.RecordActivityTaskHeartbeat(ctx,
				client.RecordActivityTaskHeartbeatRequest{
					TaskToken: token,
					Details:   nil,
				}); err != nil {
				// Logged at debug — heartbeat failures are usually
				// transient (connection hiccup, frontend restart).
				w.logger.Debug("auto-heartbeat error", "err", err)
			}
		case <-ctx.Done():
			return
		case <-stop:
			return
		}
	}
}

// commandsEnvelope is the v1 JSON wire shape for the
// RespondWorkflowTaskCompletedRequest.Commands field. It mirrors
// the `CommandsEnvelope` / `Command` ZAP structs in schema/tasks.zap
// and is the canonical producer-side encoder for workflow commands.
//
// The frontend (service/frontend/workflow_handler.go,
// handleRespondWorkflowTaskCompleted) decodes this into history
// mutations (complete / fail / schedule activity). ZAP native serde
// replaces JSON in a follow-up without changing the shape.
type commandsEnvelope struct {
	Version  int8         `json:"v"`
	Commands []rawCommand `json:"cmds"`
}

type rawCommand struct {
	Kind    int8   `json:"kind"`
	Result  []byte `json:"result,omitempty"`  // kind=0 completeWorkflow
	Failure []byte `json:"failure,omitempty"` // kind=1 failWorkflow (temporal.Encode)

	// kind=2 scheduleActivity — the decider's durable activity spec. seq is
	// the deterministic per-run command index; the server writes
	// ACTIVITY_TASK_SCHEDULED{seq} and dispatches idempotently on (run, seq).
	Seq            int                     `json:"seq,omitempty"`
	ActivityType   string                  `json:"activityType,omitempty"`
	Input          []byte                  `json:"input,omitempty"`
	TaskQueue      string                  `json:"taskQueue,omitempty"`
	StartToCloseMs int64                   `json:"startToCloseMs,omitempty"`
	HeartbeatMs    int64                   `json:"heartbeatMs,omitempty"`
	RetryPolicy    *client.RetryPolicyJSON `json:"retryPolicy,omitempty"`
}

// Command kind constants mirror the Int8 values in
// schema/tasks.zap `Command.kind`. Anchored here so producer and
// (future) decoder agree; drift is caught at test time.
const (
	commandKindCompleteWorkflow int8 = 0
	commandKindFailWorkflow     int8 = 1
	commandKindScheduleActivity int8 = 2
)

// emptyCommandsJSON is the pre-serialised empty commands response
// used in the "no workflow registered" / decode-failure paths.
var emptyCommandsJSON = mustMarshal(commandsEnvelope{Version: 1, Commands: nil})

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("hanzo/tasks/worker: marshal: %v", err))
	}
	return b
}

// encodeFailure runs the temporal failure serialiser. Returns a
// DecodeError-encoded envelope on marshal failure so the server
// never sees a nil / empty Failure field.
func encodeFailure(err error) []byte {
	if err == nil {
		err = temporal.NewError("unknown failure", temporal.CodeApplication, false)
	}
	out, serr := temporal.Encode(err)
	if serr != nil {
		// Should be unreachable per temporal.Encode's contract.
		out, _ = temporal.Encode(temporal.NewError(
			"failure encode", temporal.CodeDecode, true,
		))
	}
	return out
}

// decodeWorkflowArgs decodes the workflow input (a JSON array of arguments
// recorded on WORKFLOW_EXECUTION_STARTED and extracted by
// workflowInputFromHistory) and prepends the workflow.Context.
func decodeWorkflowArgs(fn any, ctx workflow.Context, input []byte) ([]reflect.Value, error) {
	fv := reflect.ValueOf(fn)
	if fv.Kind() != reflect.Func {
		return nil, errors.New("registered workflow is not a function")
	}
	ft := fv.Type()
	if ft.NumIn() == 0 {
		return nil, errors.New("workflow must accept workflow.Context as its first argument")
	}
	// Validate first arg is workflow.Context.
	firstParam := ft.In(0)
	ctxVal := reflect.ValueOf(ctx)
	if !ctxVal.Type().AssignableTo(firstParam) {
		return nil, fmt.Errorf("workflow first arg is %s; expected workflow.Context", firstParam)
	}
	args := []reflect.Value{ctxVal}
	numExtraInputs := ft.NumIn() - 1
	if numExtraInputs == 0 {
		return args, nil
	}
	// Decode the rest of the arguments.
	return appendDecodedArgs(args, ft, input, 1)
}

// decodeActivityArgs decodes task.Input as a JSON array of activity
// arguments and prepends the context.Context (the activity ctx already
// carries the activity scope; we pass it as-is).
func decodeActivityArgs(fn any, ctx context.Context, input []byte) ([]reflect.Value, error) {
	fv := reflect.ValueOf(fn)
	if fv.Kind() != reflect.Func {
		return nil, errors.New("registered activity is not a function")
	}
	ft := fv.Type()
	if ft.NumIn() == 0 {
		// Tolerate zero-arg activities — rare but legal.
		return nil, nil
	}
	// First arg is context.Context.
	firstParam := ft.In(0)
	ctxVal := reflect.ValueOf(ctx)
	if !ctxVal.Type().AssignableTo(firstParam) {
		return nil, fmt.Errorf("activity first arg is %s; expected context.Context", firstParam)
	}
	args := []reflect.Value{ctxVal}
	numExtraInputs := ft.NumIn() - 1
	if numExtraInputs == 0 {
		return args, nil
	}
	return appendDecodedArgs(args, ft, input, 1)
}

// appendDecodedArgs unmarshals a JSON array into the remaining
// parameters of fn starting at skip. Missing array elements yield
// zero values so activities with default-safe arguments just work.
func appendDecodedArgs(args []reflect.Value, ft reflect.Type, input []byte, skip int) ([]reflect.Value, error) {
	n := ft.NumIn() - skip
	raw := make([]json.RawMessage, 0, n)
	if len(input) > 0 {
		if err := json.Unmarshal(input, &raw); err != nil {
			// Tolerate single-value inputs (JSON object / scalar,
			// wrapping the one arg). Try decoding input as a single
			// value.
			if n == 1 {
				raw = []json.RawMessage{input}
			} else {
				return nil, fmt.Errorf("unmarshal args array: %w", err)
			}
		}
	}
	for i := 0; i < n; i++ {
		pt := ft.In(skip + i)
		pv := reflect.New(pt)
		if i < len(raw) && len(raw[i]) > 0 {
			if err := json.Unmarshal(raw[i], pv.Interface()); err != nil {
				return nil, fmt.Errorf("unmarshal arg %d: %w", i, err)
			}
		}
		args = append(args, pv.Elem())
	}
	return args, nil
}

// invokeFunc calls the workflow function and splits its returns
// into (result, err). The error tail (a trailing `error` return)
// is propagated as the err; any other non-error return becomes
// the result. Workflows with no returns yield (nil, nil).
func invokeFunc(fn any, args []reflect.Value) (any, error) {
	fv := reflect.ValueOf(fn)
	out := fv.Call(args)
	var result any
	var err error
	for _, o := range out {
		if o.Kind() == reflect.Interface && o.Type().Implements(errType) {
			if !o.IsNil() {
				err = o.Interface().(error)
			}
			continue
		}
		if result == nil && o.IsValid() && o.CanInterface() {
			result = o.Interface()
		}
	}
	return result, err
}

// invokeActivityFunc calls the activity function. Returns (result, err)
// where result is the function's first non-error return (nil if none).
func invokeActivityFunc(fn any, args []reflect.Value) (any, error) {
	fv := reflect.ValueOf(fn)
	out := fv.Call(args)
	var result any
	var err error
	for _, o := range out {
		if o.Kind() == reflect.Interface && o.Type().Implements(errType) {
			if !o.IsNil() {
				err = o.Interface().(error)
			}
			continue
		}
		if result == nil && o.IsValid() && o.CanInterface() {
			result = o.Interface()
		}
	}
	return result, err
}

var errType = reflect.TypeOf((*error)(nil)).Elem()

// bindActivityLogger returns a logger scoped to the activity. It
// derives the worker logger via log.New(...) so that Noop loggers
// stay Noop and real loggers get the standard activity fields. This
// avoids the panic path in log.Noop().With().Str(...) on luxfi/log
// v1.4.1.
func bindActivityLogger(base luxlog.Logger, task *client.ActivityTask) luxlog.Logger {
	if base == nil {
		return luxlog.Noop()
	}
	return base.New(
		"activity_id", task.ActivityID,
		"activity_type", task.ActivityTypeName,
		"workflow_id", task.WorkflowID,
	)
}

// copyBytes returns an independent copy of b so mutations by the
// caller don't leak into the worker's stored token.
func copyBytes(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}
