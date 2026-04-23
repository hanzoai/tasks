// Copyright © 2026 Hanzo AI. MIT License.

package workflow

import (
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// LocalActivityOptions configures a single local-activity invocation.
// Mirrors go.temporal.io/sdk/workflow.LocalActivityOptions.
//
// Phase-1 note: local activities execute via the remote path. Same
// wire, same guarantees. True in-process fast path arrives with the
// event-sourced replay engine in Phase 2. The options struct is
// preserved so caller code compiles unchanged and so the retry /
// timeout fields are propagated to the remote-path activity.
type LocalActivityOptions struct {
	// ScheduleToCloseTimeout is the end-to-end budget including
	// retries. 0 = derive from StartToCloseTimeout * MaximumAttempts.
	ScheduleToCloseTimeout time.Duration

	// StartToCloseTimeout caps a single attempt. 0 = unlimited.
	StartToCloseTimeout time.Duration

	// RetryPolicy overrides the default retry policy. Nil = SDK
	// default (see temporal.DefaultRetryPolicy).
	RetryPolicy *temporal.RetryPolicy
}

// localActivityKey is the unexported Context key used by
// WithLocalActivityOptions. Using a private type prevents cross-
// package key collisions.
type localActivityKey struct{}

// WithLocalActivityOptions returns a Context whose ExecuteLocalActivity
// consumes opts. Independent of WithActivityOptions — the two key
// spaces do not collide.
func WithLocalActivityOptions(parent Context, opts LocalActivityOptions) Context {
	if parent == nil {
		panic("workflow.WithLocalActivityOptions: nil context")
	}
	cp := opts // copy so later caller mutation doesn't leak
	return WithValue(parent, localActivityKey{}, &cp)
}

// localActivityOptionsFromCtx returns the attached options, or zero.
func localActivityOptionsFromCtx(ctx Context) LocalActivityOptions {
	if ctx == nil {
		return LocalActivityOptions{}
	}
	if v, ok := ctx.Value(localActivityKey{}).(*LocalActivityOptions); ok && v != nil {
		return *v
	}
	return LocalActivityOptions{}
}

// ExecuteLocalActivity dispatches an activity via the same wire as
// ExecuteActivity. Phase 1: local activities execute via the remote
// path. Same wire, same guarantees. True in-process fast path
// arrives with the event-sourced replay engine in Phase 2.
//
// The options attached to ctx via WithLocalActivityOptions are
// translated to the ActivityOptions consumed by the remote path so
// the StartToCloseTimeout and RetryPolicy flow through unchanged.
func ExecuteLocalActivity(ctx Context, activity any, args ...any) Future {
	if ctx == nil {
		f := NewFuture()
		f.Settle(nil, errNilContext)
		return f
	}
	opts := localActivityOptionsFromCtx(ctx)
	remote := ActivityOptions{
		ScheduleToCloseTimeout: opts.ScheduleToCloseTimeout,
		StartToCloseTimeout:    opts.StartToCloseTimeout,
		RetryPolicy:            opts.RetryPolicy,
	}
	// Phase-1 remote path: thread ExecuteActivity through the env
	// directly so the per-call ActivityOptions are honoured even when
	// the caller did not attach WithActivityOptions.
	return ctx.env().ExecuteActivity(remote, activity, args)
}
