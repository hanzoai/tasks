package activity

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/luxfi/log"
)

// newTestScope constructs a Scope with a logger that writes into buf and has
// the standard activity fields pre-bound.
func newTestScope(buf *bytes.Buffer) *Scope {
	logger := log.NewWriter(buf).With().
		Str("activity_id", "act-123").
		Str("activity_type", "ExecuteTask").
		Str("workflow_id", "wf-abc").
		Int32("attempt", 2).
		Logger()
	sched := time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC)
	started := sched.Add(500 * time.Millisecond)
	return &Scope{
		Info: Info{
			TaskToken:         []byte{0xde, 0xad, 0xbe, 0xef},
			WorkflowExecution: WorkflowExecution{WorkflowID: "wf-abc", RunID: "run-xyz"},
			ActivityID:        "act-123",
			ActivityType:      "ExecuteTask",
			TaskQueue:         "tq-main",
			Attempt:           2,
			ScheduledTime:     sched,
			StartedTime:       started,
		},
		Logger: logger,
	}
}

func TestGetInfo_RoundTrip(t *testing.T) {
	scope := newTestScope(&bytes.Buffer{})
	ctx := NewContext(context.Background(), scope)

	info := GetInfo(ctx)
	if info.ActivityID != "act-123" {
		t.Errorf("ActivityID = %q, want %q", info.ActivityID, "act-123")
	}
	if info.ActivityType != "ExecuteTask" {
		t.Errorf("ActivityType = %q, want %q", info.ActivityType, "ExecuteTask")
	}
	if info.WorkflowExecution.WorkflowID != "wf-abc" {
		t.Errorf("WorkflowID = %q, want %q", info.WorkflowExecution.WorkflowID, "wf-abc")
	}
	if info.WorkflowExecution.RunID != "run-xyz" {
		t.Errorf("RunID = %q, want %q", info.WorkflowExecution.RunID, "run-xyz")
	}
	if info.TaskQueue != "tq-main" {
		t.Errorf("TaskQueue = %q, want %q", info.TaskQueue, "tq-main")
	}
	if info.Attempt != 2 {
		t.Errorf("Attempt = %d, want 2", info.Attempt)
	}
	if !bytes.Equal(info.TaskToken, []byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Errorf("TaskToken = %x, want deadbeef", info.TaskToken)
	}
	if info.ScheduledTime.IsZero() || info.StartedTime.IsZero() {
		t.Errorf("times should be non-zero: scheduled=%v started=%v",
			info.ScheduledTime, info.StartedTime)
	}
	if !info.StartedTime.After(info.ScheduledTime) {
		t.Errorf("StartedTime (%v) should be after ScheduledTime (%v)",
			info.StartedTime, info.ScheduledTime)
	}
}

func TestGetInfo_TaskTokenIsolated(t *testing.T) {
	scope := newTestScope(&bytes.Buffer{})
	ctx := NewContext(context.Background(), scope)

	info := GetInfo(ctx)
	// Mutate the returned token; scope must be unaffected.
	info.TaskToken[0] = 0x00

	fresh := GetInfo(ctx)
	if fresh.TaskToken[0] != 0xde {
		t.Errorf("scope TaskToken was mutated through returned Info: got %x", fresh.TaskToken)
	}
}

func TestGetInfo_NoScope(t *testing.T) {
	info := GetInfo(context.Background())
	if info.ActivityID != "" || info.Attempt != 0 || info.TaskToken != nil {
		t.Errorf("expected zero Info, got %+v", info)
	}
}

func TestFromContext(t *testing.T) {
	scope := newTestScope(&bytes.Buffer{})
	ctx := NewContext(context.Background(), scope)

	info, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext ok = false, want true")
	}
	if info.ActivityID != "act-123" {
		t.Errorf("FromContext ActivityID = %q, want %q", info.ActivityID, "act-123")
	}

	// Without scope.
	_, ok = FromContext(context.Background())
	if ok {
		t.Error("FromContext ok = true on bare context, want false")
	}

	// Nil ctx is tolerated.
	_, ok = FromContext(nil)
	if ok {
		t.Error("FromContext ok = true on nil ctx, want false")
	}
}

func TestNewContext_NilScope(t *testing.T) {
	base := context.Background()
	got := NewContext(base, nil)
	if got != base {
		t.Error("NewContext(_, nil) should return the input context unchanged")
	}
}

func TestGetLogger_ReturnsBoundLogger(t *testing.T) {
	var buf bytes.Buffer
	scope := newTestScope(&buf)
	ctx := NewContext(context.Background(), scope)

	logger := GetLogger(ctx)
	if logger == nil {
		t.Fatal("GetLogger returned nil")
	}
	logger.Info("heartbeat ok", "phase", "exec")

	out := buf.String()
	if !strings.Contains(out, `"activity_id":"act-123"`) {
		t.Errorf("logger output missing activity_id field: %s", out)
	}
	if !strings.Contains(out, `"activity_type":"ExecuteTask"`) {
		t.Errorf("logger output missing activity_type field: %s", out)
	}
	if !strings.Contains(out, `"workflow_id":"wf-abc"`) {
		t.Errorf("logger output missing workflow_id field: %s", out)
	}
	if !strings.Contains(out, `"attempt":2`) {
		t.Errorf("logger output missing attempt field: %s", out)
	}
	if !strings.Contains(out, `"phase":"exec"`) {
		t.Errorf("logger output missing per-call field phase: %s", out)
	}
	if !strings.Contains(out, `"message":"heartbeat ok"`) {
		t.Errorf("logger output missing message: %s", out)
	}
}

func TestGetLogger_NoopWhenMissing(t *testing.T) {
	// No scope at all.
	if logger := GetLogger(context.Background()); logger == nil || !logger.IsZero() {
		t.Errorf("GetLogger without scope should return Noop, got IsZero=%v", logger != nil && logger.IsZero())
	}

	// Scope present, logger nil.
	scope := &Scope{Info: Info{ActivityID: "x"}}
	ctx := NewContext(context.Background(), scope)
	if logger := GetLogger(ctx); logger == nil || !logger.IsZero() {
		t.Errorf("GetLogger with nil scope logger should return Noop, got IsZero=%v", logger != nil && logger.IsZero())
	}
}

func TestRecordHeartbeat_CollectsDetails(t *testing.T) {
	scope := newTestScope(&bytes.Buffer{})
	ctx := NewContext(context.Background(), scope)

	RecordHeartbeat(ctx, "progress", 42)
	RecordHeartbeat(ctx) // empty details allowed
	RecordHeartbeat(ctx, "done")

	hb := scope.Heartbeats()
	if len(hb) != 3 {
		t.Fatalf("Heartbeats len = %d, want 3", len(hb))
	}
	if len(hb[0]) != 2 || hb[0][0] != "progress" || hb[0][1] != 42 {
		t.Errorf("first heartbeat = %+v, want [progress 42]", hb[0])
	}
	if len(hb[1]) != 0 {
		t.Errorf("second heartbeat should be empty, got %+v", hb[1])
	}
	if len(hb[2]) != 1 || hb[2][0] != "done" {
		t.Errorf("third heartbeat = %+v, want [done]", hb[2])
	}
}

func TestRecordHeartbeat_ForwardsToSink(t *testing.T) {
	var (
		sinkMu   sync.Mutex
		sinkCall [][]interface{}
	)
	scope := newTestScope(&bytes.Buffer{})
	scope.HeartbeatSink = func(details ...interface{}) {
		sinkMu.Lock()
		defer sinkMu.Unlock()
		dup := make([]interface{}, len(details))
		copy(dup, details)
		sinkCall = append(sinkCall, dup)
	}
	ctx := NewContext(context.Background(), scope)

	RecordHeartbeat(ctx, "tick", int64(1))
	RecordHeartbeat(ctx, "tick", int64(2))

	sinkMu.Lock()
	defer sinkMu.Unlock()
	if len(sinkCall) != 2 {
		t.Fatalf("sink invocations = %d, want 2", len(sinkCall))
	}
	if sinkCall[0][0] != "tick" || sinkCall[0][1] != int64(1) {
		t.Errorf("sink[0] = %+v, want [tick 1]", sinkCall[0])
	}
	if sinkCall[1][1] != int64(2) {
		t.Errorf("sink[1][1] = %v, want 2", sinkCall[1][1])
	}

	// Scope's internal collection should also have both.
	if hb := scope.Heartbeats(); len(hb) != 2 {
		t.Errorf("scope Heartbeats len = %d, want 2", len(hb))
	}
}

func TestRecordHeartbeat_NoScope(t *testing.T) {
	// Must not panic.
	RecordHeartbeat(context.Background(), "ignored")
	RecordHeartbeat(context.Background())
}

func TestRecordHeartbeat_DefensiveCopy(t *testing.T) {
	scope := newTestScope(&bytes.Buffer{})
	ctx := NewContext(context.Background(), scope)

	details := []interface{}{"phase", "start"}
	RecordHeartbeat(ctx, details...)

	// Mutate caller's slice; captured entry must be unaffected.
	details[0] = "tampered"
	details[1] = "boom"

	hb := scope.Heartbeats()
	if hb[0][0] != "phase" || hb[0][1] != "start" {
		t.Errorf("scope captured a mutable reference: %+v", hb[0])
	}
}

func TestHeartbeats_IndependentCopy(t *testing.T) {
	scope := newTestScope(&bytes.Buffer{})
	ctx := NewContext(context.Background(), scope)
	RecordHeartbeat(ctx, "a")

	hb := scope.Heartbeats()
	hb[0][0] = "b"

	// Fetch again; must still be "a".
	hb2 := scope.Heartbeats()
	if hb2[0][0] != "a" {
		t.Errorf("Heartbeats returned reference to internal state: got %v", hb2[0][0])
	}
}

func TestRecordHeartbeat_Concurrent(t *testing.T) {
	scope := newTestScope(&bytes.Buffer{})
	ctx := NewContext(context.Background(), scope)

	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			RecordHeartbeat(ctx, "i", i)
		}()
	}
	wg.Wait()

	if got := len(scope.Heartbeats()); got != N {
		t.Errorf("concurrent Heartbeats len = %d, want %d", got, N)
	}
}

func TestScope_NilSafe(t *testing.T) {
	var s *Scope
	if hb := s.Heartbeats(); hb != nil {
		t.Errorf("nil Scope.Heartbeats() = %v, want nil", hb)
	}
	s.recordHeartbeat("ignored") // must not panic
}
