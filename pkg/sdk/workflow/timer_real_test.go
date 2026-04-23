package workflow_test

import (
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// TestTimer_FiresOnRealDeadline asserts that a workflow.NewTimer(50ms)
// actually takes ~50 ms to settle — not instantaneously. This is the
// core regression guard for red-review §5.2: the old stub collapsed
// all durations to zero, so a 24 h timer fired immediately.
func TestTimer_FiresOnRealDeadline(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	f := workflow.NewTimer(ctx, 50*time.Millisecond)

	start := time.Now()
	if err := f.Get(ctx, nil); err != nil {
		t.Fatalf("timer Get: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond {
		t.Fatalf("timer fired too early: elapsed=%v (want >=40ms)", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("timer fired too late: elapsed=%v (want <=500ms)", elapsed)
	}
	if !f.IsReady() {
		t.Fatal("future not marked ready after Get")
	}
}

// TestTimer_CancelStopsFiring asserts that cancelling the scope
// makes a long-duration timer settle with ctx.Err() immediately,
// without waiting the nominal duration.
func TestTimer_CancelStopsFiring(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	f := workflow.NewTimer(ctx, 1*time.Hour)

	// Cancel asynchronously so Get is already blocked.
	go func() {
		time.Sleep(20 * time.Millisecond)
		env.Cancel()
	}()

	start := time.Now()
	err := f.Get(ctx, nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected cancel error, got nil")
	}
	if !temporal.IsCanceledError(err) {
		t.Fatalf("expected canceled error, got %v (%T)", err, err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("timer didn't cancel promptly: elapsed=%v (want <=500ms)", elapsed)
	}
}

// TestTimer_ZeroDuration_FiresImmediately asserts d=0 is legal and
// settles within ~10 ms. Semantically this is a "yield" — callers
// sometimes use it to park until the next coroutine tick.
func TestTimer_ZeroDuration_FiresImmediately(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	start := time.Now()
	f := workflow.NewTimer(ctx, 0)
	if err := f.Get(ctx, nil); err != nil {
		t.Fatalf("timer Get: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 10*time.Millisecond {
		t.Fatalf("zero-duration timer took too long: %v", elapsed)
	}
}

// TestTimer_NegativeDurationTreatedAsZero ensures the NewTimer
// wrapper's clamp (d<0 → d=0) still holds with the real impl.
func TestTimer_NegativeDurationTreatedAsZero(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	f := workflow.NewTimer(ctx, -5*time.Minute)
	if err := f.Get(ctx, nil); err != nil {
		t.Fatalf("timer Get: %v", err)
	}
	if !f.IsReady() {
		t.Fatal("future not ready after clamped-negative timer")
	}
}

// TestTimer_DoesNotCollapseDuration24h is a direct regression for
// the production incident: the old stub settled a 24 h timer on the
// next dispatch-loop tick. This test just guards that Get on a long
// timer is blocking, by checking it's still unsettled after 50 ms.
func TestTimer_DoesNotCollapseDuration24h(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	defer env.Cancel() // don't leak the goroutine past the test

	f := workflow.NewTimer(ctx, 24*time.Hour)

	// Sleep a hair and assert the future is still not ready.
	time.Sleep(50 * time.Millisecond)
	if f.IsReady() {
		t.Fatal("24h timer already ready after 50ms — duration collapsed")
	}
}
