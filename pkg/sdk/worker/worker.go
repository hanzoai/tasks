// Copyright © 2026 Hanzo AI. MIT License.

// Package worker is the Hanzo Tasks worker runtime. A Worker
// subscribes once per kind and receives server-pushed workflow and
// activity tasks over luxfi/zap, dispatches each task to the
// registered user function, and ships the result back.
//
// Layering:
//
//	pkg/sdk/client    — Dial, Client, Transport, WorkerTransport
//	pkg/sdk/worker    — THIS PACKAGE
//	pkg/sdk/workflow  — Context / Future / ExecuteActivity / Sleep / ...
//	pkg/sdk/activity  — GetInfo / GetLogger / RecordHeartbeat
//	pkg/sdk/temporal  — *Error / RetryPolicy / failure serde
//
// Zero go.temporal.io/* imports. Zero google.golang.org/grpc
// imports. Transport is luxfi/zap; logging is github.com/luxfi/log.
//
// Determinism (Phase 1)
//
// This worker re-runs the registered workflow function from its
// start on every workflow-task dispatch. The function must be pure
// with respect to its inputs: same arguments => same sequence of
// workflow primitives. Activities are expected to be idempotent,
// which is the same contract Temporal imposes. Phase 2 will land
// event-sourced replay (a history log and replay decider) without
// changing this package's public surface.
//
// # Server-push delivery
//
// On Start the worker installs OnWorkflowTask / OnActivityTask /
// OnActivityResult callbacks on the transport and issues one
// SubscribeWorkflowTasks + one SubscribeActivityTasks. The server
// pushes work as it arrives via OpcodeDeliverWorkflowTask /
// OpcodeDeliverActivityTask; the worker dispatches each delivery
// in a goroutine bounded by the configured concurrency caps
// (workflowExecSem, activityLimiter, taskQueueLimiter).
// Options.MaxConcurrentWorkflowTaskPollers /
// MaxConcurrentActivityExecutionSize remain on Options for API
// compatibility but no longer drive long-poll loops; they cap
// in-flight execution only.
package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	luxlog "github.com/luxfi/log"
	"golang.org/x/time/rate"
)

// Options configures a Worker. Field shape matches
// go.temporal.io/sdk/worker.Options so caller code migrating from
// upstream compiles unchanged. Any zero value means "unlimited /
// default"; the Options struct is never validated for positivity.
type Options struct {
	// MaxConcurrentActivityExecutionSize is both the activity poller
	// count and the soft cap on concurrent activity executions. Each
	// poller thread executes activities synchronously on itself, so
	// the two numbers are the same knob. 0 → defaultActivityPollers.
	MaxConcurrentActivityExecutionSize int

	// MaxConcurrentWorkflowTaskPollers is the workflow-task poller
	// count. 0 → defaultWorkflowPollers.
	MaxConcurrentWorkflowTaskPollers int

	// MaxConcurrentWorkflowTaskExecutionSize is the concurrency cap
	// on workflow-task execution. Distinct from the poller count; a
	// worker can poll more aggressively than it executes. 0 =
	// unlimited (bounded only by MaxConcurrentWorkflowTaskPollers).
	MaxConcurrentWorkflowTaskExecutionSize int

	// MaxConcurrentLocalActivityExecutionSize caps concurrent local
	// activity executions. Phase-1 local activities are dispatched
	// via the remote path; this knob is honoured in the common
	// semaphore around ExecuteActivity. 0 = unlimited.
	MaxConcurrentLocalActivityExecutionSize int

	// WorkerActivitiesPerSecond caps this worker's activity dispatch
	// rate. Per-worker; does not coordinate across replicas. 0 =
	// unlimited.
	WorkerActivitiesPerSecond float64

	// WorkerLocalActivitiesPerSecond caps this worker's local
	// activity dispatch rate. 0 = unlimited.
	WorkerLocalActivitiesPerSecond float64

	// TaskQueueActivitiesPerSecond is the shared-across-workers rate
	// intended as a task-queue global. Phase-1 enforces it per-worker
	// — true global coordination requires a server-side limiter and
	// arrives with the native serde milestone. 0 = unlimited.
	TaskQueueActivitiesPerSecond float64

	// EnableSessionWorker registers a default session tracker that
	// no-ops but satisfies the upstream API. Required by callers
	// that enable activity sessions; harmless otherwise.
	EnableSessionWorker bool

	// Identity is sent to the server on every poll so the frontend
	// attributes tasks to this worker. Empty → "<hostname>@<pid>".
	Identity string

	// Logger overrides the Worker's logger. Nil → luxlog.Noop().
	Logger luxlog.Logger
}

// Default values used when Options leaves a field zero.
const (
	defaultActivityPollers = 8
	defaultWorkflowPollers = 4
)

// Worker is the Hanzo Tasks worker runtime. Methods are safe for
// concurrent use except as noted.
type Worker interface {
	// RegisterWorkflow adds a workflow function to the registry under
	// its reflected Go name. Must be called before Start.
	RegisterWorkflow(w any)

	// RegisterWorkflowWithOptions adds a workflow function under an
	// explicit name. Must be called before Start.
	RegisterWorkflowWithOptions(w any, opts RegisterWorkflowOptions)

	// RegisterActivity adds an activity function under its reflected
	// Go name. Must be called before Start.
	RegisterActivity(a any)

	// RegisterActivityWithOptions adds an activity function under an
	// explicit name. Must be called before Start.
	RegisterActivityWithOptions(a any, opts RegisterActivityOptions)

	// Start begins polling. Non-blocking: returns as soon as the
	// poller goroutines are launched. Safe to call exactly once.
	Start() error

	// Run starts the worker and blocks until interruptCh closes or
	// Stop is called. Interruption triggers a graceful shutdown.
	Run(interruptCh <-chan any) error

	// Stop terminates all pollers and waits for in-flight tasks to
	// finish. Idempotent.
	Stop()
}

// New returns a Worker attached to c that polls taskQueue.
//
// The Worker shares c's underlying Transport — only one luxfi/zap
// connection is opened per process even when several Workers run
// against different task queues.
func New(c client.Client, taskQueue string, options Options) Worker {
	tr := client.TransportOf(c)
	// Future Client implementations outside this package may return
	// nil from TransportOf — a Worker with no transport can still
	// register workflows (valuable for unit tests that never Start).
	var wt client.WorkerTransport
	if tr != nil {
		wt = client.NewWorkerTransport(tr)
	}

	identity := options.Identity
	if identity == "" {
		identity = client.IdentityOf(c)
	}
	if identity == "" {
		identity = defaultIdentity()
	}

	namespace := client.NamespaceOf(c)
	if namespace == "" {
		namespace = "default"
	}

	opts := options
	if opts.MaxConcurrentActivityExecutionSize <= 0 {
		opts.MaxConcurrentActivityExecutionSize = defaultActivityPollers
	}
	if opts.MaxConcurrentWorkflowTaskPollers <= 0 {
		opts.MaxConcurrentWorkflowTaskPollers = defaultWorkflowPollers
	}
	logger := opts.Logger
	if logger == nil {
		logger = luxlog.Noop()
	}

	w := &workerImpl{
		client:    c,
		transport: wt,
		taskQueue: taskQueue,
		namespace: namespace,
		identity:  identity,
		opts:      opts,
		logger:    logger,
		registry:  newRegistry(),
		stopCh:    make(chan struct{}),
	}

	// Rate limiters: a zero rate means "unlimited" (no limiter). Burst
	// is 1 so bursty dispatch is throttled to the steady-state rate.
	if opts.WorkerActivitiesPerSecond > 0 {
		w.activityLimiter = rate.NewLimiter(rate.Limit(opts.WorkerActivitiesPerSecond), 1)
	}
	if opts.WorkerLocalActivitiesPerSecond > 0 {
		w.localActivityLimiter = rate.NewLimiter(rate.Limit(opts.WorkerLocalActivitiesPerSecond), 1)
	}
	if opts.TaskQueueActivitiesPerSecond > 0 {
		w.taskQueueLimiter = rate.NewLimiter(rate.Limit(opts.TaskQueueActivitiesPerSecond), 1)
	}

	// Execution semaphores: zero cap means "no cap".
	if opts.MaxConcurrentWorkflowTaskExecutionSize > 0 {
		w.workflowExecSem = make(chan struct{}, opts.MaxConcurrentWorkflowTaskExecutionSize)
	}
	if opts.MaxConcurrentLocalActivityExecutionSize > 0 {
		w.localActExecSem = make(chan struct{}, opts.MaxConcurrentLocalActivityExecutionSize)
	}

	if opts.EnableSessionWorker {
		w.sessionTracker = &sessionTracker{}
	}

	return w
}

// workerImpl is the concrete Worker.
type workerImpl struct {
	client    client.Client
	transport client.WorkerTransport
	taskQueue string
	namespace string
	identity  string
	opts      Options
	logger    luxlog.Logger
	registry  *registry

	// Rate limiters. Nil limiters mean "unlimited".
	//
	// activityLimiter covers all remote activity dispatches observed
	// by this worker (per-worker scope). The task-queue limit shares
	// the same limiter chain: v1 enforces it per-worker since true
	// cross-worker coordination requires a server-side gatekeeper.
	activityLimiter      *rate.Limiter
	localActivityLimiter *rate.Limiter
	taskQueueLimiter     *rate.Limiter

	// Execution-size semaphores. Nil = unlimited.
	workflowExecSem chan struct{}
	localActExecSem chan struct{}

	// Optional session tracker. Non-nil when EnableSessionWorker was
	// set; Phase-1 implementation is a no-op tracker that satisfies
	// the upstream API without running real sessions.
	sessionTracker *sessionTracker

	// Subscription IDs (server-pushed delivery). Populated by
	// startSubscriptions; cleared by Stop via Unsubscribe.
	subMu         sync.Mutex
	workflowSubID string
	activitySubID string

	// pendingActivities holds the chan into which OnActivityResult
	// pushes the result for an activityID previously scheduled by
	// a workflow's ExecuteActivity call. The chan is buffered (cap=1)
	// so the transport delivery goroutine never blocks — the workflow
	// goroutine drains on its own pace via select.
	actMu             sync.Mutex
	pendingActivities map[string]chan *activityResultMsg

	startOnce sync.Once
	stopOnce  sync.Once
	startErr  error

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// activityResultMsg is the payload OnActivityResult delivers to a
// pending ExecuteActivity future.
type activityResultMsg struct {
	result  []byte
	failure []byte
}

// registerPendingActivity returns a chan the workflow goroutine can
// receive on; completeActivity will deliver into it when the server
// pushes OpcodeDeliverActivityResult for activityID. Caller must
// removePendingActivity once it has received the result (or the
// surrounding ctx is done) to avoid leaking the entry.
func (w *workerImpl) registerPendingActivity(activityID string) chan *activityResultMsg {
	w.actMu.Lock()
	defer w.actMu.Unlock()
	if w.pendingActivities == nil {
		w.pendingActivities = make(map[string]chan *activityResultMsg)
	}
	ch := make(chan *activityResultMsg, 1)
	w.pendingActivities[activityID] = ch
	return ch
}

// removePendingActivity drops the entry for activityID. Idempotent.
func (w *workerImpl) removePendingActivity(activityID string) {
	w.actMu.Lock()
	defer w.actMu.Unlock()
	delete(w.pendingActivities, activityID)
}

// completeActivity is the OnActivityResult callback. It looks up the
// pending channel and delivers the result. If no entry exists (the
// workflow already gave up, or the activityID is unknown), the
// delivery is dropped silently — the server is single-source-of-truth
// and does not retry result pushes for unsubscribed ids.
func (w *workerImpl) completeActivity(activityID string, result, failure []byte) {
	w.actMu.Lock()
	ch, ok := w.pendingActivities[activityID]
	w.actMu.Unlock()
	if !ok || ch == nil {
		return
	}
	// chan is buffered cap=1; if a duplicate delivery arrives, drop it.
	select {
	case ch <- &activityResultMsg{result: result, failure: failure}:
	default:
	}
}

// sessionTracker is the Phase-1 no-op session worker. Retained as a
// distinct type so future wiring (real session activities, heartbeat
// coordination) can replace it without changing the Options shape.
type sessionTracker struct{}

// Start implements Worker. The server-push wire model means Start
// installs delivery handlers and issues two Subscribe calls (one for
// workflow tasks, one for activity tasks); subsequent task pushes
// arrive asynchronously and dispatch in goroutines bounded by the
// configured concurrency caps. There are no poller goroutines.
func (w *workerImpl) Start() error {
	w.startOnce.Do(func() {
		if w.transport == nil {
			w.startErr = errors.New("hanzo/tasks/worker: Client has no transport (dial with a real Transport)")
			return
		}
		if w.taskQueue == "" {
			w.startErr = errors.New("hanzo/tasks/worker: taskQueue is required")
			return
		}
		if err := w.startSubscriptions(); err != nil {
			w.startErr = fmt.Errorf("hanzo/tasks/worker: subscribe: %w", err)
			return
		}
	})
	return w.startErr
}

// Run implements Worker.
func (w *workerImpl) Run(interruptCh <-chan any) error {
	if err := w.Start(); err != nil {
		return err
	}
	if interruptCh != nil {
		select {
		case <-interruptCh:
		case <-w.stopCh:
		}
	} else {
		<-w.stopCh
	}
	w.Stop()
	return nil
}

// Stop implements Worker. Closes stopCh, sends Unsubscribe for each
// active subscription so the server stops pushing, and waits for any
// in-flight task dispatches to drain.
func (w *workerImpl) Stop() {
	w.stopOnce.Do(func() {
		close(w.stopCh)
		// Best-effort unsubscribe; a failed call (server gone) is
		// harmless because the server cleans up subs on disconnect.
		w.subMu.Lock()
		wfSub, actSub := w.workflowSubID, w.activitySubID
		w.workflowSubID, w.activitySubID = "", ""
		w.subMu.Unlock()
		if w.transport != nil {
			ctx, cancel := context.WithCancel(context.Background())
			if wfSub != "" {
				_ = w.transport.Unsubscribe(ctx, wfSub)
			}
			if actSub != "" {
				_ = w.transport.Unsubscribe(ctx, actSub)
			}
			cancel()
		}
	})
	w.wg.Wait()
}

// stopped reports whether Stop has been called.
func (w *workerImpl) stopped() bool {
	select {
	case <-w.stopCh:
		return true
	default:
		return false
	}
}

// ctxWithStop returns a child context that is canceled when Stop is
// called on the worker.
func (w *workerImpl) ctxWithStop(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-w.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

// defaultIdentity returns hostname@pid as a conservative default.
func defaultIdentity() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "hanzo-tasks-worker"
	}
	return fmt.Sprintf("%s@%d", host, os.Getpid())
}

// RegisterWorkflow / etc. delegate to the registry (defined in
// registry.go). Having thin methods here keeps worker.go focused on
// lifecycle and keeps registry wiring contained.
func (w *workerImpl) RegisterWorkflow(fn any) {
	w.registry.registerWorkflow(fn, RegisterWorkflowOptions{})
}

func (w *workerImpl) RegisterWorkflowWithOptions(fn any, opts RegisterWorkflowOptions) {
	w.registry.registerWorkflow(fn, opts)
}

func (w *workerImpl) RegisterActivity(fn any) {
	w.registry.registerActivity(fn, RegisterActivityOptions{})
}

func (w *workerImpl) RegisterActivityWithOptions(fn any, opts RegisterActivityOptions) {
	w.registry.registerActivity(fn, opts)
}

// interruptChOnce guards the package-level interrupt channel so it is
// installed exactly once per process — subsequent InterruptCh calls
// return the same channel. This matches upstream semantics: a worker
// that calls `w.Run(worker.InterruptCh())` hands the same listener
// across every worker it wires.
var (
	interruptChOnce sync.Once
	interruptCh     chan any
)

// InterruptCh returns a process-wide channel that closes on the first
// SIGINT or SIGTERM. Pass it to Worker.Run to trigger a graceful
// shutdown on Ctrl-C or `kill`. Repeated calls return the same channel.
//
// The channel is `chan any` (not `chan os.Signal`) so it satisfies the
// Worker.Run signature (`<-chan any`) without leaking os.Signal into
// the caller's type surface. The value sent on interrupt is the
// os.Signal that caused it; callers typically ignore it.
func InterruptCh() <-chan any {
	interruptChOnce.Do(func() {
		interruptCh = make(chan any, 1)
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			sig := <-sigCh
			// Single-shot: the channel is CLOSED after the first
			// signal so all receivers observe it, and so a second
			// signal does not block trying to send.
			select {
			case interruptCh <- sig:
			default:
			}
			close(interruptCh)
		}()
	})
	return interruptCh
}
