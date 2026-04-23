package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// Future is the workflow-side handle for an asynchronous result —
// the completion of an activity, a timer, or a user-settled promise.
//
// Future is exactly the Temporal shape so callers migrating from
// go.temporal.io/sdk/workflow change only the import.
//
//	f := workflow.ExecuteActivity(ctx, DoWork, req)
//	var resp Response
//	if err := f.Get(ctx, &resp); err != nil { ... }
type Future interface {
	// Get blocks the workflow coroutine until the future settles,
	// then decodes the result into valPtr. valPtr may be nil, in
	// which case the result payload is dropped. Returns the
	// settlement error, if any.
	Get(ctx Context, valPtr any) error

	// IsReady reports whether the future has settled. Useful for
	// non-blocking peeks inside a Selector callback; most code
	// calls Get directly.
	IsReady() bool
}

// Settleable is the env-facing side of a Future. The worker owns a
// Settleable handle for each Future it hands back and completes it
// when the activity/timer finishes. This package exposes it so the
// worker and the in-memory StubEnv can both drive futures without
// reaching into unexported state.
type Settleable interface {
	Future
	// Settle delivers a final (value, err). Subsequent Settle calls
	// are no-ops. value may be nil (e.g. for timers).
	Settle(value []byte, err error)
}

// future is the default Settleable. It holds a one-shot result slot
// and a channel used by Get to wait. The env closes settleCh on
// Settle; Get then reads the slot and returns.
type future struct {
	mu        sync.Mutex
	settled   bool
	settleCh  chan struct{}
	valueJSON []byte
	err       error
}

// NewFuture constructs a settleable Future. It is intended for the
// worker / StubEnv to call; ordinary workflow code receives Futures
// from ExecuteActivity / NewTimer / a Selector callback.
func NewFuture() Settleable {
	return &future{settleCh: make(chan struct{})}
}

// Settle records the final result. First call wins.
func (f *future) Settle(value []byte, err error) {
	f.mu.Lock()
	if f.settled {
		f.mu.Unlock()
		return
	}
	f.settled = true
	f.valueJSON = value
	f.err = err
	close(f.settleCh)
	f.mu.Unlock()
}

// IsReady reports whether Settle has been called.
func (f *future) IsReady() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settled
}

// Get blocks on settleCh; once the future settles it decodes the
// payload into valPtr. If ctx is canceled first, returns the
// scope's Err.
//
// Note: Get intentionally uses the coroutine's Select / Sleep path
// indirectly via the env's AwaitFuture? No — in Phase 1 the
// worker's event loop resolves futures on the same goroutine that
// called Get, so a naive channel receive here parks the goroutine
// in exactly the way Phase 1 expects. Phase 2 will swap this for a
// scheduler-driven block to survive replay. The public API does
// not change.
func (f *future) Get(ctx Context, valPtr any) error {
	if ctx == nil {
		return errors.New("workflow.Future.Get: nil context")
	}
	select {
	case <-f.settleCh:
	case <-ctx.Done():
		if err := ctx.Err(); err != nil {
			return err
		}
		return temporal.NewCanceledError()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if valPtr == nil || len(f.valueJSON) == 0 {
		return nil
	}
	if err := json.Unmarshal(f.valueJSON, valPtr); err != nil {
		return fmt.Errorf("workflow.Future.Get: decode: %w", err)
	}
	return nil
}

// EncodePayload serialises a value to the wire form a Future carries.
// Phase 1 uses JSON; Phase 2 will swap this for the canonical ZAP
// codec. The helper is exposed so the worker / StubEnv produce the
// same shape user code later decodes.
func EncodePayload(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
