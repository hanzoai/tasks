// Copyright (c) Hanzo AI Inc 2026.

package frontend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedKey is used in tests so tokens are deterministic and we can
// construct forged tokens with a different key.
var fixedKey = []byte("unit-test-broker-key-32bytes!!!!")

func testBroker() *frontendActivityBroker {
	return newActivityBrokerWithKey(fixedKey)
}

// defaultScope gives tests a sensible workflow scope.
func defaultScope() activityScope {
	return activityScope{
		Namespace:  "default",
		TaskQueue:  "tq-1",
		WorkflowID: "wf-a",
		RunID:      "run-a",
	}
}

func TestActivityBroker_RegisterWaitComplete(t *testing.T) {
	b := testBroker()
	token := b.Register("act-1", defaultScope(), 5*time.Second)
	if !bytes.HasPrefix(token, []byte(brokerTokenPrefixV2)) {
		t.Fatalf("expected v2 broker token prefix, got %q", token)
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
	b := testBroker()
	token := b.Register("act-2", defaultScope(), 5*time.Second)

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
	b := testBroker()
	b.Register("act-3", defaultScope(), 5*time.Second)
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
	b := testBroker()
	b.Register("act-4", defaultScope(), 5*time.Second)
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
	b := testBroker()
	token := b.Register("act-5", defaultScope(), 5*time.Second)
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
	b := testBroker()
	ready, _, _ := b.Wait("never-registered", time.Millisecond)
	if ready {
		t.Fatalf("expected ready=false for unknown id")
	}
}

func TestActivityBroker_DeliverUnknownToken(t *testing.T) {
	b := testBroker()
	if b.Complete([]byte("not-a-broker-token"), nil) {
		t.Fatalf("Complete should return false for non-broker token")
	}
	// A v1 token with an unregistered id should also not deliver.
	if b.Fail([]byte("zapact:unknown-id"), nil) {
		t.Fatalf("Fail should return false for unregistered id")
	}
}

func TestActivityBroker_DoubleDeliverFirstWins(t *testing.T) {
	b := testBroker()
	token := b.Register("act-6", defaultScope(), 5*time.Second)
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

func TestActivityBroker_ExtractID_V2RoundTrip(t *testing.T) {
	b := testBroker()
	token := b.Register("abc/def/1", defaultScope(), time.Minute)
	if id := b.ExtractID(token); id != "abc/def/1" {
		t.Fatalf("ExtractID round-trip bad: %q", id)
	}
	if id := b.ExtractID([]byte("raw-token")); id != "" {
		t.Fatalf("ExtractID should reject non-broker token, got %q", id)
	}
}

func TestActivityBroker_SweepExpired(t *testing.T) {
	b := testBroker()
	fixed := time.Now()
	b.now = func() time.Time { return fixed }
	b.Register("act-7", defaultScope(), time.Second)
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

// --- R2.1 security tests -------------------------------------------------

// TestActivityBroker_ForgedTokenRejected verifies that a token signed
// with a different key is rejected even if the activityTaskID and
// scope are correct.
func TestActivityBroker_ForgedTokenRejected(t *testing.T) {
	b := testBroker()
	// Legit registration so the entry exists on the victim broker.
	b.Register("forged-target", defaultScope(), time.Minute)

	attacker := newActivityBrokerWithKey([]byte("attacker-key-32bytes!!!!aaaaaaaa"))
	// Attacker signs a token with their key but claims to settle the
	// victim's activity.
	forged := attacker.mintToken("forged-target", defaultScope(), time.Now().Add(time.Minute))

	if b.Complete(forged, []byte("pwned")) {
		t.Fatalf("forged token must not complete on victim broker")
	}
	if b.Fail(forged, []byte("pwned")) {
		t.Fatalf("forged token must not fail on victim broker")
	}

	// The real entry must still be unsettled.
	b.mu.Lock()
	e, ok := b.entries["forged-target"]
	settled := ok && e.settled
	b.mu.Unlock()
	if settled {
		t.Fatalf("victim entry was settled by forged token")
	}
}

// TestActivityBroker_ExpiredTokenRejected verifies that a
// correctly-signed token past its exp is rejected.
func TestActivityBroker_ExpiredTokenRejected(t *testing.T) {
	b := testBroker()
	fixed := time.Now()
	b.now = func() time.Time { return fixed }

	b.Register("exp-target", defaultScope(), time.Second)
	token := b.mintToken("exp-target", defaultScope(), fixed.Add(time.Second))

	// Advance clock past exp.
	b.now = func() time.Time { return fixed.Add(2 * time.Second) }

	if b.Complete(token, []byte("late")) {
		t.Fatalf("expired token must not complete")
	}
}

// TestActivityBroker_CrossWorkflowTokenRejected verifies that a valid
// token minted for workflow A cannot settle activity B scheduled by
// workflow B — even when the attacker knows B's activityTaskID.
func TestActivityBroker_CrossWorkflowTokenRejected(t *testing.T) {
	b := testBroker()
	// Target: workflow A registers activity "shared-id".
	b.Register("shared-id", activityScope{Namespace: "default", TaskQueue: "tq-A", WorkflowID: "wf-A"}, time.Minute)

	// Attacker: knows "shared-id" but controls workflow B. They mint a
	// well-signed token (they share the broker, i.e. same process)
	// claiming scope B.
	badToken := b.mintToken("shared-id", activityScope{Namespace: "default", TaskQueue: "tq-B", WorkflowID: "wf-B"}, time.Now().Add(time.Minute))

	if b.Complete(badToken, []byte("pwned")) {
		t.Fatalf("cross-workflow token must not complete")
	}
}

// TestActivityBroker_HappyPath end-to-end verifies verifyToken ok +
// Complete delivers.
func TestActivityBroker_HappyPath(t *testing.T) {
	b := testBroker()
	token := b.Register("happy", defaultScope(), time.Minute)

	id, err := b.verifyToken(token)
	if err != nil {
		t.Fatalf("verifyToken(happy) err = %v", err)
	}
	if id != "happy" {
		t.Fatalf("verifyToken id = %q, want happy", id)
	}
	if !b.Complete(token, []byte("ok")) {
		t.Fatalf("Complete(happy) = false")
	}
	ready, result, _ := b.Wait("happy", 0)
	if !ready || string(result) != "ok" {
		t.Fatalf("happy wait: ready=%v result=%q", ready, result)
	}
}

// TestActivityBroker_MalformedTokens exercises the token parser.
func TestActivityBroker_MalformedTokens(t *testing.T) {
	b := testBroker()
	cases := [][]byte{
		[]byte("zapact2:"),                         // empty body
		[]byte("zapact2:nodot"),                    // no dot sep
		[]byte("zapact2:!!!!.!!!!"),                // bad base64
		[]byte("zapact2:" + string(garbagePayload())), // bad JSON after decode
		nil,
		[]byte(""),
	}
	for _, tok := range cases {
		if _, err := b.verifyToken(tok); err == nil {
			t.Fatalf("verifyToken(%q) expected error", tok)
		}
	}
}

func garbagePayload() []byte {
	raw := []byte("not-valid-json")
	sig := []byte("sig")
	enc := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(sig)
	return []byte(enc)
}

// TestActivityBroker_LoadTokenKey_EnvBase64 verifies the env-var path.
func TestActivityBroker_LoadTokenKey_EnvBase64(t *testing.T) {
	raw := []byte("pinned-env-key-32-bytes-long!!!!")
	t.Setenv("TASKS_ACTIVITY_TOKEN_KEY", base64.StdEncoding.EncodeToString(raw))
	got := loadOrGenerateTokenKey(nil)
	if !bytes.Equal(got, raw) {
		t.Fatalf("loadOrGenerateTokenKey returned %x, want %x", got, raw)
	}
}

// TestActivityBroker_LoadTokenKey_EnvUnset falls back to a random key.
func TestActivityBroker_LoadTokenKey_EnvUnset(t *testing.T) {
	t.Setenv("TASKS_ACTIVITY_TOKEN_KEY", "")
	k := loadOrGenerateTokenKey(nil)
	if len(k) != 32 {
		t.Fatalf("expected 32-byte random key, got %d", len(k))
	}
}

// --- R2.2 ctx-deadline tests -------------------------------------------

// TestActivityBroker_WaitCtxCancelled verifies that a canceled context
// unblocks WaitCtx before WaitMs elapses and returns ctx.Err wrapped.
func TestActivityBroker_WaitCtxCancelled(t *testing.T) {
	b := testBroker()
	b.Register("ctx-act", defaultScope(), 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var (
		ready bool
		err   error
	)
	go func() {
		defer close(done)
		ready, _, _, err = b.WaitCtx(ctx, "ctx-act", 5*time.Second)
	}()

	// Let the waiter park, then cancel.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("WaitCtx did not honor ctx cancellation within 500ms")
	}

	if ready {
		t.Fatalf("expected ready=false on ctx cancel")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped context.Canceled, got %v", err)
	}
}

// TestActivityBroker_WaitCtxDeadline verifies ctx deadline short-circuits
// WaitMs.
func TestActivityBroker_WaitCtxDeadline(t *testing.T) {
	b := testBroker()
	b.Register("dl-act", defaultScope(), 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	ready, _, _, err := b.WaitCtx(ctx, "dl-act", 5*time.Second)
	elapsed := time.Since(start)

	if ready {
		t.Fatalf("expected ready=false on ctx deadline")
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected wrapped DeadlineExceeded, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("WaitCtx blocked past ctx deadline: %v", elapsed)
	}
}

// TestActivityBroker_WaitCtxAlreadyCancelled short-circuits without
// parking when ctx is already done.
func TestActivityBroker_WaitCtxAlreadyCancelled(t *testing.T) {
	b := testBroker()
	b.Register("prc-act", defaultScope(), 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ready, _, _, err := b.WaitCtx(ctx, "prc-act", 5*time.Second)
	if ready {
		t.Fatal("expected ready=false")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected wrapped Canceled, got %v", err)
	}
}

// TestActivityBroker_WaitCtxSettles still delivers when ctx stays live.
func TestActivityBroker_WaitCtxSettles(t *testing.T) {
	b := testBroker()
	token := b.Register("ok-act", defaultScope(), 10*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	var (
		ready bool
		res   []byte
		err   error
	)
	go func() {
		defer close(done)
		ready, res, _, err = b.WaitCtx(ctx, "ok-act", time.Second)
	}()

	time.Sleep(20 * time.Millisecond)
	if !b.Complete(token, []byte("ctx-ok")) {
		t.Fatal("Complete failed")
	}
	<-done

	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !ready || string(res) != "ctx-ok" {
		t.Fatalf("unexpected outcome ready=%v res=%q", ready, res)
	}
}

// TestActivityBroker_TokenClaimsVisible decodes a token and ensures the
// expected claims are present — so downstream observability can parse
// without reverse-engineering the format.
func TestActivityBroker_TokenClaimsVisible(t *testing.T) {
	b := testBroker()
	sc := activityScope{Namespace: "ns1", TaskQueue: "tq1", WorkflowID: "w1", RunID: "r1"}
	exp := time.Now().Add(time.Minute)
	token := b.mintToken("aid1", sc, exp)

	rest := strings.TrimPrefix(string(token), brokerTokenPrefixV2)
	parts := strings.SplitN(rest, ".", 2)
	if len(parts) != 2 {
		t.Fatalf("expected payload.sig format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var c tokenClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if c.NS != "ns1" || c.TQ != "tq1" || c.WF != "w1" || c.RID != "r1" || c.AID != "aid1" {
		t.Fatalf("claims mismatch: %+v", c)
	}
	if c.Exp <= 0 {
		t.Fatalf("exp not set: %d", c.Exp)
	}
}
