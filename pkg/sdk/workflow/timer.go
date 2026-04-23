package workflow

import "time"

// NewTimer schedules a durable timer that fires after d of workflow
// time and returns the Future that represents its completion. The
// Future's Get settles with nil error when the timer fires, or with
// temporal.Canceled if the ctx is canceled before d elapses.
//
// Unlike time.AfterFunc, the timer is durable: the worker persists
// it and fires it at the right workflow time even across restarts
// (Phase 2; Phase 1 restarts the workflow from the beginning).
//
// Mirrors go.temporal.io/sdk/workflow.NewTimer.
func NewTimer(ctx Context, d time.Duration) Future {
	if ctx == nil {
		// Return an already-failed future so callers don't crash.
		f := NewFuture()
		f.Settle(nil, errNilContext)
		return f
	}
	if d < 0 {
		d = 0
	}
	return ctx.env().NewTimer(d)
}
