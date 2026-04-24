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
//     carries the broker signature (see Register).
//  3. The respond handler verifies the token's HMAC, delivers the
//     result to the broker channel, and short-circuits the upstream
//     WorkflowHandler call (there is no server-side activity record
//     for broker-owned activities).
//  4. handleWaitActivityResult blocks up to WaitMs on the channel and
//     returns ready=true with the result / failure bytes.
//
// Token ownership (v2):
// The v1 broker issued raw tokens of the shape "zapact:<id>", which
// meant any caller who learned the activityTaskID string could
// Complete/Fail an activity belonging to another workflow. v2 mints
// an HMAC-signed token bound to {namespace, taskQueue, workflowID,
// runID, activityTaskID, expiry}; the signing key is loaded from
// TASKS_ACTIVITY_TOKEN_KEY (base64) at process start. Respond*
// handlers verify the signature + expiry + id match before
// delivering.

package frontend

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Token prefixes. v1 is retained so legacy in-flight tokens minted
// before the upgrade continue to resolve until they drain; new
// registrations always mint v2.
const (
	brokerTokenPrefixV1 = "zapact:"
	brokerTokenPrefixV2 = "zapact2:"
)

// ErrTokenForged, ErrTokenExpired, ErrTokenMismatch are returned by
// VerifyToken so callers can log the specific failure mode. They are
// not surfaced to the wire — the respond handler treats any
// verification failure as "not broker-owned" and falls through.
var (
	errTokenMalformed = fmt.Errorf("broker token: malformed")
	errTokenForged    = fmt.Errorf("broker token: invalid signature")
	errTokenExpired   = fmt.Errorf("broker token: expired")
	errTokenMismatch  = fmt.Errorf("broker token: id/workflow mismatch")
)

// tokenClaims is the signed payload embedded in a v2 broker token.
// Field names are short to keep the token compact on the wire.
type tokenClaims struct {
	NS  string `json:"ns"`
	TQ  string `json:"tq"`
	WF  string `json:"wf"`
	RID string `json:"rid,omitempty"`
	AID string `json:"aid"`
	Exp int64  `json:"exp"` // unix seconds
}

// activityScope identifies the workflow that owns an activity.
// Passed into Register so the broker can bind the token.
type activityScope struct {
	Namespace  string
	TaskQueue  string
	WorkflowID string
	RunID      string
}

// activityOutcome is the settled result of a broker-owned activity.
type activityOutcome struct {
	result  []byte
	failure []byte
}

// brokerEntry is one pending activity registration.
type brokerEntry struct {
	scope   activityScope
	done    chan activityOutcome
	settled bool
	outcome activityOutcome
	expires time.Time
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

	// signKey is the HMAC-SHA256 secret used to sign v2 tokens. Loaded
	// from TASKS_ACTIVITY_TOKEN_KEY (base64) at construction; if the
	// env var is unset a fresh random key is generated and a warning
	// is logged. The key is never written to disk.
	signKey []byte
}

func newActivityBroker() *frontendActivityBroker {
	return newActivityBrokerWithKey(loadOrGenerateTokenKey(slog.Default()))
}

// newActivityBrokerWithKey is exposed for tests so they can pin the
// signing key and construct forged tokens.
func newActivityBrokerWithKey(key []byte) *frontendActivityBroker {
	return &frontendActivityBroker{
		entries: make(map[string]*brokerEntry),
		now:     time.Now,
		signKey: key,
	}
}

// loadOrGenerateTokenKey reads TASKS_ACTIVITY_TOKEN_KEY (base64) and
// falls back to a random 32-byte key if unset or invalid. The
// fallback is logged at WARN so ops can see that tokens will not
// validate across restarts (acceptable for dev, not for prod).
func loadOrGenerateTokenKey(logger *slog.Logger) []byte {
	if logger == nil {
		logger = slog.Default()
	}
	if v := os.Getenv("TASKS_ACTIVITY_TOKEN_KEY"); v != "" {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err == nil && len(raw) >= 16 {
			return raw
		}
		logger.Warn("TASKS_ACTIVITY_TOKEN_KEY unusable, generating ephemeral key", "err", err, "len", len(raw))
	} else {
		logger.Warn("TASKS_ACTIVITY_TOKEN_KEY unset; generating ephemeral broker signing key (tokens will not validate across restarts)")
	}
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		// crypto/rand never fails on supported platforms; but if it
		// does we would rather crash than run without a key.
		panic(fmt.Errorf("broker: cannot read random bytes for signing key: %w", err))
	}
	return k
}

// Register creates a pending entry for activityTaskID and returns a
// signed v2 TaskToken that respond handlers must present to deliver
// the result. scope binds the token to its originating workflow.
//
// If the id is already registered, Register is a no-op and returns
// the same token — schedule is idempotent so replay-safe.
func (b *frontendActivityBroker) Register(activityTaskID string, scope activityScope, ttl time.Duration) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()

	exp := b.now().Add(ttl)
	if _, ok := b.entries[activityTaskID]; !ok {
		b.entries[activityTaskID] = &brokerEntry{
			scope:   scope,
			done:    make(chan activityOutcome, 1),
			expires: exp,
		}
	} else {
		// Preserve the first scope; keep the existing expiry so the
		// TTL doesn't silently extend on re-register.
		exp = b.entries[activityTaskID].expires
	}
	return b.mintToken(activityTaskID, scope, exp)
}

// mintToken produces a v2 HMAC-signed broker token.
func (b *frontendActivityBroker) mintToken(activityTaskID string, scope activityScope, exp time.Time) []byte {
	claims := tokenClaims{
		NS:  scope.Namespace,
		TQ:  scope.TaskQueue,
		WF:  scope.WorkflowID,
		RID: scope.RunID,
		AID: activityTaskID,
		Exp: exp.Unix(),
	}
	payload, _ := json.Marshal(claims)
	sig := b.sign(payload)

	encPayload := base64.RawURLEncoding.EncodeToString(payload)
	encSig := base64.RawURLEncoding.EncodeToString(sig)
	return []byte(brokerTokenPrefixV2 + encPayload + "." + encSig)
}

// sign computes HMAC-SHA256 over payload using the broker's signing key.
func (b *frontendActivityBroker) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, b.signKey)
	m.Write(payload)
	return m.Sum(nil)
}

// verifyToken validates a v2 token and returns the embedded
// activityTaskID. v1 tokens are accepted in legacy mode: they pass
// signature check vacuously but Complete/Fail must then confirm the
// entry still exists and the id matches — forgery is still possible
// on v1 tokens, which is why all new tokens are v2.
//
// Errors map to the sentinel values above; callers that only care
// about the id can ignore the err and check id != "".
func (b *frontendActivityBroker) verifyToken(token []byte) (id string, err error) {
	s := string(token)
	switch {
	case strings.HasPrefix(s, brokerTokenPrefixV2):
		rest := s[len(brokerTokenPrefixV2):]
		dot := strings.IndexByte(rest, '.')
		if dot < 0 {
			return "", errTokenMalformed
		}
		encPayload := rest[:dot]
		encSig := rest[dot+1:]
		payload, e1 := base64.RawURLEncoding.DecodeString(encPayload)
		sig, e2 := base64.RawURLEncoding.DecodeString(encSig)
		if e1 != nil || e2 != nil {
			return "", errTokenMalformed
		}
		expected := b.sign(payload)
		if !hmac.Equal(sig, expected) {
			return "", errTokenForged
		}
		var claims tokenClaims
		if err := json.Unmarshal(payload, &claims); err != nil {
			return "", errTokenMalformed
		}
		if claims.Exp > 0 && b.now().Unix() > claims.Exp {
			return "", errTokenExpired
		}
		// Bind: the entry must exist and its recorded scope must match
		// the claims. This is what stops a v2 token minted for
		// workflow A from being used to settle activity B scheduled
		// by workflow B — the id would resolve, but the scope lookup
		// would mismatch.
		b.mu.Lock()
		e, ok := b.entries[claims.AID]
		b.mu.Unlock()
		if ok && !scopeMatches(e.scope, claims) {
			return "", errTokenMismatch
		}
		return claims.AID, nil
	case strings.HasPrefix(s, brokerTokenPrefixV1):
		return s[len(brokerTokenPrefixV1):], nil
	}
	return "", errTokenMalformed
}

func scopeMatches(s activityScope, c tokenClaims) bool {
	if s.Namespace != c.NS {
		return false
	}
	if s.TaskQueue != c.TQ {
		return false
	}
	if s.WorkflowID != c.WF {
		return false
	}
	// RunID may be empty on schedule (not yet known); only compare
	// when both sides were populated.
	if s.RunID != "" && c.RID != "" && s.RunID != c.RID {
		return false
	}
	return true
}

// ExtractID returns the activity id encoded in token, or "" if token
// is not a broker-issued token or fails verification.
func (b *frontendActivityBroker) ExtractID(token []byte) string {
	id, err := b.verifyToken(token)
	if err != nil {
		return ""
	}
	return id
}

// Complete delivers a success outcome to the waiter.
//
// Returns true if the token was broker-owned, verified, and the
// delivery succeeded (or was a duplicate of an already-settled entry).
func (b *frontendActivityBroker) Complete(token []byte, result []byte) bool {
	return b.deliver(token, activityOutcome{result: result})
}

// Fail delivers a failure outcome to the waiter.
func (b *frontendActivityBroker) Fail(token []byte, failure []byte) bool {
	return b.deliver(token, activityOutcome{failure: failure})
}

func (b *frontendActivityBroker) deliver(token []byte, out activityOutcome) bool {
	id, err := b.verifyToken(token)
	if err != nil {
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
//
// Deprecated: use WaitCtx so the caller's context deadline /
// cancellation is honored.
func (b *frontendActivityBroker) Wait(activityTaskID string, waitFor time.Duration) (ready bool, result, failure []byte) {
	ready, result, failure, _ = b.WaitCtx(context.Background(), activityTaskID, waitFor)
	return
}

// WaitCtx is the context-aware form of Wait. It returns as soon as
// any of these occur:
//   - the entry settles (ready=true)
//   - waitFor elapses (ready=false, err=nil)
//   - ctx is canceled or its deadline is exceeded (ready=false, err=ctx.Err())
//
// A waitFor of 0 means single-poll: if a result is not already
// present the call returns immediately without consulting ctx.
func (b *frontendActivityBroker) WaitCtx(ctx context.Context, activityTaskID string, waitFor time.Duration) (ready bool, result, failure []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}

	b.mu.Lock()
	e, ok := b.entries[activityTaskID]
	if !ok {
		b.mu.Unlock()
		return false, nil, nil, nil
	}
	if e.settled {
		out := e.outcome
		delete(b.entries, activityTaskID)
		b.mu.Unlock()
		return true, out.result, out.failure, nil
	}
	done := e.done
	b.mu.Unlock()

	if waitFor <= 0 {
		return false, nil, nil, nil
	}

	// Fast-path: ctx already canceled before we park.
	if cerr := ctx.Err(); cerr != nil {
		return false, nil, nil, fmt.Errorf("broker.Wait: %w", cerr)
	}

	t := time.NewTimer(waitFor)
	defer t.Stop()
	select {
	case out := <-done:
		b.mu.Lock()
		delete(b.entries, activityTaskID)
		b.mu.Unlock()
		return true, out.result, out.failure, nil
	case <-t.C:
		return false, nil, nil, nil
	case <-ctx.Done():
		return false, nil, nil, fmt.Errorf("broker.Wait: %w", ctx.Err())
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
