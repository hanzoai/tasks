package workflow

import (
	"errors"
	"time"

	"github.com/luxfi/log"
)

// Context is the workflow-scoped counterpart to the stdlib
// context.Context. It carries the CoroutineEnv, a cancel scope, a
// deadline, per-call ActivityOptions, and user-supplied Values.
//
// Like context.Context, Context is immutable: helpers (WithCancel,
// WithActivityOptions, WithValue) return a derived Context with one
// field replaced. The parent is unchanged.
//
// Inside a workflow function treat Context exactly as you would
// treat context.Context in a regular request handler — but remember:
//
//   - ctx.Done() closes when the workflow is canceled (not when the
//     goroutine ends).
//   - ctx.Err() returns temporal.Canceled once canceled, not
//     context.Canceled.
//   - ctx.Value(k) carries workflow-side Values, not stdlib context
//     values — and may not cross the activity boundary.
type Context interface {
	// Deadline returns the time after which the workflow will be
	// canceled, or zero time + false if no deadline is set.
	Deadline() (time.Time, bool)

	// Done returns a channel that is closed when the workflow (or
	// the containing cancel scope) is canceled. Mirrors
	// context.Context.Done.
	Done() <-chan struct{}

	// Err reports why Done was closed. Returns nil while still
	// running, a *temporal.Error once canceled / timed out.
	Err() error

	// Value returns the value associated with key k, or nil.
	Value(k any) any

	// env exposes the internal CoroutineEnv to sibling helpers in
	// this package. Not part of the public surface — users who
	// never type-assert this out of the interface never need to
	// know it exists.
	env() CoroutineEnv

	// activityOptions returns the currently attached ActivityOptions,
	// or the zero value.
	activityOptions() ActivityOptions

	// scope returns the current CancelScope (opaque to callers).
	scope() any
}

// ctxImpl is the default workflow.Context implementation. A chain
// of these models the parent/child relationship:
//
//	root (carries env) → withCancel → withActivityOptions → withValue
//
// Each derived ctx holds a pointer to its parent plus one override.
// Reads walk up the chain; writes create a new link.
type ctxImpl struct {
	parent Context
	e      CoroutineEnv // set only on the root
	sc     any          // cancel scope override
	opts   *ActivityOptions
	key    any
	val    any
}

// NewContextFromEnv constructs the root workflow.Context for a
// workflow execution. pkg/sdk/worker calls this once per run,
// passing the CoroutineEnv it owns; it then invokes the registered
// workflow function with the returned Context.
//
// This is the ONLY way a Context enters the system. User code never
// calls NewContextFromEnv directly.
func NewContextFromEnv(e CoroutineEnv) Context {
	if e == nil {
		e = stubEnv{} // defensive: never return a nil-env ctx
	}
	return &ctxImpl{e: e}
}

func (c *ctxImpl) Deadline() (time.Time, bool) {
	// Phase 1 does not surface a scope deadline; callers with a
	// time budget express it via WithActivityOptions or explicit
	// Timer + Selector. This keeps the worker side simpler.
	return time.Time{}, false
}

func (c *ctxImpl) Done() <-chan struct{} {
	return c.env().ScopeDone(c.scope())
}

func (c *ctxImpl) Err() error {
	return c.env().ScopeErr(c.scope())
}

func (c *ctxImpl) Value(k any) any {
	if c.key != nil && c.key == k {
		return c.val
	}
	if c.parent != nil {
		return c.parent.Value(k)
	}
	return nil
}

// env returns the root env, walking up the parent chain. Linear,
// which is fine: workflow ctx chains in practice are a handful deep.
func (c *ctxImpl) env() CoroutineEnv {
	if c.e != nil {
		return c.e
	}
	if c.parent != nil {
		return c.parent.env()
	}
	return stubEnv{}
}

func (c *ctxImpl) activityOptions() ActivityOptions {
	if c.opts != nil {
		return *c.opts
	}
	if c.parent != nil {
		return c.parent.activityOptions()
	}
	return ActivityOptions{}
}

func (c *ctxImpl) scope() any {
	if c.sc != nil {
		return c.sc
	}
	if c.parent != nil {
		return c.parent.scope()
	}
	return c.env().CurrentScope()
}

// WithCancel derives a child Context whose cancel function, when
// invoked, cancels operations scoped to the child (timers created
// under it, activities scheduled under it, Selector.Select running
// under it). Canceling the child does not cancel the parent.
//
// Mirrors go.temporal.io/sdk/workflow.WithCancel semantics so
// migration is a pure path swap.
func WithCancel(parent Context) (Context, CancelFunc) {
	sc, cancel := parent.env().NewCancelScope()
	return &ctxImpl{parent: parent, sc: sc}, cancel
}

// WithValue returns a Context carrying key → val. Follows the same
// rules as context.WithValue: use a custom key type to avoid
// collisions, and don't abuse values for parameters.
func WithValue(parent Context, key, val any) Context {
	if key == nil {
		panic("workflow.WithValue: nil key")
	}
	return &ctxImpl{parent: parent, key: key, val: val}
}

// GetLogger returns a logger bound to the current workflow. The
// worker wires it to suppress duplicate lines during replay.
func GetLogger(ctx Context) log.Logger {
	if ctx == nil {
		return log.Noop()
	}
	l := ctx.env().Logger()
	if l == nil {
		return log.Noop()
	}
	return l
}

// Now returns the deterministic workflow clock. time.Now() is NOT
// deterministic and MUST NOT be called inside a workflow function;
// use this helper instead. Activities, by contrast, run off-ctx
// and can use time.Now freely.
func Now(ctx Context) time.Time {
	if ctx == nil {
		return time.Time{}
	}
	return ctx.env().Now()
}

// Sleep blocks the workflow coroutine for d of workflow time. Returns
// temporal.Canceled if ctx is canceled before d elapses.
//
// Sleep is the ONE-AND-ONLY way to delay execution inside a workflow;
// time.Sleep inside a workflow is both wrong (non-deterministic) and
// a scheduler bug (it blocks the coroutine thread).
func Sleep(ctx Context, d time.Duration) error {
	if ctx == nil {
		return errors.New("workflow.Sleep: nil context")
	}
	return ctx.env().Sleep(d)
}

// GetInfo returns a snapshot of stable identity fields for the
// running workflow (ID, RunID, TaskQueue, …). Safe to log.
func GetInfo(ctx Context) Info {
	if ctx == nil {
		return Info{}
	}
	return ctx.env().WorkflowInfo()
}
