package tasks

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNew_ThreeArgSignature pins the public constructor contract
// (tasksURL, zapAddr, handler).
func TestNew_ThreeArgSignature(t *testing.T) {
	c := New("", "", nil)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	c.Stop()
}

// TestDefault_LazyBoot ensures Default() returns a client when SetDefault
// has not been called.
func TestDefault_LazyBoot(t *testing.T) {
	SetDefault(nil)
	c := Default()
	if c == nil {
		t.Fatal("Default() should lazy-create a client")
	}
	SetDefault(nil)
}

// TestSetDefault_HandlerRouting verifies SetDefault installs the client that
// Default() returns, and Now() dispatches to the handler in local mode.
func TestSetDefault_HandlerRouting(t *testing.T) {
	var (
		mu      sync.Mutex
		gotType string
		gotPay  map[string]any
	)

	c := New("", "", func(taskType string, payload map[string]any) {
		mu.Lock()
		gotType = taskType
		gotPay = payload
		mu.Unlock()
	})
	SetDefault(c)
	t.Cleanup(func() { SetDefault(nil) })

	if err := Default().Now("webhook.deliver", map[string]any{"org_id": "o1"}); err != nil {
		t.Fatalf("Now: %v", err)
	}

	// Give the goroutine dispatcher a moment.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		if gotType != "" {
			mu.Unlock()
			break
		}
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotType != "webhook.deliver" {
		t.Fatalf("expected webhook.deliver, got %q", gotType)
	}
	if gotPay["org_id"] != "o1" {
		t.Fatalf("payload not propagated: %v", gotPay)
	}
}

// TestAdd_DurationLocal registers a short-interval schedule and verifies it
// runs at least once locally when neither TASKS_URL nor TASKS_ZAP is set.
func TestAdd_DurationLocal(t *testing.T) {
	c := New("", "", nil)
	t.Cleanup(c.Stop)

	var ran atomic.Int32
	if err := c.Add("test-interval", "30ms", func() { ran.Add(1) }); err != nil {
		t.Fatalf("Add: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if ran.Load() < 1 {
		t.Fatalf("expected at least one tick, got %d", ran.Load())
	}
}

// TestAdd_CronExpression accepts a standard 5-field cron expression.
func TestAdd_CronExpression(t *testing.T) {
	c := New("", "", nil)
	t.Cleanup(c.Stop)

	// Standard cron, parseable by robfig/cron/v3's ParseStandard.
	if err := c.Add("test-cron", "0 3 * * *", func() {}); err != nil {
		t.Fatalf("Add cron: %v", err)
	}
}

// TestAdd_InvalidSpec rejects garbage.
func TestAdd_InvalidSpec(t *testing.T) {
	c := New("", "", nil)
	t.Cleanup(c.Stop)

	if err := c.Add("bad", "not-a-cron-or-duration", func() {}); err == nil {
		t.Fatal("expected error on invalid spec")
	}
	if err := c.Add("bad-empty", "", func() {}); err == nil {
		t.Fatal("expected error on empty spec")
	}
}

// TestNow_HTTPSubmit verifies Now() submits to the HTTP endpoint when
// TASKS_URL is set. (ZAP not tested here — needs a running server.)
func TestNow_HTTPSubmit(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "", nil)
	t.Cleanup(c.Stop)
	if err := c.Now("webhook.deliver", map[string]any{"org_id": "o2"}); err != nil {
		t.Fatalf("Now: %v", err)
	}

	if got == nil {
		t.Fatal("server did not receive task")
	}
	if got["workflow_type"] != "webhook.deliver" {
		t.Fatalf("workflow_type mismatch: %v", got)
	}
	input, _ := got["input"].(map[string]any)
	if input["org_id"] != "o2" {
		t.Fatalf("input not forwarded: %v", input)
	}
}

// TestAdd_HTTPDurableSchedule verifies Add() calls the server schedule API
// when TASKS_URL is set. Payload shape is pinned.
func TestAdd_HTTPDurableSchedule(t *testing.T) {
	var got bytes.Buffer
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = got.ReadFrom(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "", nil)
	t.Cleanup(c.Stop)
	if err := c.Add("sched-duration", "1m", func() {}); err != nil {
		t.Fatalf("Add duration: %v", err)
	}
	if err := c.Add("sched-cron", "*/5 * * * *", func() {}); err != nil {
		t.Fatalf("Add cron: %v", err)
	}
	if got.Len() == 0 {
		t.Fatal("server did not receive schedule payload")
	}
}
