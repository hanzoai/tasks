package tasks

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pickPort returns an OS-assigned loopback port. There is a small race
// between close and ZAP bind; acceptable for a test.
func pickPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// bootServer starts an embedded Tasks server on a free ZAP port and
// returns it plus its loopback address.
func bootServer(t *testing.T) (*Embedded, string) {
	t.Helper()
	ctx := context.Background()
	port := pickPort(t)
	emb, err := Embed(ctx, EmbedConfig{Address: fmt.Sprintf(":%d", port)})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	t.Cleanup(func() { _ = emb.Stop(context.Background()) })
	return emb, net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

// TestNew_TwoArgSignature pins the public constructor contract
// (zapAddr, handler).
func TestNew_TwoArgSignature(t *testing.T) {
	c := New("", nil)
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

	c := New("", func(taskType string, payload map[string]any) {
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
// runs at least once locally when no server is configured.
func TestAdd_DurationLocal(t *testing.T) {
	c := New("", nil)
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
	c := New("", nil)
	t.Cleanup(c.Stop)

	// Standard cron, parseable by robfig/cron/v3's ParseStandard.
	if err := c.Add("test-cron", "0 3 * * *", func() {}); err != nil {
		t.Fatalf("Add cron: %v", err)
	}
}

// TestAdd_InvalidSpec rejects garbage.
func TestAdd_InvalidSpec(t *testing.T) {
	c := New("", nil)
	t.Cleanup(c.Stop)

	if err := c.Add("bad", "not-a-cron-or-duration", func() {}); err == nil {
		t.Fatal("expected error on invalid spec")
	}
	if err := c.Add("bad-empty", "", func() {}); err == nil {
		t.Fatal("expected error on empty spec")
	}
}

// TestNow_ZAPSubmit verifies Now() starts a durable workflow over ZAP when
// zapAddr points at a running server. The task type becomes the workflow
// type; the payload becomes the workflow input.
func TestNow_ZAPSubmit(t *testing.T) {
	emb, addr := bootServer(t)

	c := New(addr, nil)
	t.Cleanup(c.Stop)
	if err := c.Now("webhook.deliver", map[string]any{"org_id": "o2"}); err != nil {
		t.Fatalf("Now: %v", err)
	}

	// The submit is a synchronous request/response; the workflow record
	// is persisted before Now returns. Poll briefly to absorb any wire
	// scheduling jitter.
	if !eventually(t, 5*time.Second, func() bool {
		rows, err := emb.View(Principal{}).ListWorkflows("default")
		if err != nil {
			return false
		}
		for i := range rows {
			if rows[i].Type.Name == "webhook.deliver" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("server did not record the workflow started via ZAP submit")
	}
}

// TestAdd_ZAPDurableSchedule verifies Add() creates durable schedules over
// ZAP for both a duration (interval) and a cron expression, and that the
// server persists the spec faithfully.
func TestAdd_ZAPDurableSchedule(t *testing.T) {
	emb, addr := bootServer(t)

	c := New(addr, nil)
	t.Cleanup(c.Stop)

	if err := c.Add("sched-interval", "1m", func() {}); err != nil {
		t.Fatalf("Add duration: %v", err)
	}
	if err := c.Add("sched-cron", "*/5 * * * *", func() {}); err != nil {
		t.Fatalf("Add cron: %v", err)
	}

	// Interval schedule persisted with the interval spec.
	if !eventually(t, 5*time.Second, func() bool {
		s, ok, err := emb.View(Principal{}).DescribeSchedule("default", "sched-interval")
		if err != nil || !ok {
			return false
		}
		return len(s.Spec.Interval) == 1 && s.Spec.Interval[0].Interval == "1m0s"
	}) {
		t.Fatal("interval schedule not persisted with its spec over ZAP")
	}

	// Cron schedule persisted with the cron spec.
	if !eventually(t, 5*time.Second, func() bool {
		s, ok, err := emb.View(Principal{}).DescribeSchedule("default", "sched-cron")
		if err != nil || !ok {
			return false
		}
		return len(s.Spec.CronString) == 1 && s.Spec.CronString[0] == "*/5 * * * *"
	}) {
		t.Fatal("cron schedule not persisted with its spec over ZAP")
	}
}

// eventually polls cond until it returns true or the deadline elapses.
func eventually(t *testing.T, within time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}
