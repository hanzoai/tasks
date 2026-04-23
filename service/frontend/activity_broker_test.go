// Copyright (c) Hanzo AI Inc 2026.

package frontend

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestActivityBroker_RegisterWaitComplete(t *testing.T) {
	b := newActivityBroker()
	token := b.Register("act-1", 5*time.Second)
	if !bytes.HasPrefix(token, []byte(brokerTokenPrefix)) {
		t.Fatalf("expected broker token prefix, got %q", token)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var (
		ready   bool
		result  []byte
		failure []byte
	)
	go func() {
		defer wg.Done()
		ready, result, failure = b.Wait("act-1", 2*time.Second)
	}()

	// Give the waiter a moment to block on the channel.
	time.Sleep(10 * time.Millisecond)
	if !b.Complete(token, []byte("hello")) {
		t.Fatalf("Complete returned false for broker token")
	}
	wg.Wait()

	if !ready {
		t.Fatalf("expected ready=true")
	}
	if string(result) != "hello" {
		t.Fatalf("expected result=hello, got %q", result)
	}
	if failure != nil {
		t.Fatalf("expected failure=nil, got %q", failure)
	}
}

func TestActivityBroker_RegisterWaitFail(t *testing.T) {
	b := newActivityBroker()
	token := b.Register("act-2", 5*time.Second)

	done := make(chan struct{})
	var (
		ready   bool
		result  []byte
		failure []byte
	)
	go func() {
		defer close(done)
		ready, result, failure = b.Wait("act-2", 2*time.Second)
	}()
	time.Sleep(10 * time.Millisecond)
	if !b.Fail(token, []byte("boom")) {
		t.Fatalf("Fail returned false for broker token")
	}
	<-done

	if !ready {
		t.Fatalf("expected ready=true")
	}
	if result != nil {
		t.Fatalf("expected result=nil, got %q", result)
	}
	if string(failure) != "boom" {
		t.Fatalf("expected failure=boom, got %q", failure)
	}
}

func TestActivityBroker_WaitTimeout(t *testing.T) {
	b := newActivityBroker()
	b.Register("act-3", 5*time.Second)
	start := time.Now()
	ready, _, _ := b.Wait("act-3", 50*time.Millisecond)
	if ready {
		t.Fatalf("expected ready=false on timeout")
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("expected wait to block at least 40ms, got %v", elapsed)
	}
}

func TestActivityBroker_WaitZeroSinglePoll(t *testing.T) {
	b := newActivityBroker()
	b.Register("act-4", 5*time.Second)
	start := time.Now()
	ready, _, _ := b.Wait("act-4", 0)
	if ready {
		t.Fatalf("expected ready=false on single-poll with no result")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Millisecond {
		t.Fatalf("single-poll should return immediately, took %v", elapsed)
	}
}

func TestActivityBroker_WaitZeroSettled(t *testing.T) {
	b := newActivityBroker()
	token := b.Register("act-5", 5*time.Second)
	if !b.Complete(token, []byte("done")) {
		t.Fatal("Complete failed")
	}
	ready, result, _ := b.Wait("act-5", 0)
	if !ready {
		t.Fatalf("expected ready=true for already-settled entry")
	}
	if string(result) != "done" {
		t.Fatalf("expected result=done, got %q", result)
	}
}

func TestActivityBroker_WaitUnknownID(t *testing.T) {
	b := newActivityBroker()
	ready, _, _ := b.Wait("never-registered", time.Millisecond)
	if ready {
		t.Fatalf("expected ready=false for unknown id")
	}
}

func TestActivityBroker_DeliverUnknownToken(t *testing.T) {
	b := newActivityBroker()
	if b.Complete([]byte("not-a-broker-token"), nil) {
		t.Fatalf("Complete should return false for non-broker token")
	}
	if b.Fail([]byte("zapact:unknown-id"), nil) {
		t.Fatalf("Fail should return false for unregistered id")
	}
}

func TestActivityBroker_DoubleDeliverFirstWins(t *testing.T) {
	b := newActivityBroker()
	token := b.Register("act-6", 5*time.Second)
	if !b.Complete(token, []byte("first")) {
		t.Fatal("first Complete failed")
	}
	// Second delivery still reports true (broker-owned) but the outcome
	// is unchanged.
	if !b.Complete(token, []byte("second")) {
		t.Fatal("second Complete should still report broker-owned")
	}
	ready, result, _ := b.Wait("act-6", 0)
	if !ready || string(result) != "first" {
		t.Fatalf("first-write-wins violated: ready=%v result=%q", ready, result)
	}
}

func TestActivityBroker_ExtractID(t *testing.T) {
	b := newActivityBroker()
	if id := b.ExtractID([]byte("zapact:abc/def/1")); id != "abc/def/1" {
		t.Fatalf("ExtractID bad: %q", id)
	}
	if id := b.ExtractID([]byte("raw-token")); id != "" {
		t.Fatalf("ExtractID should reject non-broker token, got %q", id)
	}
}

func TestActivityBroker_SweepExpired(t *testing.T) {
	b := newActivityBroker()
	fixed := time.Now()
	b.now = func() time.Time { return fixed }
	b.Register("act-7", time.Second)
	// Advance clock past the TTL.
	b.now = func() time.Time { return fixed.Add(2 * time.Second) }
	b.sweepExpired()
	b.mu.Lock()
	_, still := b.entries["act-7"]
	b.mu.Unlock()
	if still {
		t.Fatalf("expected expired entry to be swept")
	}
}
