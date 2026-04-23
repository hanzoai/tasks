package workflow

import (
	"errors"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// ActivityOptions configures a single activity invocation. Attached
// to a Context via WithActivityOptions; every ExecuteActivity call
// below reads the attached options at dispatch time.
//
// Shape matches go.temporal.io/sdk/workflow.ActivityOptions so
// existing base/commerce call sites compile unchanged after the
// import swap.
type ActivityOptions struct {
	// TaskQueue routes the activity to a specific worker pool.
	// Empty means "the workflow's own task queue" (the common case).
	TaskQueue string

	// ScheduleToStartTimeout caps the wait between enqueue and
	// worker pick-up. 0 = unlimited.
	ScheduleToStartTimeout time.Duration

	// StartToCloseTimeout caps a single activity run. Exceeding it
	// forces a retry per RetryPolicy. 0 = unlimited (NOT
	// recommended; configure something reasonable).
	StartToCloseTimeout time.Duration

	// ScheduleToCloseTimeout is the end-to-end budget including
	// retries. 0 means "derive from StartToCloseTimeout *
	// MaximumAttempts".
	ScheduleToCloseTimeout time.Duration

	// HeartbeatTimeout is the max interval between heartbeats for
	// a long-running activity. Exceeding it aborts the attempt
	// and triggers retry. 0 = no heartbeat required.
	HeartbeatTimeout time.Duration

	// RetryPolicy controls attempt count and backoff. nil = the
	// SDK default (unlimited attempts, exponential backoff starting
	// at 1s, capped at 100s). Pass &temporal.RetryPolicy{MaximumAttempts: 1}
	// to disable retries entirely.
	RetryPolicy *temporal.RetryPolicy

	// WaitForCancellation, when true, makes cancellation wait for
	// the activity to acknowledge before returning. Default false:
	// cancellation unblocks the caller immediately and the
	// activity is expected to observe ctx.Done and exit.
	WaitForCancellation bool
}

// WithActivityOptions returns a Context carrying opts. The ctx
// returned is what ExecuteActivity(ctx, …) consumes when it
// dispatches the activity.
func WithActivityOptions(parent Context, opts ActivityOptions) Context {
	if parent == nil {
		panic("workflow.WithActivityOptions: nil context")
	}
	cp := opts // copy so later caller mutation doesn't leak
	return &ctxImpl{parent: parent, opts: &cp}
}

// ExecuteActivity dispatches an activity for execution and returns
// the Future that settles with its (result, error). activity may
// be a function value or a registered name (string). args are the
// activity's input parameters; they will be JSON-encoded for the
// wire.
//
// The activity's attached ActivityOptions are read from ctx. If no
// options were attached, the SDK default is used (unlimited retry,
// no timeouts — configure something).
//
// Callers MUST NOT spawn goroutines that call ExecuteActivity; in
// Phase 1 the workflow is a single coroutine and parallel dispatch
// is done by calling ExecuteActivity N times in a loop and then
// Get-ing the Futures (or selecting on them).
func ExecuteActivity(ctx Context, activity any, args ...any) Future {
	if ctx == nil {
		f := NewFuture()
		f.Settle(nil, errNilContext)
		return f
	}
	if activity == nil {
		f := NewFuture()
		f.Settle(nil, temporal.NewError("nil activity", temporal.CodeApplication, true))
		return f
	}
	opts := ctx.activityOptions()
	return ctx.env().ExecuteActivity(opts, activity, args)
}

// errNilContext is returned when user code passes a nil Context
// into a helper. We surface it as a non-retryable Error so retry
// policies don't try to recover from a programming bug.
var errNilContext = &temporal.Error{
	Message:      "workflow.Context is nil",
	Code:         temporal.CodeApplication,
	NonRetryable: true,
	Cause:        errors.New("nil context"),
}
