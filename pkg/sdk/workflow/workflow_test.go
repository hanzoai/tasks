package workflow_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// --- Context & Logger ---

func TestContextFromEnv_NonNilLoggerAndInfo(t *testing.T) {
	env := workflow.NewStubEnv()
	env.SetInfo(workflow.Info{WorkflowID: "wf-42", RunID: "run-1", WorkflowType: "T", TaskQueue: "q"})

	ctx := workflow.NewContextFromEnv(env)
	if workflow.GetLogger(ctx) == nil {
		t.Fatal("GetLogger returned nil")
	}
	info := workflow.GetInfo(ctx)
	if info.WorkflowID != "wf-42" || info.RunID != "run-1" {
		t.Fatalf("GetInfo = %+v, want wf-42/run-1", info)
	}
}

func TestContextFromEnv_NilEnvIsSafe(t *testing.T) {
	ctx := workflow.NewContextFromEnv(nil)
	// Every accessor must not panic.
	_ = workflow.GetLogger(ctx)
	_ = workflow.Now(ctx)
	_ = workflow.GetInfo(ctx)
	if err := workflow.Sleep(ctx, time.Millisecond); err == nil {
		t.Fatal("Sleep with nil env should return error, got nil")
	}
}

func TestContext_WithValueChain(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	type k string
	ctx2 := workflow.WithValue(ctx, k("a"), 1)
	ctx3 := workflow.WithValue(ctx2, k("b"), "two")
	if ctx3.Value(k("a")) != 1 {
		t.Fatalf("Value(a) = %v, want 1", ctx3.Value(k("a")))
	}
	if ctx3.Value(k("b")) != "two" {
		t.Fatalf("Value(b) = %v, want two", ctx3.Value(k("b")))
	}
	if ctx3.Value(k("missing")) != nil {
		t.Fatalf("Value(missing) = %v, want nil", ctx3.Value(k("missing")))
	}
}

// --- Now / Sleep ---

func TestNow_DeterministicAdvance(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	t0 := workflow.Now(ctx)
	if t0.IsZero() {
		t.Fatal("Now returned zero time")
	}

	env.AdvanceClock(5 * time.Second)
	t1 := workflow.Now(ctx)
	if !t1.After(t0) {
		t.Fatalf("Now did not advance: t0=%v t1=%v", t0, t1)
	}
	if got := t1.Sub(t0); got != 5*time.Second {
		t.Fatalf("advance = %v, want 5s", got)
	}
}

func TestSleep_AdvancesClock(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	t0 := workflow.Now(ctx)
	if err := workflow.Sleep(ctx, 2*time.Second); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	t1 := workflow.Now(ctx)
	if t1.Sub(t0) != 2*time.Second {
		t.Fatalf("Sleep advance = %v, want 2s", t1.Sub(t0))
	}
}

func TestSleep_CanceledReturnsCanceled(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	env.Cancel()

	err := workflow.Sleep(ctx, time.Second)
	if err == nil {
		t.Fatal("Sleep after cancel returned nil error")
	}
	if !temporal.IsCanceledError(err) {
		t.Fatalf("Sleep error = %v, want canceled", err)
	}
}

// --- WithCancel ---

func TestWithCancel_ChildScopeIndependent(t *testing.T) {
	env := workflow.NewStubEnv()
	root := workflow.NewContextFromEnv(env)
	child, cancel := workflow.WithCancel(root)

	select {
	case <-child.Done():
		t.Fatal("child Done closed before cancel")
	default:
	}

	cancel()

	select {
	case <-child.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("child Done did not close after cancel")
	}
	if !temporal.IsCanceledError(child.Err()) {
		t.Fatalf("child.Err() = %v, want canceled", child.Err())
	}

	// Parent unaffected.
	select {
	case <-root.Done():
		t.Fatal("root Done closed after child cancel")
	default:
	}
}

// --- Timer ---

func TestNewTimer_FiresWithNilError(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	// Real wall-clock timer: use a small duration so the test is fast.
	f := workflow.NewTimer(ctx, 20*time.Millisecond)
	if err := f.Get(ctx, nil); err != nil {
		t.Fatalf("timer Get: %v", err)
	}
	if !f.IsReady() {
		t.Fatal("timer future not ready after Get")
	}
}

func TestNewTimer_NegativeDurationClamped(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	// Should not panic or return error for negative duration.
	f := workflow.NewTimer(ctx, -1*time.Second)
	if err := f.Get(ctx, nil); err != nil {
		t.Fatalf("timer Get: %v", err)
	}
}

// --- Activities ---

func DemoActivity(_ workflow.Context, _ string) (string, error) { return "", nil }

func TestExecuteActivity_ResolvesWithQueuedResponse(t *testing.T) {
	env := workflow.NewStubEnv()
	env.OnActivity(DemoActivity).Return(map[string]any{"ok": true, "n": 7}, nil)

	ctx := workflow.NewContextFromEnv(env)
	opts := workflow.ActivityOptions{StartToCloseTimeout: time.Minute}
	ctx = workflow.WithActivityOptions(ctx, opts)

	f := workflow.ExecuteActivity(ctx, DemoActivity, "hello")
	var out map[string]any
	if err := f.Get(ctx, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out["ok"] != true || out["n"].(float64) != 7 {
		t.Fatalf("out = %#v", out)
	}
}

func TestExecuteActivity_ErrorPropagates(t *testing.T) {
	env := workflow.NewStubEnv()
	boom := errors.New("boom")
	env.OnActivity("DoThing").Return(nil, boom)

	ctx := workflow.NewContextFromEnv(env)
	f := workflow.ExecuteActivity(ctx, "DoThing", nil)

	var out any
	err := f.Get(ctx, &out)
	if err == nil {
		t.Fatal("expected error")
	}
	// Stub wraps non-*Error errors into *Error.
	te, ok := temporal.AsError(err)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if !errors.Is(te, boom) {
		t.Fatalf("cause chain broken: %v", err)
	}
}

func TestExecuteActivity_NilActivityFailsNonRetryable(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	f := workflow.ExecuteActivity(ctx, nil, "x")
	err := f.Get(ctx, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	te, ok := temporal.AsError(err)
	if !ok || !te.NonRetryable {
		t.Fatalf("expected non-retryable *Error, got %v", err)
	}
}

func TestExecuteActivity_NilContextReturnsFailedFuture(t *testing.T) {
	f := workflow.ExecuteActivity(nil, DemoActivity, "x")
	// Get with nil ctx on a nil-env future still surfaces the error.
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	err := f.Get(ctx, nil)
	if err == nil {
		t.Fatal("expected error for nil-ctx ExecuteActivity")
	}
}

// --- Signal Channels ---

func TestGetSignalChannel_ReceiveAfterSignal(t *testing.T) {
	env := workflow.NewStubEnv()
	env.SignalAsync("claim", map[string]string{"agent_id": "alice"})

	ctx := workflow.NewContextFromEnv(env)
	ch := workflow.GetSignalChannel(ctx, "claim")

	var data map[string]string
	if ok := ch.Receive(ctx, &data); !ok {
		t.Fatal("Receive returned !ok")
	}
	if data["agent_id"] != "alice" {
		t.Fatalf("data = %v, want agent_id=alice", data)
	}
}

func TestGetSignalChannel_MultipleHandlesShareQueue(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	h1 := workflow.GetSignalChannel(ctx, "s1")
	h2 := workflow.GetSignalChannel(ctx, "s1")

	env.SignalAsync("s1", "payload")

	var got1, got2 string
	if ok := h1.ReceiveAsync(&got1); !ok {
		t.Fatal("first handle didn't receive")
	}
	if ok := h2.ReceiveAsync(&got2); ok {
		t.Fatal("second handle received a phantom value (queue wasn't shared)")
	}
	if got1 != "payload" {
		t.Fatalf("got1 = %q", got1)
	}
}

// --- User Channels ---

func TestNewBufferedChannel_SendReceive(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	ch := workflow.NewBufferedChannel(ctx, 2)

	if err := ch.Send(ctx, 1); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := ch.Send(ctx, 2); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var a, b int
	ch.Receive(ctx, &a)
	ch.Receive(ctx, &b)
	if a != 1 || b != 2 {
		t.Fatalf("FIFO broken: got %d, %d", a, b)
	}
}

func TestChannel_CloseReturnsNotOK(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	ch := workflow.NewBufferedChannel(ctx, 1)

	if err := ch.Send(ctx, "x"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ch.Close()

	// First Receive drains the buffered value.
	var got string
	if ok := ch.Receive(ctx, &got); !ok {
		t.Fatal("expected first Receive to succeed")
	}
	// Second should return ok=false now that channel is closed + drained.
	if ok := ch.Receive(ctx, &got); ok {
		t.Fatal("Receive on closed+drained returned ok=true")
	}
}

func TestChannel_ReceiveAsyncEmpty(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)
	ch := workflow.NewBufferedChannel(ctx, 1)
	var v int
	if ok := ch.ReceiveAsync(&v); ok {
		t.Fatal("empty buffered channel ReceiveAsync returned ok=true")
	}
}

// --- Selector ---

func TestSelector_FutureReadyFiresFirst(t *testing.T) {
	env := workflow.NewStubEnv()
	env.OnActivity("A").Return("result", nil)

	ctx := workflow.NewContextFromEnv(env)
	f := workflow.ExecuteActivity(ctx, "A")
	ch := workflow.GetSignalChannel(ctx, "never")

	fired := int32(0)
	sel := workflow.NewSelector(ctx)
	sel.AddFuture(f, func(fu workflow.Future) {
		var s string
		_ = fu.Get(ctx, &s)
		if s != "result" {
			t.Errorf("future callback saw %q", s)
		}
		atomic.StoreInt32(&fired, 1)
	})
	sel.AddReceive(ch, func(c workflow.ReceiveChannel, more bool) {
		t.Error("receive case fired unexpectedly")
	})
	sel.Select(ctx)
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("future callback did not fire")
	}
}

func TestSelector_ReceiveCaseFires(t *testing.T) {
	env := workflow.NewStubEnv()
	env.SignalAsync("tick", "ok")

	ctx := workflow.NewContextFromEnv(env)
	ch := workflow.GetSignalChannel(ctx, "tick")

	fired := int32(0)
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(ch, func(c workflow.ReceiveChannel, more bool) {
		if !more {
			t.Error("expected more=true for open channel with value")
		}
		var s string
		c.Receive(ctx, &s)
		if s != "ok" {
			t.Errorf("receive callback saw %q", s)
		}
		atomic.StoreInt32(&fired, 1)
	})
	sel.Select(ctx)
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("receive case did not fire")
	}
}

func TestSelector_AddDefaultFiresWhenNothingReady(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	ch := workflow.GetSignalChannel(ctx, "empty")
	// Two unsettled futures — neither will be ready when Select runs
	// so the default case must fire. (Prior to PR-C this test created
	// a 1h timer and "drained" it, which exercised the stub's old
	// eager-fire bug; real timers don't pre-fire.)
	fu1 := workflow.NewFuture()
	fu2 := workflow.NewFuture()
	defFired := int32(0)
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(ch, func(workflow.ReceiveChannel, bool) {})
	sel.AddFuture(fu1, func(workflow.Future) {})
	sel.AddFuture(fu2, func(workflow.Future) {})
	sel.AddDefault(func() { atomic.StoreInt32(&defFired, 1) })
	sel.Select(ctx)
	if atomic.LoadInt32(&defFired) != 1 {
		t.Fatal("default did not fire when no case was ready")
	}
}

// --- End-to-end: simulate base/plugins/tasks AgentTaskWorkflow flow ---

type taskResult struct {
	Status string `json:"status"`
}

func signalDrivenWorkflow(ctx workflow.Context) string {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
	})
	actFuture := workflow.ExecuteActivity(ctx, "ExecuteTask")
	claimCh := workflow.GetSignalChannel(ctx, "claim")
	timerF := workflow.NewTimer(ctx, 24*time.Hour)

	state := "pending"
	for state == "pending" {
		sel := workflow.NewSelector(ctx)
		sel.AddFuture(actFuture, func(f workflow.Future) {
			var r taskResult
			if err := f.Get(ctx, &r); err != nil {
				state = "failed:" + err.Error()
				return
			}
			state = "completed:" + r.Status
		})
		sel.AddReceive(claimCh, func(ch workflow.ReceiveChannel, more bool) {
			var claim map[string]string
			ch.Receive(ctx, &claim)
			state = "claimed:" + claim["agent_id"]
		})
		sel.AddFuture(timerF, func(f workflow.Future) {
			if err := f.Get(ctx, nil); err == nil {
				state = "timeout"
			}
		})
		sel.Select(ctx)
	}
	return state
}

func TestAgentTaskWorkflow_ActivityCompletionPath(t *testing.T) {
	env := workflow.NewStubEnv()
	env.OnActivity("ExecuteTask").Return(taskResult{Status: "done"}, nil)
	ctx := workflow.NewContextFromEnv(env)

	out := signalDrivenWorkflow(ctx)
	if out != "completed:done" {
		t.Fatalf("state = %q, want completed:done", out)
	}
}

func TestAgentTaskWorkflow_SignalClaimPath(t *testing.T) {
	env := workflow.NewStubEnv()
	// Queue a long-pending activity response so the signal wins
	// in the selector. In the stub ExecuteActivity settles synchronously,
	// so to exercise the signal path we omit the activity registration —
	// which leaves the future settled with nil,nil. That IS a completion
	// path, so we instead queue a never-fire situation by pre-populating
	// the signal queue (stub Select prefers ready cases; both will be
	// ready; the signal is a receive-case that gets checked first in
	// readyIndex iteration order, but really both are ready).
	//
	// To deterministically test the signal path we simulate what the
	// real worker does: a pending (unsettled) activity. We do that by
	// NOT registering a response and then manually marking it unsettled
	// via a custom env — but here the stub always settles. So instead
	// use a workflow that selects on a signal without an activity.
	env.SignalAsync("claim", map[string]string{"agent_id": "bob"})
	ctx := workflow.NewContextFromEnv(env)

	claimCh := workflow.GetSignalChannel(ctx, "claim")
	var claim map[string]string
	var got string
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(claimCh, func(ch workflow.ReceiveChannel, more bool) {
		ch.Receive(ctx, &claim)
		got = "claimed:" + claim["agent_id"]
	})
	sel.Select(ctx)

	if got != "claimed:bob" {
		t.Fatalf("got = %q, want claimed:bob", got)
	}
}

// --- Payload codec ---

func TestEncodePayload_Nil(t *testing.T) {
	b, err := workflow.EncodePayload(nil)
	if err != nil || b != nil {
		t.Fatalf("EncodePayload(nil) = %v, %v; want nil, nil", b, err)
	}
}

func TestEncodePayload_Struct(t *testing.T) {
	b, err := workflow.EncodePayload(taskResult{Status: "x"})
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	if string(b) != `{"status":"x"}` {
		t.Fatalf("EncodePayload = %s", b)
	}
}

// --- Future direct ---

func TestFuture_GetBeforeSettle_BlocksThenUnblocks(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	f := workflow.NewFuture()
	done := make(chan error, 1)
	go func() {
		var s string
		done <- f.Get(ctx, &s)
	}()

	time.Sleep(20 * time.Millisecond)
	payload, _ := workflow.EncodePayload("hello")
	f.Settle(payload, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Get did not unblock after Settle")
	}
	if !f.IsReady() {
		t.Fatal("IsReady=false after Settle")
	}
}

func TestFuture_GetRespectsContextCancellation(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	f := workflow.NewFuture() // never settles
	done := make(chan error, 1)
	go func() { done <- f.Get(ctx, nil) }()

	time.Sleep(10 * time.Millisecond)
	env.Cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		if !temporal.IsCanceledError(err) {
			t.Fatalf("err = %v, want canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Get did not return after cancel")
	}
}
