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
type StubEnv struct {
	mu sync.Mutex

	clock       time.Time
	autoAdvance time.Duration // added to clock on each Now() call; 0 = frozen

	logger log.Logger

	// activity stubs: keyed by activity name (the Go function name
	// for function values, the literal string for string activities).
	activities map[string][]stubActivityResponse

	// signal channels by signal name.
	signals map[string]*chanImpl

	// cancellation: the root scope. Phase 1 uses a single scope
	// per env; children created by WithCancel get their own.
	rootScope *stubScope

	info Info
}

// stubActivityResponse is one queued reply for an activity.
type stubActivityResponse struct {
	value any
	err   error
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
		clock:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		logger:     log.Noop(),
		activities: make(map[string][]stubActivityResponse),
		signals:    make(map[string]*chanImpl),
		rootScope:  newStubScope(),
		info: Info{
			WorkflowID:   "stub-wf",
			RunID:        "stub-run",
			WorkflowType: "StubWorkflow",
			TaskQueue:    "stub-queue",
			Namespace:    "default",
			Attempt:      1,
		},
	}
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
	ch.mu.Unlock()
}

// AdvanceClock manually advances workflow time by d. Use this in
// tests that want to validate timer-driven behaviour without
// actually sleeping.
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
	// Synchronous: advance the clock. Tests that want to simulate
	// cancellation during Sleep should call Cancel before Sleep.
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

func (e *StubEnv) NewTimer(d time.Duration) Future {
	f := NewFuture()
	// Advance the clock and settle immediately. Tests that want to
	// delay until Advance*/Cancel do so by adding a zero-duration
	// NewTimer after manually advancing.
	e.AdvanceClock(d)
	f.Settle(nil, nil)
	return f
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

func (e *StubEnv) Select(cases []SelectCase) int {
	// Phase-1 stub Select: spin with a short sleep between polls.
	// Real worker will park the coroutine on a scheduler; we just
	// need enough to unblock tests where signals/futures arrive
	// from another goroutine.
	e.mu.Lock()
	sc := e.rootScope
	e.mu.Unlock()

	for {
		if idx := readyIndex(cases); idx >= 0 {
			return idx
		}
		select {
		case <-sc.done:
			return -1
		case <-time.After(time.Millisecond):
			// re-check
		}
	}
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

// stubEnv is the zero-value env returned as a defensive fallback
// when NewContextFromEnv is given a nil. Everything it does is
// either harmless or clearly-broken so tests notice.
type stubEnv struct{}

func (stubEnv) Now() time.Time        { return time.Time{} }
func (stubEnv) Logger() log.Logger    { return log.Noop() }
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
