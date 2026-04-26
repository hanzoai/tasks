package workflow

import (
	"errors"
	"sync"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/luxfi/log"
)

// StubEnv is a single-goroutine, in-memory CoroutineEnv implementation
// sufficient for unit-testing workflow code without standing up a real
// worker. It IS NOT a substitute for the worker runtime; it lacks
// event-sourced replay, durable timers, and cross-process activity
// dispatch.
//
// StubEnv is test-only. Production workflow dispatch uses
// workerEnv (pkg/sdk/worker/env.go). Do NOT depend on StubEnv from
// non-test code — its API surface is intentionally unstable and may
// add / remove deterministic-testing hooks over time.
//
// Typical use:
//
//	env := workflow.NewStubEnv()
//	env.OnActivity("DoWork").Return(&Result{OK: true}, nil)
//	env.SignalAsync("claim", map[string]string{"agent_id": "alice"})
//	ctx := workflow.NewContextFromEnv(env)
//	out, err := MyWorkflow(ctx, input)
//
// The stub processes activities synchronously: ExecuteActivity looks
// up the registered response by matching function name / string and
// settles the Future on the same goroutine before returning it.
//
// Timers in the stub run against a real wall clock via workflow.Timer
// so that tests observe the same scheduling semantics as production
// (a Future that settles after d, cancelable via scope). Tests that
// want deterministic-time assertions use short durations (tens of
// milliseconds) or AdvanceClock in combination with the Now()
// helpers — AdvanceClock no longer force-fires timers.
type StubEnv struct {
	mu sync.Mutex

	clock       time.Time
	autoAdvance time.Duration // added to clock on each Now() call; 0 = frozen

	logger log.Logger

	// activity stubs: keyed by activity name (the Go function name
	// for function values, the literal string for string activities).
	activities map[string][]stubActivityResponse

	// child-workflow stubs: keyed by child workflow name.
	childWorkflows map[string][]stubChildWorkflowResponse

	// signal channels by signal name.
	signals map[string]*chanImpl

	// cancellation: the root scope. Phase 1 uses a single scope
	// per env; children created by WithCancel get their own.
	rootScope *stubScope

	info Info

	// versions records GetVersion outcomes per changeID. The first
	// call records maxSupported and seals the value; later calls
	// return the recorded value clamped to the new [min, max] range.
	versions map[string]Version

	// sideEffects records SideEffect outcomes per call site. The key
	// is the call ordinal (incrementing counter); replays of the same
	// run return the recorded JSON bytes.
	sideEffects []sideEffectRecord

	// sideEffectIdx tracks the next ordinal to consume on this run.
	sideEffectIdx int

	// mutables records MutableSideEffect outcomes per changeID. The
	// recorded value is replaced when eq reports the new value differs.
	mutables map[string][]byte

	// metricsHandler is the workflow-scoped metrics handler. nil =
	// noop.
	metricsHandler MetricsHandler

	// upserts records UpsertSearchAttributes calls so tests can
	// assert against them.
	upserts []map[string]any
}

// sideEffectRecord is one persisted SideEffect outcome.
type sideEffectRecord struct {
	bytes []byte
	err   error
}

// stubActivityResponse is one queued reply for an activity.
type stubActivityResponse struct {
	value any
	err   error
}

// stubChildWorkflowResponse is one queued reply for a child workflow.
type stubChildWorkflowResponse struct {
	value     any
	err       error
	execution WorkflowExecution
}

// stubScope implements the cancellation scope tracking for the stub.
type stubScope struct {
	done chan struct{}
	err  error
	once sync.Once
}

func newStubScope() *stubScope {
	return &stubScope{done: make(chan struct{})}
}

func (s *stubScope) cancel() {
	s.once.Do(func() {
		s.err = temporal.NewCanceledError()
		close(s.done)
	})
}

// NewStubEnv returns an in-memory CoroutineEnv for tests. Clock
// starts at a stable epoch and does not advance unless the caller
// sets AutoAdvance or calls AdvanceClock.
func NewStubEnv() *StubEnv {
	return &StubEnv{
		clock:          time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		logger:         log.Noop(),
		activities:     make(map[string][]stubActivityResponse),
		childWorkflows: make(map[string][]stubChildWorkflowResponse),
		signals:        make(map[string]*chanImpl),
		rootScope:      newStubScope(),
		info: Info{
			WorkflowID:   "stub-wf",
			RunID:        "stub-run",
			WorkflowType: "StubWorkflow",
			TaskQueue:    "stub-queue",
			Namespace:    "default",
			Attempt:      1,
		},
		versions: make(map[string]Version),
		mutables: make(map[string][]byte),
	}
}

// SetMetricsHandler installs a workflow-scoped metrics handler the
// stub returns from MetricsHandler(). Pass nil to reset to the noop
// fallback.
func (e *StubEnv) SetMetricsHandler(h MetricsHandler) {
	e.mu.Lock()
	e.metricsHandler = h
	e.mu.Unlock()
}

// UpsertedSearchAttributes returns a copy of the attributes the
// workflow upserted via UpsertSearchAttributes. Tests assert against
// it after running the workflow.
func (e *StubEnv) UpsertedSearchAttributes() []map[string]any {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]map[string]any, len(e.upserts))
	for i, m := range e.upserts {
		c := make(map[string]any, len(m))
		for k, v := range m {
			c[k] = v
		}
		out[i] = c
	}
	return out
}

// OnActivity queues a response for the named activity. Pass a
// function value (reflect.TypeOf(fn).Name() is matched) or a literal
// string. Returns the env so multiple OnActivity.Return calls chain.
//
// Example:
//
//	env.OnActivity("DoWork").Return(&Result{OK: true}, nil)
//	env.OnActivity(DoWork).Return(&Result{OK: false}, errors.New("boom"))
func (e *StubEnv) OnActivity(activity any) *stubActivityReg {
	return &stubActivityReg{env: e, name: activityName(activity)}
}

// stubActivityReg is the fluent registration handle.
type stubActivityReg struct {
	env  *StubEnv
	name string
}

// Return queues one response. Responses are consumed in FIFO order;
// when the queue is empty, the stub falls back to (nil, nil).
func (r *stubActivityReg) Return(value any, err error) *stubActivityReg {
	r.env.mu.Lock()
	r.env.activities[r.name] = append(r.env.activities[r.name], stubActivityResponse{value: value, err: err})
	r.env.mu.Unlock()
	return r
}

// SignalAsync enqueues a signal value for the named channel. The
// channel is created on demand if no workflow code has asked for it
// yet.
func (e *StubEnv) SignalAsync(name string, val any) {
	e.mu.Lock()
	ch, ok := e.signals[name]
	if !ok {
		ch = newChan(name, 1024)
		e.signals[name] = ch
	}
	e.mu.Unlock()
	// Send with a detached context: signals are fire-and-forget.
	payload, err := EncodePayload(val)
	if err != nil {
		return
	}
	ch.mu.Lock()
	ch.buf = append(ch.buf, payload)
	if len(ch.recvWaiters) > 0 {
		// Hand directly to the first waiter so Receive doesn't have
		// to do a full re-loop.
		r := ch.recvWaiters[0]
		ch.recvWaiters = ch.recvWaiters[1:]
		r.payload = ch.buf[0]
		ch.buf = ch.buf[1:]
		r.ok = true
		close(r.done)
	}
	ch.signalWakersLocked()
	ch.mu.Unlock()
}

// AdvanceClock manually advances workflow Now() by d. It does NOT
// fire pending timers: timers are wall-clock-based so their firing
// is driven by the real OS scheduler. Tests that want to observe
// timer firing should either use small durations or run them on a
// separate goroutine.
func (e *StubEnv) AdvanceClock(d time.Duration) {
	e.mu.Lock()
	e.clock = e.clock.Add(d)
	e.mu.Unlock()
}

// SetAutoAdvance sets a per-Now increment. Setting to 1*time.Second
// means every Now() call jumps one second, which is enough to
// unblock simple Sleep-based tests without an explicit advance.
func (e *StubEnv) SetAutoAdvance(d time.Duration) {
	e.mu.Lock()
	e.autoAdvance = d
	e.mu.Unlock()
}

// Cancel cancels the root scope. Equivalent to the workflow's top-
// level cancellation firing. All active Sleeps, Timers, and Selects
// return temporal.Canceled.
func (e *StubEnv) Cancel() {
	e.mu.Lock()
	sc := e.rootScope
	e.mu.Unlock()
	sc.cancel()
}

// SetInfo overrides the WorkflowInfo surfaced by GetInfo. Useful for
// tests that assert on identity fields.
func (e *StubEnv) SetInfo(i Info) {
	e.mu.Lock()
	e.info = i
	e.mu.Unlock()
}

// SetLogger replaces the stub's logger. Defaults to log.Noop().
func (e *StubEnv) SetLogger(l log.Logger) {
	if l == nil {
		l = log.Noop()
	}
	e.mu.Lock()
	e.logger = l
	e.mu.Unlock()
}

// ---- CoroutineEnv ----

func (e *StubEnv) Now() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.autoAdvance > 0 {
		e.clock = e.clock.Add(e.autoAdvance)
	}
	return e.clock
}

func (e *StubEnv) Logger() log.Logger {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.logger == nil {
		return log.Noop()
	}
	return e.logger
}

func (e *StubEnv) Sleep(d time.Duration) error {
	// Synchronous: advance the deterministic clock. Tests that want
	// to simulate cancellation during Sleep call Cancel before Sleep.
	// Unlike NewTimer, Sleep is a pure clock-only primitive in the
	// stub — wall-clock delay would make determnistic Now() assertions
	// flaky.
	e.mu.Lock()
	sc := e.rootScope
	e.mu.Unlock()
	select {
	case <-sc.done:
		return sc.err
	default:
	}
	e.AdvanceClock(d)
	return nil
}

// NewTimer schedules a wall-clock timer using the real workflow.Timer
// helper. The returned Future settles with (nil, nil) after d, or
// with temporal.Canceled if the root scope cancels first.
func (e *StubEnv) NewTimer(d time.Duration) Future {
	e.mu.Lock()
	sc := e.rootScope
	e.mu.Unlock()
	return NewWallClockTimer(d, sc.done, func() error { return sc.err })
}

func (e *StubEnv) ExecuteActivity(opts ActivityOptions, activity any, args []any) Future {
	f := NewFuture()
	name := activityName(activity)
	e.mu.Lock()
	q := e.activities[name]
	var resp stubActivityResponse
	if len(q) > 0 {
		resp = q[0]
		e.activities[name] = q[1:]
	}
	e.mu.Unlock()

	// Apply RetryPolicy *only* for non-retryable short-circuit: we
	// don't simulate exponential-backoff retries in the stub. If
	// the caller wants to exercise retry, queue multiple responses.
	if resp.err != nil {
		_, ok := temporal.AsError(resp.err)
		if !ok {
			resp.err = temporal.NewErrorWithCause(resp.err.Error(), temporal.CodeApplication, resp.err, false)
		}
	}

	payload, encErr := EncodePayload(resp.value)
	if encErr != nil {
		f.Settle(nil, temporal.NewErrorWithCause(encErr.Error(), temporal.CodeApplication, encErr, true))
		return f
	}
	f.Settle(payload, resp.err)
	_ = opts // reserved for Phase 2 assertions
	return f
}

// OnChildWorkflow queues a response for a named child workflow. The
// next ExecuteChildWorkflow invocation with a matching type consumes
// the queued entry (FIFO). When the queue is empty the stub settles
// the future with (nil, nil) — same fallback shape as ExecuteActivity.
func (e *StubEnv) OnChildWorkflow(childWorkflow any) *stubChildWorkflowReg {
	return &stubChildWorkflowReg{env: e, name: activityName(childWorkflow)}
}

// stubChildWorkflowReg is the fluent registration handle for child
// workflows.
type stubChildWorkflowReg struct {
	env  *StubEnv
	name string
}

// Return queues one response; execution identifiers are populated
// with deterministic test-stable values.
func (r *stubChildWorkflowReg) Return(value any, err error) *stubChildWorkflowReg {
	r.env.mu.Lock()
	idx := len(r.env.childWorkflows[r.name])
	r.env.childWorkflows[r.name] = append(r.env.childWorkflows[r.name], stubChildWorkflowResponse{
		value: value,
		err:   err,
		execution: WorkflowExecution{
			WorkflowID: r.name + "-stub-" + itoa(idx),
			RunID:      r.name + "-run-" + itoa(idx),
		},
	})
	r.env.mu.Unlock()
	return r
}

// itoa avoids pulling strconv for the tiny integer-to-string conversion.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ExecuteChildWorkflow implements CoroutineEnv.
func (e *StubEnv) ExecuteChildWorkflow(childWorkflow any, args []any) ChildWorkflowFuture {
	cf, result, execution := NewChildWorkflowFuture()
	name := activityName(childWorkflow)
	e.mu.Lock()
	q := e.childWorkflows[name]
	var resp stubChildWorkflowResponse
	if len(q) > 0 {
		resp = q[0]
		e.childWorkflows[name] = q[1:]
	}
	e.mu.Unlock()

	// Resolve execution eagerly so test code observing the returned
	// future's GetChildWorkflowExecution gets a stable id regardless
	// of the result settlement.
	if resp.execution.WorkflowID == "" {
		resp.execution = WorkflowExecution{WorkflowID: name + "-stub", RunID: name + "-run"}
	}
	execBytes, _ := EncodePayload(resp.execution)
	execution.Settle(execBytes, nil)

	payload, encErr := EncodePayload(resp.value)
	if encErr != nil {
		result.Settle(nil, temporal.NewErrorWithCause(encErr.Error(), temporal.CodeApplication, encErr, true))
		return cf
	}
	result.Settle(payload, resp.err)
	_ = args // reserved for Phase 2 assertions
	return cf
}

func (e *StubEnv) GetSignalChannel(name string) ReceiveChannel {
	e.mu.Lock()
	defer e.mu.Unlock()
	ch, ok := e.signals[name]
	if !ok {
		ch = newChan(name, 1024)
		e.signals[name] = ch
	}
	return ch
}

func (e *StubEnv) NewChannel(name string, buffered int) Channel {
	return newChan(name, buffered)
}

// Select blocks until exactly one case is ready and returns its
// index. It uses the Selector fan-in (ReadyCh / RegisterReadyWaker)
// instead of a 1 ms polling spin: each case parks on the underlying
// Future's ReadyCh or the channel's waker, and the scope's done chan
// cancels the whole wait.
func (e *StubEnv) Select(cases []SelectCase) int {
	e.mu.Lock()
	sc := e.rootScope
	e.mu.Unlock()
	return SelectFanIn(cases, sc.done)
}

func (e *StubEnv) NewCancelScope() (any, CancelFunc) {
	sc := newStubScope()
	return sc, func() { sc.cancel() }
}

func (e *StubEnv) CurrentScope() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rootScope
}

func (e *StubEnv) ScopeDone(scope any) <-chan struct{} {
	if sc, ok := scope.(*stubScope); ok {
		return sc.done
	}
	if scope == nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.rootScope.done
	}
	// Unknown scope → a never-closing channel (safe default).
	blocked := make(chan struct{})
	return blocked
}

func (e *StubEnv) ScopeErr(scope any) error {
	if sc, ok := scope.(*stubScope); ok {
		return sc.err
	}
	if scope == nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.rootScope.err
	}
	return nil
}

func (e *StubEnv) WorkflowInfo() Info {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.info
}

// GetVersion records max on first call per changeID, returns the
// recorded value clamped to [min, max] on later calls.
func (e *StubEnv) GetVersion(changeID string, minSupported, maxSupported Version) Version {
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

// SideEffect runs fn once per call ordinal and records its bytes.
func (e *StubEnv) SideEffect(fn func(ctx Context) any, ctx Context) ([]byte, error) {
	e.mu.Lock()
	idx := e.sideEffectIdx
	e.sideEffectIdx++
	if idx < len(e.sideEffects) {
		rec := e.sideEffects[idx]
		e.mu.Unlock()
		return rec.bytes, rec.err
	}
	e.mu.Unlock()
	val := fn(ctx)
	bytes, err := EncodePayload(val)
	e.mu.Lock()
	e.sideEffects = append(e.sideEffects, sideEffectRecord{bytes: bytes, err: err})
	e.mu.Unlock()
	return bytes, err
}

// MutableSideEffect runs fn and records when eq reports a change.
func (e *StubEnv) MutableSideEffect(changeID string, fn func(ctx Context) any, eq func(a, b any) bool, ctx Context) ([]byte, error) {
	val := fn(ctx)
	bytes, err := EncodePayload(val)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	prev, ok := e.mutables[changeID]
	if ok && eq != nil {
		var prevVal, newVal any
		decode(prev, &prevVal)
		decode(bytes, &newVal)
		if eq(prevVal, newVal) {
			return prev, nil
		}
	}
	e.mutables[changeID] = bytes
	return bytes, nil
}

// MetricsHandler returns the installed handler or nil.
func (e *StubEnv) MetricsHandler() MetricsHandler {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.metricsHandler
}

// UpsertSearchAttributes records the upsert for tests to assert against.
func (e *StubEnv) UpsertSearchAttributes(attrs map[string]any) error {
	if attrs == nil {
		return nil
	}
	c := make(map[string]any, len(attrs))
	for k, v := range attrs {
		c[k] = v
	}
	e.mu.Lock()
	e.upserts = append(e.upserts, c)
	e.mu.Unlock()
	return nil
}

// stubEnv is the zero-value env returned as a defensive fallback
// when NewContextFromEnv is given a nil. Everything it does is
// either harmless or clearly-broken so tests notice.
type stubEnv struct{}

func (stubEnv) Now() time.Time            { return time.Time{} }
func (stubEnv) Logger() log.Logger        { return log.Noop() }
func (stubEnv) Sleep(time.Duration) error { return errors.New("workflow: nil env") }
func (stubEnv) NewTimer(time.Duration) Future {
	f := NewFuture()
	f.Settle(nil, errors.New("workflow: nil env"))
	return f
}
func (stubEnv) ExecuteActivity(ActivityOptions, any, []any) Future {
	f := NewFuture()
	f.Settle(nil, errors.New("workflow: nil env"))
	return f
}
func (stubEnv) ExecuteChildWorkflow(any, []any) ChildWorkflowFuture {
	cf, result, exec := NewChildWorkflowFuture()
	err := errors.New("workflow: nil env")
	result.Settle(nil, err)
	exec.Settle(nil, err)
	return cf
}
func (stubEnv) GetSignalChannel(name string) ReceiveChannel {
	c := newChan(name, 0)
	c.Close()
	return c
}
func (stubEnv) NewChannel(name string, buffered int) Channel { return newChan(name, buffered) }
func (stubEnv) Select([]SelectCase) int                      { return -1 }
func (stubEnv) NewCancelScope() (any, CancelFunc)            { return nil, func() {} }
func (stubEnv) CurrentScope() any                            { return nil }
func (stubEnv) ScopeDone(any) <-chan struct{}                { c := make(chan struct{}); return c }
func (stubEnv) ScopeErr(any) error                           { return nil }
func (stubEnv) WorkflowInfo() Info                           { return Info{} }
func (stubEnv) GetVersion(string, Version, Version) Version  { return DefaultVersion }
func (stubEnv) SideEffect(fn func(ctx Context) any, ctx Context) ([]byte, error) {
	if fn == nil {
		return nil, nil
	}
	return EncodePayload(fn(ctx))
}
func (stubEnv) MutableSideEffect(_ string, fn func(ctx Context) any, _ func(a, b any) bool, ctx Context) ([]byte, error) {
	if fn == nil {
		return nil, nil
	}
	return EncodePayload(fn(ctx))
}
func (stubEnv) MetricsHandler() MetricsHandler          { return nil }
func (stubEnv) UpsertSearchAttributes(map[string]any) error { return nil }

// activityName derives the activity's registered name. For function
// values we use reflect to read the function name; for strings we
// use the string directly. Anything else falls back to "unknown".
func activityName(activity any) string {
	if activity == nil {
		return "nil"
	}
	if s, ok := activity.(string); ok {
		return s
	}
	return funcName(activity)
}
