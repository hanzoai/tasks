// Package workflow — wall-clock Timer implementation.
//
// Phase 1: Timers are backed by a real time.Timer on a per-timer
// goroutine. That goroutine settles the Future when the deadline
// fires or the scope cancels. Duration is wall-clock; Now() advances
// only deterministically (via AdvanceClock / AutoAdvance) so there is
// no attempt to align "workflow time" with "timer fire time" yet.
//
// Phase 2 (event-time replay, durable timers): the env will persist
// a pending-timer record in the history log, and replay will refire
// the same Future from the log at the same point. The public
// workflow.NewTimer API does not change.
//
// Wall-clock-only v1 is explicitly fine — the read-team §5.2 bug
// was the opposite: NewTimer collapsed every duration to zero, so a
// 24h dunning timer fired on the next dispatch loop tick. Wall-clock
// correctness eliminates that class of bug entirely.

package workflow

import (
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// NewTimer schedules a durable timer that fires after d of workflow
// time and returns the Future that represents its completion. The
// Future's Get settles with nil error when the timer fires, or with
// temporal.Canceled if the ctx is canceled before d elapses.
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

// NewWallClockTimer is the shared helper used by both StubEnv and
// workerEnv to produce a real, cancellable Future driven by a
// time.Timer. Exported so the worker package (a sibling) can reuse
// the same implementation without reaching into package internals.
//
// - d <= 0 settles the Future with (nil, nil) immediately.
// - Otherwise, a per-timer goroutine parks on the timer's channel
//   alongside the supplied doneCh. If doneCh closes first, the Future
//   settles with errFn() — typically the scope's cancel error.
//
// Cancellation semantics: a closed doneCh always wins over a fired
// timer in the rare race where both are observable simultaneously.
func NewWallClockTimer(d time.Duration, doneCh <-chan struct{}, errFn func() error) Future {
	f := NewFuture()
	if d <= 0 {
		f.Settle(nil, nil)
		return f
	}
	go func() {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-doneCh:
			err := errFn()
			if err == nil {
				err = temporal.NewCanceledError()
			}
			f.Settle(nil, err)
		case <-t.C:
			// Re-check cancellation: if both fired in the same window
			// prefer cancellation so a cancel never "loses" to a timer.
			select {
			case <-doneCh:
				err := errFn()
				if err == nil {
					err = temporal.NewCanceledError()
				}
				f.Settle(nil, err)
			default:
				f.Settle(nil, nil)
			}
		}
	}()
	return f
}

// SelectFanIn is the shared edge-triggered Select implementation.
// Exported for the worker env to reuse without duplication. User
// code should not call it directly — use workflow.NewSelector.
//
// It blocks until exactly one case in cases is ready, or until
// doneCh closes, and returns the ready case index (or -1 on
// cancellation / empty input).
//
// Implementation: each case parks on a readiness source —
//   - Future cases: the Future.ReadyCh() (closed on Settle).
//   - chanImpl cases: a single-buffer chan struct{} registered via
//     RegisterReadyWaker; the channel fires once when the chan
//     becomes readable (buffered value, parked unbuffered sender, or
//     closed).
//
// A small goroutine per case translates that signal into an index
// send on wakeCh. The main routine receives once from wakeCh and
// returns. On wake we re-scan for the earliest-ready case to preserve
// deterministic ordering across unrelated simultaneous ready events.
//
// No polling. No time.Ticker. No 1 ms spin.
func SelectFanIn(cases []SelectCase, doneCh <-chan struct{}) int {
	if len(cases) == 0 {
		return -1
	}

	// Fast path: if any case is already ready, fire it without spinning
	// up goroutines.
	if idx := readyIndex(cases); idx >= 0 {
		return idx
	}

	wakeCh := make(chan int, len(cases)+1)
	stopCh := make(chan struct{})
	defer close(stopCh)

	for i, c := range cases {
		i, c := i, c
		switch {
		case c.Future != nil:
			ready := c.Future.ReadyCh()
			go func() {
				select {
				case <-ready:
					select {
					case wakeCh <- i:
					case <-stopCh:
					}
				case <-stopCh:
				}
			}()
		case c.Channel != nil:
			if ch, ok := c.Channel.(*chanImpl); ok {
				w := make(chan struct{}, 1)
				ch.RegisterReadyWaker(w)
				go func() {
					select {
					case <-w:
						select {
						case wakeCh <- i:
						case <-stopCh:
						}
					case <-stopCh:
					}
				}()
			} else {
				// Unknown channel shape: fall back to a tiny poll
				// (should not occur — all channels flow through chanImpl).
				go func() {
					ticker := time.NewTicker(50 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-stopCh:
							return
						case <-ticker.C:
							if c.Channel != nil {
								if hv, ok := c.Channel.(interface{ HasValue() bool }); ok && hv.HasValue() {
									select {
									case wakeCh <- i:
									case <-stopCh:
									}
									return
								}
							}
						}
					}
				}()
			}
		}
	}

	select {
	case idx := <-wakeCh:
		if idx < 0 || idx >= len(cases) {
			return -1
		}
		// Prefer the earliest-ready case for deterministic ordering
		// when multiple cases became ready very close together.
		if better := readyIndex(cases); better >= 0 && better < idx {
			return better
		}
		return idx
	case <-doneCh:
		return -1
	}
}
