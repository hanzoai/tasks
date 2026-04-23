// Package activity provides the call-site helpers that activity code uses
// inside a Hanzo Tasks worker. Zero upstream go.temporal.io dependencies.
//
// Activities run inside a worker. The worker's task-dispatch loop builds an
// activity [Scope] per task, embeds it in the activity's context via
// [NewContext], then calls the user's activity function. Inside the activity,
// [GetInfo], [GetLogger] and [RecordHeartbeat] read that scope.
//
// The contract is one-sided: the worker imports this package, not the other
// way round. Workers are free to plug [Scope.HeartbeatSink] into whatever
// transport they use (the reference worker uses luxfi/zap on port 9652).
package activity

import (
	"context"
	"sync"
	"time"

	"github.com/luxfi/log"
)

// WorkflowExecution identifies a workflow run.
type WorkflowExecution struct {
	WorkflowID string
	RunID      string
}

// Info contains information about a currently executing activity.
//
// Values are a snapshot at the moment [GetInfo] was called. Mutating the
// returned value cannot affect the worker's state.
type Info struct {
	TaskToken         []byte
	WorkflowExecution WorkflowExecution
	ActivityID        string
	ActivityType      string
	TaskQueue         string
	Attempt           int32
	ScheduledTime     time.Time
	StartedTime       time.Time
}

// Scope is the worker-owned execution context for a single activity task.
// The worker constructs one, calls [NewContext] to embed it in the activity
// context, then invokes the user's activity function. The helpers in this
// package read the scope back out of the context.
//
// A Scope is safe for concurrent use: activity code may call
// [RecordHeartbeat] from goroutines it spawns.
type Scope struct {
	// Info is the per-task information exposed via [GetInfo].
	Info Info

	// Logger is returned by [GetLogger]. If nil, [GetLogger] returns
	// [log.Noop]. The worker should pre-bind activity fields
	// (activity_id, attempt, workflow_id) before assigning.
	Logger log.Logger

	// HeartbeatSink, if non-nil, is invoked by [RecordHeartbeat] after the
	// scope records the details locally. The worker wires this to the
	// transport that ships heartbeats to the task server. In tests this
	// is typically left nil and the collected details are read via
	// [Scope.Heartbeats].
	HeartbeatSink func(details ...interface{})

	mu         sync.Mutex
	heartbeats [][]interface{}
}

// Heartbeats returns a copy of every details set recorded on this scope, in
// the order they were recorded. Intended for tests; the returned slice is
// independent of the scope's internal storage and safe to modify.
func (s *Scope) Heartbeats() [][]interface{} {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.heartbeats) == 0 {
		return nil
	}
	out := make([][]interface{}, len(s.heartbeats))
	for i, d := range s.heartbeats {
		dup := make([]interface{}, len(d))
		copy(dup, d)
		out[i] = dup
	}
	return out
}

// recordHeartbeat is the internal path invoked by [RecordHeartbeat].
// It first snapshots details into the scope (so tests can assert), then
// forwards to HeartbeatSink if set.
func (s *Scope) recordHeartbeat(details ...interface{}) {
	if s == nil {
		return
	}
	// Defensive copy so callers mutating the slice after the call can't
	// mutate the scope's captured record.
	captured := make([]interface{}, len(details))
	copy(captured, details)

	s.mu.Lock()
	s.heartbeats = append(s.heartbeats, captured)
	sink := s.HeartbeatSink
	s.mu.Unlock()

	if sink != nil {
		sink(details...)
	}
}

// scopeKey is the private context key type. Defined as a distinct struct so
// no other package can construct the same key value, preventing collisions
// and impersonation.
type scopeKey struct{}

var activeScopeKey = scopeKey{}

// NewContext returns a child context carrying scope. The returned context is
// what the worker should hand to the user's activity function. If scope is
// nil, ctx is returned unchanged.
func NewContext(ctx context.Context, scope *Scope) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, activeScopeKey, scope)
}

// fromContext fetches the scope embedded by [NewContext], or nil if none.
func fromContext(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(activeScopeKey).(*Scope)
	return s
}

// FromContext returns the [Info] embedded in ctx and true, or a zero value
// and false if ctx carries no activity scope. Intended for worker-side code
// that needs to inspect the activity context without going through the
// logging/heartbeat helpers.
func FromContext(ctx context.Context) (Info, bool) {
	s := fromContext(ctx)
	if s == nil {
		return Info{}, false
	}
	return copyInfo(s.Info), true
}

// copyInfo returns an independent copy of info. Specifically, TaskToken is
// deep-copied so activity code cannot mutate the worker's stored token.
func copyInfo(info Info) Info {
	if info.TaskToken != nil {
		tok := make([]byte, len(info.TaskToken))
		copy(tok, info.TaskToken)
		info.TaskToken = tok
	}
	return info
}

// GetInfo returns information about the currently executing activity. If ctx
// carries no activity scope (for example, the activity is being exercised
// from a unit test without a worker harness), GetInfo returns the zero Info.
func GetInfo(ctx context.Context) Info {
	s := fromContext(ctx)
	if s == nil {
		return Info{}
	}
	return copyInfo(s.Info)
}

// GetLogger returns a structured logger scoped to the activity. If ctx
// carries no scope, or the scope has no logger, [log.Noop] is returned so
// the caller never has to nil-check.
func GetLogger(ctx context.Context) log.Logger {
	s := fromContext(ctx)
	if s == nil || s.Logger == nil {
		return log.Noop()
	}
	return s.Logger
}

// RecordHeartbeat pings the worker that the activity is still running and
// attaches the given details. If ctx carries no scope, the call is a no-op.
// Details are defensively copied before being handed to the sink.
func RecordHeartbeat(ctx context.Context, details ...interface{}) {
	s := fromContext(ctx)
	if s == nil {
		return
	}
	s.recordHeartbeat(details...)
}
