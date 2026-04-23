package workflow_test

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// TestSelect_FanInSettlesInOrder settles five parallel futures with
// 20 ms gaps and asserts that each Selector.Select returns in order
// and the total elapsed time is ~80 ms (not hundreds of ms that a
// 1 ms spin + scheduler jitter would produce).
func TestSelect_FanInSettlesInOrder(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	const n = 5
	futures := make([]workflow.Settleable, n)
	for i := range futures {
		futures[i] = workflow.NewFuture()
	}

	// Settle each future 20 ms apart, in order.
	go func() {
		for i := range futures {
			time.Sleep(20 * time.Millisecond)
			futures[i].Settle(nil, nil)
		}
	}()

	gotOrder := make([]int, 0, n)
	start := time.Now()

	// Reselect over the *remaining* unfired futures each iteration by
	// attaching a per-iteration done flag to detect re-fire.
	fired := make([]bool, n)
	for len(gotOrder) < n {
		sel := workflow.NewSelector(ctx)
		for i := range futures {
			if fired[i] {
				continue
			}
			i := i
			sel.AddFuture(futures[i], func(f workflow.Future) {
				_ = f.Get(ctx, nil)
				fired[i] = true
				gotOrder = append(gotOrder, i)
			})
		}
		sel.Select(ctx)
	}
	elapsed := time.Since(start)

	for i := 0; i < n; i++ {
		if gotOrder[i] != i {
			t.Fatalf("order = %v, want 0..4 ascending", gotOrder)
		}
	}

	// 5 fires × 20 ms = 100 ms ideal. Allow [70, 500] to cover
	// scheduler jitter on loaded CI without accepting a busy-spin
	// that'd be multi-hundred-ms.
	if elapsed < 70*time.Millisecond {
		t.Fatalf("too fast: elapsed=%v (got all fires before 70ms)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("too slow — likely busy-spin: elapsed=%v (want <=500ms)", elapsed)
	}
}

// TestSelect_NoCPUChurnWhileIdle asserts that a Select which has
// nothing to do doesn't churn allocs/goroutines. Prior impl polled
// every 1 ms, which burned CPU and generated a runaway alloc rate.
func TestSelect_NoCPUChurnWhileIdle(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	// Start a selector waiting on one unsettled future, in the
	// background. It blocks until we cancel.
	f := workflow.NewFuture()
	done := make(chan struct{})
	go func() {
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(f, func(workflow.Future) {})
		sel.Select(ctx)
		close(done)
	}()

	runtime.Gosched()
	time.Sleep(50 * time.Millisecond)

	var pre runtime.MemStats
	runtime.ReadMemStats(&pre)
	preGoroutines := runtime.NumGoroutine()

	// Idle for 500 ms while the Select sits blocked.
	time.Sleep(500 * time.Millisecond)

	var post runtime.MemStats
	runtime.ReadMemStats(&post)
	postGoroutines := runtime.NumGoroutine()

	// Alloc delta: 1 ms spin would produce tens of KB of alloc churn
	// across 500 ms. Real edge-triggered fan-in should produce almost
	// none — allow 64 KB ceiling for noise.
	allocDelta := post.TotalAlloc - pre.TotalAlloc
	if allocDelta > 64*1024 {
		t.Fatalf("idle Select allocated %d bytes over 500ms — busy-spin suspected", allocDelta)
	}

	// Goroutine count must not grow while idle.
	if postGoroutines > preGoroutines+1 {
		t.Fatalf("goroutine count grew: pre=%d post=%d — watcher leak suspected", preGoroutines, postGoroutines)
	}

	// Settle the future to let the background select return.
	f.Settle(nil, nil)
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Select did not wake after Settle")
	}
}

// TestSelect_CancelUnblocksPromptly verifies that cancelling the env
// unblocks a Select that is parked on unsettled futures, without the
// need for any timer to fire.
func TestSelect_CancelUnblocksPromptly(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	f1 := workflow.NewFuture()
	f2 := workflow.NewFuture()

	done := make(chan struct{})
	go func() {
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(f1, func(workflow.Future) {})
		sel.AddFuture(f2, func(workflow.Future) {})
		sel.Select(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	env.Cancel()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Select did not return after Cancel")
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("Select took %v to wake after Cancel — too slow", elapsed)
	}
}

// TestSelect_ChannelWakerFires verifies the chanImpl waker fan-in:
// a blocked Select attached to an empty signal channel wakes when a
// signal is asynchronously enqueued, without a polling ticker.
func TestSelect_ChannelWakerFires(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	ch := workflow.GetSignalChannel(ctx, "async")

	fired := int32(0)
	done := make(chan struct{})
	go func() {
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(ch, func(c workflow.ReceiveChannel, more bool) {
			var v string
			c.Receive(ctx, &v)
			if v == "hi" {
				atomic.StoreInt32(&fired, 1)
			}
		})
		sel.Select(ctx)
		close(done)
	}()

	time.Sleep(30 * time.Millisecond)
	env.SignalAsync("async", "hi")

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Select did not wake on async signal")
	}
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("receive callback did not see signal value")
	}
}
