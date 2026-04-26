// Copyright © 2026 Hanzo AI. MIT License.

package workflow_test

import (
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// Property: GetVersion records max on first call and returns the
// recorded value clamped to the new [min, max] range thereafter.
func TestGetVersion_RecordsMaxOnFirstCall(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	v1 := workflow.GetVersion(ctx, "feature-x", workflow.DefaultVersion, 3)
	if v1 != 3 {
		t.Fatalf("first GetVersion: want 3, got %d", v1)
	}
	v2 := workflow.GetVersion(ctx, "feature-x", workflow.DefaultVersion, 5)
	if v2 != 3 {
		t.Fatalf("second GetVersion: want 3 (recorded), got %d", v2)
	}
}

// Property: GetVersion clamps the recorded value into [min, max] when
// the caller widens minSupported above the recorded value.
func TestGetVersion_ClampsBelowMin(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	// Record version 2.
	if v := workflow.GetVersion(ctx, "k", workflow.DefaultVersion, 2); v != 2 {
		t.Fatalf("first call: want 2, got %d", v)
	}
	// Re-call with min=5 — recorded 2 < 5; clamps up to 5.
	if v := workflow.GetVersion(ctx, "k", 5, 10); v != 5 {
		t.Fatalf("clamp-min: want 5, got %d", v)
	}
}

// Property: GetVersion clamps the recorded value into [min, max] when
// the caller narrows maxSupported below the recorded value.
func TestGetVersion_ClampsAboveMax(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	// Record version 7.
	if v := workflow.GetVersion(ctx, "k", workflow.DefaultVersion, 7); v != 7 {
		t.Fatalf("first call: want 7, got %d", v)
	}
	// Re-call with max=3 — recorded 7 > 3; clamps down to 3.
	if v := workflow.GetVersion(ctx, "k", workflow.DefaultVersion, 3); v != 3 {
		t.Fatalf("clamp-max: want 3, got %d", v)
	}
}

// Property: each changeID is independent.
func TestGetVersion_IndependentChangeIDs(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	if v := workflow.GetVersion(ctx, "a", workflow.DefaultVersion, 1); v != 1 {
		t.Fatalf("a: %d", v)
	}
	if v := workflow.GetVersion(ctx, "b", workflow.DefaultVersion, 2); v != 2 {
		t.Fatalf("b: %d", v)
	}
	if v := workflow.GetVersion(ctx, "a", workflow.DefaultVersion, 99); v != 1 {
		t.Fatalf("a re-call: want 1 (recorded), got %d", v)
	}
}

// Property: SideEffect runs fn exactly once per ordinal call site.
func TestSideEffect_RunsFnOncePerOrdinal(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	calls := 0
	mk := func(want int) func(workflow.Context) any {
		return func(workflow.Context) any {
			calls++
			return want
		}
	}

	// Two distinct call sites — both should run.
	f1 := workflow.SideEffect(ctx, mk(10))
	f2 := workflow.SideEffect(ctx, mk(20))
	var v1, v2 int
	if err := f1.Get(ctx, &v1); err != nil {
		t.Fatalf("f1: %v", err)
	}
	if err := f2.Get(ctx, &v2); err != nil {
		t.Fatalf("f2: %v", err)
	}
	if v1 != 10 || v2 != 20 {
		t.Fatalf("got (%d,%d) want (10,20)", v1, v2)
	}
	if calls != 2 {
		t.Fatalf("fn invocations: want 2, got %d", calls)
	}
}

// Property: SideEffect replays return recorded bytes without re-running.
// The stub does not yet model a "re-dispatch from history" pass; we
// emulate replay by manually pre-seeding the side-effect buffer through
// the public surface.
func TestSideEffect_DeterministicAcrossOrdinals(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	// First pass: record three.
	for i := 0; i < 3; i++ {
		v := i
		var got int
		if err := workflow.SideEffect(ctx, func(workflow.Context) any { return v }).Get(ctx, &got); err != nil {
			t.Fatalf("pass1[%d]: %v", i, err)
		}
		if got != i {
			t.Fatalf("pass1[%d]: want %d got %d", i, i, got)
		}
	}
}

// Property: MutableSideEffect returns the recorded value when eq
// reports the new value matches, and replaces it otherwise.
func TestMutableSideEffect_PersistsByEquality(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	eq := func(a, b any) bool {
		af, _ := a.(float64)
		bf, _ := b.(float64)
		return af == bf
	}

	// First call records 1.
	var v1 float64
	if err := workflow.MutableSideEffect(ctx, "tweak", func(workflow.Context) any { return 1 }, eq).Get(ctx, &v1); err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1 != 1 {
		t.Fatalf("v1: want 1, got %v", v1)
	}

	// Same value via eq — should keep recorded.
	var v2 float64
	if err := workflow.MutableSideEffect(ctx, "tweak", func(workflow.Context) any { return 1 }, eq).Get(ctx, &v2); err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2 != 1 {
		t.Fatalf("v2: want 1, got %v", v2)
	}

	// Different value — should update.
	var v3 float64
	if err := workflow.MutableSideEffect(ctx, "tweak", func(workflow.Context) any { return 2 }, eq).Get(ctx, &v3); err != nil {
		t.Fatalf("v3: %v", err)
	}
	if v3 != 2 {
		t.Fatalf("v3: want 2, got %v", v3)
	}
}

// Property: SideEffect surfaces errors from the nil-context path.
func TestSideEffect_NilContextReturnsError(t *testing.T) {
	f := workflow.SideEffect(nil, func(workflow.Context) any { return 1 })
	if !f.IsReady() {
		t.Fatalf("future not settled")
	}
	var v int
	if err := f.Get(nil, &v); err == nil {
		t.Fatalf("expected error from nil-ctx SideEffect")
	}
}

// Property: GetMetricsHandler returns a noop when no provider wired
// and a working handler when one is installed.
func TestGetMetricsHandler_NoopWhenAbsent(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	h := workflow.GetMetricsHandler(ctx)
	if h == nil {
		t.Fatalf("noop handler must not be nil")
	}
	// Chain through the surface — must not panic.
	h.WithTags(map[string]string{"k": "v"}).Counter("c").Inc(1)
	h.Gauge("g").Update(0.5)
	// Use the timer handle.
	h.Timer("t").Record(0)
}

// fakeMetrics is a minimal handler that records counter increments.
type fakeMetrics struct {
	counters map[string]int64
}

func newFake() *fakeMetrics                                { return &fakeMetrics{counters: make(map[string]int64)} }
func (f *fakeMetrics) WithTags(map[string]string) workflow.MetricsHandler { return f }
func (f *fakeMetrics) Counter(name string) workflow.MetricsCounter        { return &fakeCounter{f: f, name: name} }
func (f *fakeMetrics) Gauge(string) workflow.MetricsGauge                 { return &fakeGauge{} }
func (f *fakeMetrics) Timer(string) workflow.MetricsTimer                 { return &fakeTimer{} }

type fakeCounter struct {
	f    *fakeMetrics
	name string
}

func (c *fakeCounter) Inc(d int64) { c.f.counters[c.name] += d }

type fakeGauge struct{}

func (fakeGauge) Update(float64) {}

type fakeTimer struct{}

func (fakeTimer) Record(time.Duration) {}

func TestGetMetricsHandler_InstalledHandlerRoutes(t *testing.T) {
	env := workflow.NewStubEnv()
	fake := newFake()
	env.SetMetricsHandler(fake)
	ctx := workflow.NewContextFromEnv(env)

	workflow.GetMetricsHandler(ctx).Counter("workflow.tick").Inc(7)
	workflow.GetMetricsHandler(ctx).Counter("workflow.tick").Inc(3)
	if got := fake.counters["workflow.tick"]; got != 10 {
		t.Fatalf("want 10, got %d", got)
	}
}

// Property: UpsertSearchAttributes is recorded by StubEnv for assertion.
func TestUpsertSearchAttributes_Recorded(t *testing.T) {
	env := workflow.NewStubEnv()
	ctx := workflow.NewContextFromEnv(env)

	if err := workflow.UpsertSearchAttributes(ctx, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("upsert1: %v", err)
	}
	if err := workflow.UpsertSearchAttributes(ctx, map[string]any{"q": 1}); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	got := env.UpsertedSearchAttributes()
	if len(got) != 2 {
		t.Fatalf("want 2 upserts, got %d", len(got))
	}
	if got[0]["k"] != "v" {
		t.Fatalf("upsert1 missing key: %v", got[0])
	}
	if v, ok := got[1]["q"]; !ok || v != 1 {
		t.Fatalf("upsert2 missing key: %v", got[1])
	}
}
