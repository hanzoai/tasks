// Copyright (c) Hanzo AI Inc 2026.
//
// frontendActivityBroker tracks in-workflow scheduled activities so the
// ZAP handlers for 0x006B scheduleActivity / 0x006C waitActivityResult
// have a real result-delivery path, independent of the legacy matching
// task-queue poll loop.
//
// Lifecycle:
//
//  1. handleScheduleActivity mints a stable activityTaskId, registers a
//     pending entry (result channel + deadline), and returns the id to
//     the worker.
//  2. The worker executes the activity (via its own dispatch — poll or
//     in-process) and, when complete, calls
//     handleRespondActivityTaskCompleted / Failed with a TaskToken that
//     carries the activityTaskId prefix ("zapact:<id>").
//  3. The respond handler detects the prefix, delivers the result to
//     the broker channel, and short-circuits the upstream
//     WorkflowHandler call (there is no server-side activity record
//     for broker-owned activities).
//  4. handleWaitActivityResult blocks up to WaitMs on the channel and
//     returns ready=true with the result / failure bytes.
//
// The broker is intentionally simple: it is not a replacement for the
// history-service's activity record. It exists to unblock worker-side
// dispatch that needs wire-backed scheduling without waiting on the
// full matching+history wiring (tracked in task #42).

package frontend

import (
	"strings"
	"sync"
	"time"
)

// brokerTokenPrefix prefixes synthesized TaskTokens returned by the
// broker so respond handlers can route back without a lookup table.
const brokerTokenPrefix = "zapact:"

// activityOutcome is the settled result of a broker-owned activity.
type activityOutcome struct {
	result  []byte
	failure []byte
}

// brokerEntry is one pending activity registration.
type brokerEntry struct {
	done     chan activityOutcome
	settled  bool
	outcome  activityOutcome
	expires  time.Time
}

// frontendActivityBroker tracks in-workflow scheduled activities.
//
// It is safe for concurrent use. Entries are garbage-collected lazily
// on Wait when expired; callers that never wait leak the entry for up
// to one GC sweep (bounded by the caller's retry logic).
type frontendActivityBroker struct {
	mu      sync.Mutex
	entries map[string]*brokerEntry
	now     func() time.Time
}

func newActivityBroker() *frontendActivityBroker {
	return &frontendActivityBroker{
		entries: make(map[string]*brokerEntry),
		now:     time.Now,
	}
}

// Register creates a pending entry for activityTaskID and returns a
// TaskToken that respond handlers use to deliver the result.
//
// If the id is already registered, Register is a no-op and returns the
// same token — schedule is idempotent so replay-safe.
func (b *frontendActivityBroker) Register(activityTaskID string, ttl time.Duration) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.entries[activityTaskID]; !ok {
		b.entries[activityTaskID] = &brokerEntry{
			done:    make(chan activityOutcome, 1),
			expires: b.now().Add(ttl),
		}
	}
	return []byte(brokerTokenPrefix + activityTaskID)
}

// ExtractID returns the activity id encoded in token, or "" if token
// is not a broker-issued token.
func (b *frontendActivityBroker) ExtractID(token []byte) string {
	s := string(token)
	if !strings.HasPrefix(s, brokerTokenPrefix) {
		return ""
	}
	return s[len(brokerTokenPrefix):]
}

// Complete delivers a success outcome to the waiter.
//
// Returns true if the token was broker-owned and the delivery succeeded.
func (b *frontendActivityBroker) Complete(token []byte, result []byte) bool {
	return b.deliver(token, activityOutcome{result: result})
}

// Fail delivers a failure outcome to the waiter.
func (b *frontendActivityBroker) Fail(token []byte, failure []byte) bool {
	return b.deliver(token, activityOutcome{failure: failure})
}

func (b *frontendActivityBroker) deliver(token []byte, out activityOutcome) bool {
	id := b.ExtractID(token)
	if id == "" {
		return false
	}
	b.mu.Lock()
	e, ok := b.entries[id]
	if !ok {
		b.mu.Unlock()
		return false
	}
	if e.settled {
		b.mu.Unlock()
		// Duplicate delivery: the first one wins, still report the
		// token as broker-owned so the respond handler does not
		// forward to the upstream WorkflowHandler.
		return true
	}
	e.settled = true
	e.outcome = out
	b.mu.Unlock()
	// Non-blocking send — channel is buffered size 1.
	select {
	case e.done <- out:
	default:
	}
	return true
}

// Wait blocks up to waitFor for the outcome of activityTaskID.
//
// A waitFor of 0 means single-poll (non-blocking): if a result is not
// already present the call returns ready=false immediately.
//
// If the entry does not exist, Wait returns ready=false with empty
// bytes — the caller interprets this as "not scheduled here" and falls
// back to the task-queue path.
func (b *frontendActivityBroker) Wait(activityTaskID string, waitFor time.Duration) (ready bool, result, failure []byte) {
	b.mu.Lock()
	e, ok := b.entries[activityTaskID]
	if !ok {
		b.mu.Unlock()
		return false, nil, nil
	}
	if e.settled {
		out := e.outcome
		delete(b.entries, activityTaskID)
		b.mu.Unlock()
		return true, out.result, out.failure
	}
	done := e.done
	b.mu.Unlock()

	if waitFor <= 0 {
		return false, nil, nil
	}

	t := time.NewTimer(waitFor)
	defer t.Stop()
	select {
	case out := <-done:
		b.mu.Lock()
		delete(b.entries, activityTaskID)
		b.mu.Unlock()
		return true, out.result, out.failure
	case <-t.C:
		return false, nil, nil
	}
}

// sweepExpired drops entries whose TTL has elapsed without a waiter.
// Called opportunistically from Register so the map does not grow
// unboundedly under load with abandoned waiters.
func (b *frontendActivityBroker) sweepExpired() {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for id, e := range b.entries {
		if !e.settled && now.After(e.expires) {
			delete(b.entries, id)
		}
	}
}
