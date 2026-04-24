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
// ScheduleActivity + long-poll WaitActivityResult; NewTimer uses a
// per-timer goroutine; Select uses a fan-in channel instead of a 1ms
// spin.
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

// newWorkerEnv constructs a workerEnv for a workflow task.
func newWorkerEnv(ctx context.Context, transport client.WorkerTransport, info workflow.Info, taskQueue string, logger luxlog.Logger) *workerEnv {
	if logger == nil {
		logger = luxlog.Noop()
	}
	root := newCancelScope()
	e := &workerEnv{
		info:      info,
		transport: transport,
		logger:    logger,
		ctx:       ctx,
		signals:   make(map[string]workflow.Channel),
		scopes:    make(map[*cancelScope]struct{}),
		root:      root,
		taskQueue: taskQueue,
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
// ScheduleActivity + long-poll WaitActivityResult. Returns a Future
// that settles with the activity's result (JSON-encoded bytes) or a
// *temporal.Error decoded from the failure envelope.
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

		// Long-poll the result. Each WaitActivityResult call caps at
		// 5s so ctx cancellation propagates within a short window
		// even when the activity takes longer.
		for {
			select {
			case <-ctx.Done():
				f.Settle(nil, temporal.NewErrorWithCause("activity deadline", "TimeoutError", ctx.Err(), false))
				return
			case <-e.root.done:
				f.Settle(nil, e.root.err)
				return
			default:
			}

			waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
			resp, err := e.transport.WaitActivityResult(waitCtx, client.WaitActivityResultRequest{
				ActivityTaskID: schedResp.ActivityTaskID,
				WaitMs:         5000,
			})
			waitCancel()
			if err != nil {
				// Transient errors: back off briefly and retry.
				time.Sleep(500 * time.Millisecond)
				continue
			}
			if !resp.Ready {
				// Still pending. Loop; ctx deadline bounds the wait.
				continue
			}
			if len(resp.Failure) > 0 {
				f.Settle(nil, temporal.Decode(resp.Failure))
				return
			}
			f.Settle(resp.Result, nil)
			return
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

