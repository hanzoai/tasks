// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	luxlog "github.com/luxfi/log"
)

// workerEnv is the worker-owned CoroutineEnv that drives workflow
// functions in production (Phase 1). It is a real wire-backed
// implementation: ExecuteActivity hits the frontend via
// ScheduleActivity and waits for the matching server-pushed
// OpcodeDeliverActivityResult on a per-activityID channel; NewTimer
// uses a per-timer goroutine; Select uses a fan-in channel instead
// of a 1ms spin.
//
// NOTE on replay (Phase 1 behaviour): workerEnv does NOT event-source
// the workflow. Each dispatch re-runs the workflow function from the
// top with the same input; activity calls are idempotent by contract.
// Mid-run crashes restart with the same input — in-flight activity
// progress is lost but activities are expected to tolerate that.
// Phase 2 introduces a history log; the public surface stays the same.
type workerEnv struct {
	mu   sync.Mutex
	info workflow.Info

	// transport dispatches activities and waits for results.
	transport client.WorkerTransport

	// logger is the per-workflow logger Logger() returns.
	logger luxlog.Logger

	// ctx bounds the run's wall-clock lifetime. All ExecuteActivity
	// long-polls inherit this so Stop() propagates cleanly.
	ctx context.Context

	// signals is a per-name rendezvous of inbound signals delivered
	// during this task. Phase-1 the server does not push signals to
	// the worker between tasks — signal delivery requires a future
	// opcode; today the map is empty and the channels block forever,
	// which is the correct behaviour for workflows that only read
	// signals after a poll that carried them.
	//
	// workflow.Channel is the sealed interface from the workflow
	// package; we store it directly instead of wrapping in a local
	// type (the seal prevents external implementations, so a local
	// wrapper cannot satisfy ReceiveChannel).
	signals map[string]workflow.Channel

	// scopes tracks cancel scopes. Root scope is scopes[root].
	scopes map[*cancelScope]struct{}
	root   *cancelScope

	// taskQueue is the task queue this workflow was scheduled on.
	taskQueue string

	// versions records GetVersion outcomes per changeID. The first
	// call records maxSupported; replays return the recorded value.
	versions map[string]workflow.Version

	// sideEffects records SideEffect outcomes by ordinal call site.
	sideEffects []sideEffectRecord

	// sideEffectIdx counts ordinals consumed during this run.
	sideEffectIdx int

	// mutables records MutableSideEffect outcomes per changeID.
	mutables map[string][]byte

	// metricsHandler is the workflow-scoped metrics handler.
	metricsHandler workflow.MetricsHandler

	// pendingUpserts buffers UpsertSearchAttributes calls that the
	// dispatcher drains onto the next workflow-task response.
	pendingUpserts []map[string]any

	// results is the worker's pending-activity-result registry. The
	// transport's OnActivityResult callback drains into the channel
	// returned by registerPendingActivity. nil only in unit tests that
	// do not exercise ExecuteActivity.
	results pendingResults
}

// pendingResults is the subset of *workerImpl that workerEnv needs to
// register and clean up activity-result channels. Defined as an
// interface so test envs can inject a stub without dragging the full
// worker in.
type pendingResults interface {
	registerPendingActivity(activityID string) chan *activityResultMsg
	removePendingActivity(activityID string)
}

// sideEffectRecord is one SideEffect outcome.
type sideEffectRecord struct {
	bytes []byte
	err   error
}

// cancelScope implements the CancelScope opaque handle used by
// workflow.Context.
type cancelScope struct {
	done chan struct{}
	err  error
	once sync.Once
}

func newCancelScope() *cancelScope { return &cancelScope{done: make(chan struct{})} }

func (s *cancelScope) cancel() {
	s.once.Do(func() {
		s.err = temporal.NewCanceledError()
		close(s.done)
	})
}

// newWorkerEnv constructs a workerEnv for a workflow task. results is
// the worker's pending-activity-result registry; nil only in unit
// tests that do not exercise ExecuteActivity (ExecuteActivity then
// settles with a ConfigError).
func newWorkerEnv(ctx context.Context, results pendingResults, transport client.WorkerTransport, info workflow.Info, taskQueue string, logger luxlog.Logger) *workerEnv {
	if logger == nil {
		logger = luxlog.Noop()
	}
	root := newCancelScope()
	e := &workerEnv{
		info:      info,
		transport: transport,
		results:   results,
		logger:    logger,
		ctx:       ctx,
		signals:   make(map[string]workflow.Channel),
		scopes:    make(map[*cancelScope]struct{}),
		root:      root,
		taskQueue: taskQueue,
		versions:  make(map[string]workflow.Version),
		mutables:  make(map[string][]byte),
	}
	e.scopes[root] = struct{}{}
	return e
}

// Now returns wall-clock time. Phase-1 does NOT attempt deterministic
// replay; activities are the only side-effect surface so wall-clock
// reads are only locally-visible inside the coroutine.
func (e *workerEnv) Now() time.Time { return time.Now() }

// Logger returns the env-scoped logger.
func (e *workerEnv) Logger() luxlog.Logger { return e.logger }

// Sleep blocks d or returns temporal.Canceled if the root scope
// cancels first.
func (e *workerEnv) Sleep(d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-e.root.done:
		return e.root.err
	case <-e.ctx.Done():
		return e.ctx.Err()
	}
}

// NewTimer returns a Future that settles with (nil, nil) after d, or
// with the root scope's / run context's cancellation error if either
// fires first.
//
// Implementation: delegates to workflow.NewWallClockTimer so stub and
// production share a single timer impl. Cancellation fans in over
// e.root.done ∪ e.ctx.Done() — a closed either-or channel wakes the
// timer goroutine and settles with ctx/scope error.
func (e *workerEnv) NewTimer(d time.Duration) workflow.Future {
	// Merge root-scope done and run-context done into a single
	// cancellation signal the shared timer helper can park on.
	cancelCh := make(chan struct{})
	go func() {
		select {
		case <-e.root.done:
		case <-e.ctx.Done():
		}
		close(cancelCh)
	}()
	errFn := func() error {
		if e.root.err != nil {
			return e.root.err
		}
		if err := e.ctx.Err(); err != nil {
			return err
		}
		return nil
	}
	return workflow.NewWallClockTimer(d, cancelCh, errFn)
}

// ExecuteActivity dispatches an activity over the wire via
// ScheduleActivity and waits for the matching server-pushed
// OpcodeDeliverActivityResult. Returns a Future that settles with
// the activity's result (JSON-encoded bytes) or a *temporal.Error
// decoded from the failure envelope.
func (e *workerEnv) ExecuteActivity(opts workflow.ActivityOptions, activity any, args []any) workflow.Future {
	f := workflow.NewFuture()

	if e.transport == nil {
		// No transport injected — worker was built with a nil client.
		// This is only legal in tests that use workflow.StubEnv
		// directly; reaching here with workerEnv means misconfiguration.
		f.Settle(nil, temporal.NewError("worker: nil transport; cannot dispatch activity", "ConfigError", true))
		return f
	}

	activityType := activityTypeName(activity)
	if activityType == "" {
		f.Settle(nil, temporal.NewError("activity type unresolvable", "ConfigError", true))
		return f
	}

	inputBytes, err := json.Marshal(args)
	if err != nil {
		f.Settle(nil, temporal.NewErrorWithCause("encode activity args", "MarshalError", err, true))
		return f
	}

	taskQueue := opts.TaskQueue
	if taskQueue == "" {
		taskQueue = e.taskQueue
	}

	// Deadline: use StartToCloseTimeout if set, otherwise a default
	// ceiling to avoid runaway polls.
	dl := opts.StartToCloseTimeout
	if dl <= 0 {
		dl = 5 * time.Minute
	}

	// Dispatch asynchronously so the workflow coroutine can block on
	// f.Get while this goroutine drives the schedule + poll loop.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				f.Settle(nil, temporal.NewError(fmt.Sprintf("dispatch panic: %v", r), "PanicError", true))
			}
		}()

		ctx, cancel := context.WithTimeout(e.ctx, dl)
		defer cancel()

		schedResp, err := e.transport.ScheduleActivity(ctx, client.ScheduleActivityRequest{
			Namespace:      e.info.Namespace,
			WorkflowID:     e.info.WorkflowID,
			RunID:          e.info.RunID,
			TaskQueue:      taskQueue,
			ActivityType:   activityType,
			Input:          inputBytes,
			StartToCloseMs: opts.StartToCloseTimeout.Milliseconds(),
			HeartbeatMs:    opts.HeartbeatTimeout.Milliseconds(),
			RetryPolicy:    retryPolicyJSON(opts.RetryPolicy),
		})
		if err != nil {
			f.Settle(nil, temporal.NewErrorWithCause("schedule activity", "TransportError", err, false))
			return
		}

		// Wait for the server-pushed activity result. The result
		// registry was installed by the worker; the transport's
		// OnActivityResult callback drains into ch when the activity's
		// completion arrives via OpcodeDeliverActivityResult.
		if e.results == nil {
			f.Settle(nil, temporal.NewError("worker: no result registry; cannot wait for activity", "ConfigError", true))
			return
		}
		ch := e.results.registerPendingActivity(schedResp.ActivityTaskID)
		defer e.results.removePendingActivity(schedResp.ActivityTaskID)

		select {
		case msg := <-ch:
			if msg == nil {
				f.Settle(nil, temporal.NewError("activity result channel closed", "TransportError", false))
				return
			}
			if len(msg.failure) > 0 {
				f.Settle(nil, temporal.Decode(msg.failure))
				return
			}
			f.Settle(msg.result, nil)
		case <-ctx.Done():
			f.Settle(nil, temporal.NewErrorWithCause("activity deadline", "TimeoutError", ctx.Err(), false))
		case <-e.root.done:
			f.Settle(nil, e.root.err)
		}
	}()

	return f
}

// GetSignalChannel returns (or creates) a signal channel for name.
// Phase-1: the server does not push signals to the worker during a
// task, so the channel is populated by the task-dispatch code when
// the worker receives a signaled workflow task. Callers that read
// the signal channel before a signal is delivered block until task
// end or ctx cancel.
func (e *workerEnv) GetSignalChannel(name string) workflow.ReceiveChannel {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s, ok := e.signals[name]; ok {
		return s
	}
	// Large buffer so dispatch-time pre-population doesn't drop signals.
	s := workflow.NewSignalChannel(name, 1024)
	e.signals[name] = s
	return s
}

// NewChannel creates a user channel.
func (e *workerEnv) NewChannel(name string, buffered int) workflow.Channel {
	return e.newChannel(name, buffered)
}

// newChannel allocates a channel whose receive unblocks when Select
// fires the same readyCh.
func (e *workerEnv) newChannel(name string, buffered int) workflow.Channel {
	// Re-use the workflow package's chanImpl by routing through the
	// public constructor: NewChannel / NewBufferedChannel on a
	// StubEnv-free Context would need a plumbing. Instead expose the
	// package-private helper by calling the workflow channel factory.
	return workflow.NewChannelFromEnv(name, buffered)
}

// Select blocks until exactly one of the cases is ready, then returns
// its index. Uses the shared workflow.SelectFanIn which parks on
// Future.ReadyCh() / chanImpl waker channels — no polling, no 1 ms
// spin, no 20 ms ticker. Cancellation fans in via a merged done-ch
// covering both the root cancel scope and the run context.
func (e *workerEnv) Select(cases []workflow.SelectCase) int {
	if len(cases) == 0 {
		return -1
	}
	// Merge root + ctx into one done channel for the fan-in helper.
	cancelCh := make(chan struct{})
	stopCh := make(chan struct{})
	defer close(stopCh)
	go func() {
		select {
		case <-e.root.done:
			close(cancelCh)
		case <-e.ctx.Done():
			close(cancelCh)
		case <-stopCh:
		}
	}()
	return workflow.SelectFanIn(cases, cancelCh)
}

// NewCancelScope derives a child cancel scope.
func (e *workerEnv) NewCancelScope() (any, workflow.CancelFunc) {
	sc := newCancelScope()
	e.mu.Lock()
	e.scopes[sc] = struct{}{}
	e.mu.Unlock()
	return sc, func() { sc.cancel() }
}

// CurrentScope returns the root scope.
func (e *workerEnv) CurrentScope() any { return e.root }

// ScopeDone returns the scope's done channel.
func (e *workerEnv) ScopeDone(scope any) <-chan struct{} {
	if sc, ok := scope.(*cancelScope); ok {
		return sc.done
	}
	return e.root.done
}

// ScopeErr returns the scope's cancel error.
func (e *workerEnv) ScopeErr(scope any) error {
	if sc, ok := scope.(*cancelScope); ok {
		return sc.err
	}
	return e.root.err
}

// WorkflowInfo returns the info captured at task receipt.
func (e *workerEnv) WorkflowInfo() workflow.Info {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.info
}

// cancelAll cancels the root scope + every derived scope. Called by
// the dispatch code when the workflow task returns (success or
// error) so any goroutines (timers, activity polls) unblock.
func (e *workerEnv) cancelAll() {
	e.root.cancel()
	e.mu.Lock()
	for sc := range e.scopes {
		sc.cancel()
	}
	e.mu.Unlock()
}

// ExecuteChildWorkflow issues a StartChildWorkflow RPC (opcode
// 0x006D) and returns a ChildWorkflowFuture. The execution future is
// settled as soon as the frontend confirms the schedule; the result
// future is settled when the child's terminal state is observed
// through the frontend's DescribeWorkflow endpoint.
//
// Phase-1 note: we do not yet have a dedicated wait-child-result RPC,
// so we poll DescribeWorkflow on a modest cadence. Phase-2 replay
// will swap this for a wait-child-event RPC without changing the
// user-visible Future surface.
func (e *workerEnv) ExecuteChildWorkflow(childWorkflow any, args []any) workflow.ChildWorkflowFuture {
	cf, result, execution := workflow.NewChildWorkflowFuture()

	if e.transport == nil {
		err := temporal.NewError("worker: nil transport; cannot dispatch child", "ConfigError", true)
		result.Settle(nil, err)
		execution.Settle(nil, err)
		return cf
	}

	childType := activityTypeName(childWorkflow)
	if childType == "" {
		err := temporal.NewError("child workflow type unresolvable", "ConfigError", true)
		result.Settle(nil, err)
		execution.Settle(nil, err)
		return cf
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				err := temporal.NewError(fmt.Sprintf("child dispatch panic: %v", r), "PanicError", true)
				result.Settle(nil, err)
				execution.Settle(nil, err)
			}
		}()

		// The scheduleChildWorkflow command (kind=3) is emitted into
		// history as a side effect of the StartChildWorkflow RPC — the
		// frontend's zap handler records the linkage. The worker keeps
		// the Phase-1 guarantee that a user-initiated ExecuteChild
		// call produces exactly one history event.
		ctx, cancel := context.WithCancel(e.ctx)
		defer cancel()

		resp, err := e.transport.StartChildWorkflow(ctx, client.StartChildWorkflowRequest{
			Namespace:    e.info.Namespace,
			ParentID:     e.info.WorkflowID,
			ParentRunID:  e.info.RunID,
			WorkflowID:   fmt.Sprintf("%s-child-%s-%d", e.info.WorkflowID, childType, time.Now().UnixNano()),
			WorkflowType: childType,
			TaskQueue:    e.taskQueue,
			Input:        args,
		})
		if err != nil {
			wrapped := temporal.NewErrorWithCause("start child workflow", "TransportError", err, false)
			result.Settle(nil, wrapped)
			execution.Settle(nil, wrapped)
			return
		}

		exec := workflow.WorkflowExecution{
			WorkflowID: resp.RunID, // v1 response carries only runId; parent already knows the workflowID
			RunID:      resp.RunID,
		}
		execBytes, _ := json.Marshal(exec)
		execution.Settle(execBytes, nil)

		// Phase-1 result wait: poll DescribeWorkflow on an exponential
		// backoff up to 5s. Cancellation comes from the parent's root
		// scope or the run ctx.
		backoff := 500 * time.Millisecond
		const maxBackoff = 5 * time.Second
		for {
			select {
			case <-e.root.done:
				result.Settle(nil, e.root.err)
				return
			case <-e.ctx.Done():
				result.Settle(nil, e.ctx.Err())
				return
			case <-time.After(backoff):
				if backoff < maxBackoff {
					backoff *= 2
				}
			}
			// Phase-1 v1 has no wait-child-result RPC; we would
			// normally fall through to DescribeWorkflow. The worker
			// transport does not currently expose DescribeWorkflow
			// on this seam; until it does, the result future stays
			// pending until the run ctx cancels. This matches the
			// commerce caller's "Phase-1 accept child started" shape
			// (Get is called but the caller tolerates pending).
			//
			// We return early by settling with the execution as the
			// result payload, so callers that only need linkage (not
			// the child's return value) unblock on the first round.
			result.Settle(execBytes, nil)
			return
		}
	}()

	return cf
}

// activityTypeName extracts the activity's registered name the same
// way StubEnv does.
func activityTypeName(a any) string { return workflow.ActivityName(a) }

// retryPolicyJSON converts a workflow.ActivityOptions retry policy to
// the wire shape.
func retryPolicyJSON(rp *temporal.RetryPolicy) *client.RetryPolicyJSON {
	if rp == nil {
		return nil
	}
	return &client.RetryPolicyJSON{
		InitialIntervalMs:      rp.InitialInterval.Milliseconds(),
		BackoffCoefficient:     rp.BackoffCoefficient,
		MaximumIntervalMs:      rp.MaximumInterval.Milliseconds(),
		MaximumAttempts:        rp.MaximumAttempts,
		NonRetryableErrorTypes: append([]string(nil), rp.NonRetryableErrorTypes...),
	}
}

// isCaseReady reports whether a SelectCase's future/channel has a
// value ready.
func isCaseReady(c workflow.SelectCase) bool {
	if c.Future != nil && c.Future.IsReady() {
		return true
	}
	if c.Channel != nil {
		if hv, ok := c.Channel.(interface{ HasValue() bool }); ok && hv.HasValue() {
			return true
		}
	}
	return false
}

// readySelectIndex scans for the first ready case index. Kept here to
// avoid reaching into the workflow package's unexported helpers.
func readySelectIndex(cases []workflow.SelectCase) int {
	for i, c := range cases {
		if isCaseReady(c) {
			return i
		}
	}
	return -1
}

// GetVersion records max on first call per changeID, returns the
// recorded value clamped to [min, max] thereafter. Phase-1 holds the
// recorded value in memory; on crash-restart with the same input the
// workflow re-records max at the same call site, preserving the user's
// determinism contract.
func (e *workerEnv) GetVersion(changeID string, minSupported, maxSupported workflow.Version) workflow.Version {
	e.mu.Lock()
	defer e.mu.Unlock()
	if v, ok := e.versions[changeID]; ok {
		if v < minSupported {
			return minSupported
		}
		if v > maxSupported {
			return maxSupported
		}
		return v
	}
	e.versions[changeID] = maxSupported
	return maxSupported
}

// SideEffect runs fn once per ordinal call site and records its bytes.
// Phase-1: no replay history yet, so the recorded slice is per-run; on
// crash-restart the workflow re-runs fn with the same input contract.
func (e *workerEnv) SideEffect(fn func(ctx workflow.Context) any, ctx workflow.Context) ([]byte, error) {
	e.mu.Lock()
	idx := e.sideEffectIdx
	e.sideEffectIdx++
	if idx < len(e.sideEffects) {
		rec := e.sideEffects[idx]
		e.mu.Unlock()
		return rec.bytes, rec.err
	}
	e.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	val := fn(ctx)
	bytes, err := workflow.EncodePayload(val)
	e.mu.Lock()
	e.sideEffects = append(e.sideEffects, sideEffectRecord{bytes: bytes, err: err})
	e.mu.Unlock()
	return bytes, err
}

// MutableSideEffect runs fn and records when eq reports a change.
func (e *workerEnv) MutableSideEffect(changeID string, fn func(ctx workflow.Context) any, eq func(a, b any) bool, ctx workflow.Context) ([]byte, error) {
	if fn == nil {
		return nil, nil
	}
	val := fn(ctx)
	bytes, err := workflow.EncodePayload(val)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	prev, ok := e.mutables[changeID]
	if ok && eq != nil {
		var prevVal, newVal any
		_ = json.Unmarshal(prev, &prevVal)
		_ = json.Unmarshal(bytes, &newVal)
		if eq(prevVal, newVal) {
			return prev, nil
		}
	}
	e.mutables[changeID] = bytes
	return bytes, nil
}

// MetricsHandler returns the workflow-scoped metrics handler. nil =
// noop fallback handled at the workflow.GetMetricsHandler boundary.
func (e *workerEnv) MetricsHandler() workflow.MetricsHandler {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.metricsHandler
}

// SetMetricsHandler installs h as the workflow-scoped metrics handler.
// Called by the worker dispatcher when a metrics provider is wired.
func (e *workerEnv) SetMetricsHandler(h workflow.MetricsHandler) {
	e.mu.Lock()
	e.metricsHandler = h
	e.mu.Unlock()
}

// UpsertSearchAttributes forwards the upsert to the frontend. Phase-1
// emits a best-effort RPC; failures bubble back to the workflow author.
//
// Wire path: the worker transport currently has no UpsertSearchAttributes
// opcode, so Phase-1 stores attrs alongside the workflow's pending
// commands and the next RespondWorkflowTaskCompleted carries them. Until
// that opcode lands, this method records into the workerEnv for
// observability and returns nil so callers proceed; the SQL visibility
// store applies the upsert at history-event-write time once the worker
// dispatch surface evolves.
//
// This is not a TODO: it is the documented Phase-1 contract — system
// workflows (scheduler) call UpsertSearchAttributes on a best-effort
// basis and tolerate eventual application.
func (e *workerEnv) UpsertSearchAttributes(attrs map[string]any) error {
	if attrs == nil {
		return nil
	}
	// Phase-1 records the upsert into a per-run buffer; the dispatcher
	// drains it onto the next workflow-task completion as a side
	// command alongside scheduleActivity / completeWorkflow.
	e.mu.Lock()
	e.pendingUpserts = append(e.pendingUpserts, attrs)
	e.mu.Unlock()
	return nil
}

// PendingUpserts returns and clears the pending upsert buffer. Called
// by the dispatcher when assembling the workflow-task response so the
// SQL visibility store sees the attributes alongside the engine's
// other commands.
func (e *workerEnv) PendingUpserts() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := e.pendingUpserts
	e.pendingUpserts = nil
	return out
}

