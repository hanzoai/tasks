// Package workflow is the workflow-code-facing API of the Hanzo
// Tasks SDK. User workflow functions import this package and call
// its helpers (ExecuteActivity, GetSignalChannel, NewSelector, …)
// to express durable orchestration.
//
// Layering
//
// The package is DELIBERATELY THIN. All runtime behaviour —
// deterministic time, activity dispatch, signal queues, selector
// scheduling — is delegated to a CoroutineEnv supplied by the
// worker (pkg/sdk/worker). Users never see CoroutineEnv; they see
// Context, Future, Channel, Selector. Those types carry a
// CoroutineEnv handle under the hood.
//
// This separation lets pkg/sdk/workflow ship the public surface
// that base/commerce/ta call into today — with only the package
// path changing in their imports — while the coroutine scheduler
// and event-sourced replay engine live in pkg/sdk/worker and can
// evolve independently.
//
// Zero upstream imports. Zero go.temporal.io/*. Zero
// google.golang.org/grpc. Logging via github.com/luxfi/log.
//
// Determinism (Phase 1)
//
// Phase 1 of the worker runs a workflow as a single Go goroutine
// driving CoroutineEnv synchronously. There is NO event-sourced
// replay yet. Crashes mid-workflow restart the workflow from the
// beginning with the same input. This is safe as long as activities
// are idempotent — which is the same contract Temporal imposes —
// but you do lose in-flight-activity progress on crash. Phase 2
// will add the history log and true replay.
//
// Inside a workflow function you MUST NOT call time.Now(),
// rand.Read, or spawn goroutines. Use workflow.Now(ctx),
// workflow.NewTimer, and (phase 2) workflow.Go. Map iteration order
// and time-sensitive branching are also forbidden. CI will add a
// `tasks-vet` linter for these in a follow-up.
package workflow

import (
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/luxfi/log"
)

// CoroutineEnv is the package-private seam between this public
// surface and the worker runtime. pkg/sdk/worker supplies a
// concrete implementation at workflow-execution start and passes
// it to NewContextFromEnv; user code never sees this type.
//
// Every method runs on the workflow's single coroutine. A method
// that "blocks" (AwaitFuture, AwaitReceive, Sleep, Select) parks
// the coroutine until the event fires — during replay the same
// decisions must happen in the same order, which is the worker's
// problem, not this package's.
//
// The interface is intentionally small: every primitive callers use
// is expressible as a combination of a few env calls. Adding a new
// public helper means composing existing env methods, not extending
// this interface — unless the helper needs a genuinely new runtime
// capability (e.g. workflow.Go in phase 2).
type CoroutineEnv interface {
	// Now returns the deterministic workflow clock. It advances
	// only at commit points (timer fire, activity result); it does
	// not track wall-clock time.
	Now() time.Time

	// Logger returns a logger bound to this workflow execution.
	// Writes during replay are suppressed by the worker so logs
	// reflect only the real (non-replayed) progress of the run.
	Logger() log.Logger

	// Sleep blocks the coroutine for d of workflow time, unless the
	// workflow is canceled first. Returns temporal.Canceled if
	// canceled before d elapses.
	Sleep(d time.Duration) error

	// NewTimer schedules a durable timer and returns a Future that
	// settles with nil after d of workflow time, or with
	// temporal.Canceled if the timer is canceled.
	NewTimer(d time.Duration) Future

	// ExecuteActivity submits an activity for execution. The
	// Future settles with the activity's (result, error). The
	// activity handle is opaque to this package: it is resolved
	// against the worker's registered name by the worker itself.
	ExecuteActivity(opts ActivityOptions, activity any, args []any) Future

	// GetSignalChannel returns (a handle to) the named signal
	// channel. Repeated calls with the same name return handles
	// that observe the same queue — signals are keyed by name, not
	// by handle.
	GetSignalChannel(name string) ReceiveChannel

	// NewChannel creates a user channel. buffered=0 makes it
	// unbuffered; >0 is the capacity. name is used only for
	// debugging and replay identity and may be empty.
	NewChannel(name string, buffered int) Channel

	// Select waits for exactly one of the supplied cases to fire
	// and returns its index. The caller fires the matching user
	// callback; this keeps the env purely mechanical.
	//
	// Cases carry either a Future or a ReceiveChannel. The env's
	// implementation is free to reorder "ready" cases according to
	// its own determinism rules, but it must be deterministic
	// across replays.
	Select(cases []SelectCase) int

	// NewCancelScope derives a child cancel scope whose cancel
	// function, when invoked, cancels only workflow primitives
	// created under that scope (timers, activities, selects).
	// Returns a handle opaque to callers and the cancel function.
	NewCancelScope() (scope any, cancel CancelFunc)

	// ExecuteChildWorkflow starts a child workflow and returns a
	// ChildWorkflowFuture. The returned future's Get settles with
	// the child's result; its GetChildWorkflowExecution() returns a
	// Future that settles as soon as the server assigns a runID.
	//
	// Phase-1: the worker forwards the request to the frontend via
	// OpcodeStartChildWorkflow and emits a scheduleChildWorkflow
	// command (kind=3) so history-based replay can recover the
	// linkage. A StubEnv used in tests may fake this path.
	ExecuteChildWorkflow(childWorkflow any, args []any) ChildWorkflowFuture

	// CurrentScope returns the env's current cancel scope, which
	// Context uses to route Done()/Err() calls.
	CurrentScope() any

	// ScopeDone returns a done channel for the supplied scope. It
	// is closed when the scope is canceled.
	ScopeDone(scope any) <-chan struct{}

	// ScopeErr returns the cancellation error for the supplied
	// scope, or nil if not canceled.
	ScopeErr(scope any) error

	// WorkflowInfo returns a snapshot of stable metadata about the
	// running workflow (ID, RunID, TaskQueue, etc.). Phase 1
	// returns zero values for fields the worker hasn't wired yet.
	WorkflowInfo() Info

	// GetVersion returns the recorded version for changeID, clamped to
	// [minSupported, maxSupported]. The first call records
	// maxSupported; subsequent calls (and replays) return the recorded
	// value. See workflow.GetVersion for the user-facing contract.
	GetVersion(changeID string, minSupported, maxSupported Version) Version

	// SideEffect runs fn exactly once per workflow execution and
	// records its result into history. ctx is the user Context for
	// pass-through to fn. The returned bytes are the JSON-encoded
	// result; replays return the recorded bytes without re-running fn.
	SideEffect(fn func(ctx Context) any, ctx Context) ([]byte, error)

	// MutableSideEffect runs fn whenever the workflow re-evaluates
	// changeID. The recorded value is replaced when eq reports the new
	// value differs from the recorded one; otherwise the recorded
	// value is preserved across the call.
	MutableSideEffect(changeID string, fn func(ctx Context) any, eq func(a, b any) bool, ctx Context) ([]byte, error)

	// MetricsHandler returns the workflow-scoped metrics handler, or
	// nil when no provider is wired. workflow.GetMetricsHandler
	// upgrades a nil to a noop so user code never sees nil.
	MetricsHandler() MetricsHandler

	// UpsertSearchAttributes upserts attrs onto the workflow's
	// visibility record. Phase-1 forwards to the frontend; Phase-2
	// will record the upsert into history alongside other commands.
	UpsertSearchAttributes(attrs map[string]any) error
}

// SelectCase is the internal representation of a case submitted to
// Selector.Select. Exactly one of Future / Channel is non-nil.
type SelectCase struct {
	Future  Future
	Channel ReceiveChannel
}

// CancelFunc cancels a context or cancel scope. It is idempotent.
type CancelFunc func()

// Info is a stable snapshot of workflow identity. It mirrors the
// shape Temporal's WorkflowInfo carries so handlers that log it
// don't change at the call site after migration.
type Info struct {
	// WorkflowID is the caller-supplied business identifier.
	WorkflowID string

	// RunID is the engine-generated per-execution identifier.
	RunID string

	// WorkflowType is the registered name (typically the Go
	// function name) of the workflow being executed.
	WorkflowType string

	// TaskQueue is the queue the workflow is running on.
	TaskQueue string

	// Namespace groups workflow executions. Defaults to "default".
	Namespace string

	// Attempt is the 1-based retry attempt for this run.
	Attempt int32
}

// defaultRetryPolicy is the retry policy applied when ActivityOptions
// doesn't carry one. Callers who want to disable retries entirely
// set RetryPolicy.MaximumAttempts = 1.
func defaultRetryPolicy() *temporal.RetryPolicy { return temporal.DefaultRetryPolicy() }
