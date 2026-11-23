// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"testing"
	"time"
)

// The worker pulls jobs over HTTP: POST .../activities/claim returns 200 with
// the claimed activity, or 204 when the queue is empty. This is the exact wire
// contract `hanzo gpu connect` polls.
func TestClaim_HTTP(t *testing.T) {
	_, h := httpFixture(t)
	base := "/v1/tasks/namespaces/default/activities"
	if code, _ := httpDo(t, h, "POST", base, map[string]any{
		"activityId":   "job-http",
		"activityType": map[string]any{"name": "echo"},
		"taskQueue":    "gpu",
	}); code != 200 {
		t.Fatalf("start: code=%d", code)
	}
	code, out := httpDo(t, h, "POST", base+"/claim", map[string]any{"taskQueue": "gpu", "identity": "spark", "leaseSeconds": 30})
	if code != 200 {
		t.Fatalf("claim: code=%d body=%v", code, out)
	}
	if out["status"] != activityStateStarted {
		t.Fatalf("claim status=%v want STARTED", out["status"])
	}
	if out["identity"] != "spark" {
		t.Fatalf("claim identity=%v", out["identity"])
	}
	// Empty queue ⇒ 204 No Content.
	if code, _ := httpDo(t, h, "POST", base+"/claim", map[string]any{"taskQueue": "gpu", "identity": "spark"}); code != 204 {
		t.Fatalf("empty claim: code=%d want 204", code)
	}
}

// ClaimNextActivity is the outbound worker's pull primitive: it must move the
// oldest SCHEDULED activity to STARTED under a lease, stamp the claimant, and
// never hand the same activity to two workers.
func TestClaim_TransitionsAndLeases(t *testing.T) {
	en, ns := engineFixture(t)
	if _, err := en.StartActivity(ns, "job-1", "", TypeRef{Name: "echo"}, "gpu", nil, nil, "", "", "", "30s", "", ""); err != nil {
		t.Fatalf("start: %v", err)
	}
	a, ok, err := en.ClaimNextActivity(ns, "gpu", "worker-a", 0)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if a.Status != activityStateStarted {
		t.Fatalf("status=%s want STARTED", a.Status)
	}
	if a.Identity != "worker-a" {
		t.Fatalf("identity=%q want worker-a", a.Identity)
	}
	if a.LeaseExpiry == "" {
		t.Fatalf("lease not set")
	}
	// Queue is now empty: a second claim yields nothing (no double-claim).
	if _, ok, err := en.ClaimNextActivity(ns, "gpu", "worker-b", 0); err != nil || ok {
		t.Fatalf("second claim: ok=%v err=%v, want ok=false", ok, err)
	}
}

// A claim filtered by taskQueue must skip activities on other queues.
func TestClaim_FiltersByTaskQueue(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.StartActivity(ns, "cpu-job", "", TypeRef{Name: "echo"}, "cpu", nil, nil, "", "", "", "", "", "")
	if _, ok, _ := en.ClaimNextActivity(ns, "gpu", "w", 0); ok {
		t.Fatalf("claimed a cpu-queue job on the gpu queue")
	}
	if _, ok, _ := en.ClaimNextActivity(ns, "cpu", "w", 0); !ok {
		t.Fatalf("failed to claim the cpu-queue job")
	}
}

// An empty taskQueue claims from any queue, oldest first.
func TestClaim_OldestFirstAnyQueue(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.StartActivity(ns, "first", "", TypeRef{Name: "echo"}, "q1", nil, nil, "", "", "", "", "", "")
	time.Sleep(1100 * time.Millisecond) // RFC3339 second-granularity ordering
	_, _ = en.StartActivity(ns, "second", "", TypeRef{Name: "echo"}, "q2", nil, nil, "", "", "", "", "", "")
	a, ok, err := en.ClaimNextActivity(ns, "", "w", 0)
	if err != nil || !ok {
		t.Fatalf("claim: %v", err)
	}
	if a.Execution.WorkflowId != "first" {
		t.Fatalf("claimed %q, want oldest 'first'", a.Execution.WorkflowId)
	}
}

// An expired lease returns the activity to SCHEDULED (attempt bumped) so a
// peer/restarted worker reclaims it — the reaper on the claim hot path.
func TestReap_ExpiredLeaseRequeues(t *testing.T) {
	en, ns := engineFixture(t)
	// 1s heartbeat SLA ⇒ 1s lease; wait it out then reap.
	_, _ = en.StartActivity(ns, "flaky", "", TypeRef{Name: "echo"}, "gpu", nil, nil, "", "", "", "1s", "", "")
	first, ok, _ := en.ClaimNextActivity(ns, "gpu", "dead-worker", 0)
	if !ok {
		t.Fatalf("initial claim failed")
	}
	if first.Attempt != 1 {
		t.Fatalf("attempt=%d want 1", first.Attempt)
	}
	time.Sleep(1200 * time.Millisecond)
	// A new claim reaps the dead worker's lease and hands the job over.
	again, ok, err := en.ClaimNextActivity(ns, "gpu", "live-worker", 0)
	if err != nil || !ok {
		t.Fatalf("reclaim: ok=%v err=%v", ok, err)
	}
	if again.Execution.WorkflowId != "flaky" {
		t.Fatalf("reclaimed %q", again.Execution.WorkflowId)
	}
	if again.Identity != "live-worker" {
		t.Fatalf("identity=%q want live-worker", again.Identity)
	}
	if again.Attempt != 2 {
		t.Fatalf("attempt=%d want 2 after requeue", again.Attempt)
	}
}

// When MaximumAttempts is exhausted the reaper FAILS the activity instead of
// requeuing it forever.
func TestReap_ExhaustedAttemptsFails(t *testing.T) {
	en, ns := engineFixture(t)
	rp := &RetryPolicy{MaximumAttempts: 1}
	_, _ = en.StartActivity(ns, "poison", "", TypeRef{Name: "echo"}, "gpu", nil, rp, "", "", "", "1s", "", "")
	if _, ok, _ := en.ClaimNextActivity(ns, "gpu", "dead", 0); !ok {
		t.Fatalf("claim failed")
	}
	time.Sleep(1200 * time.Millisecond)
	if err := en.reapExpiredLeases(ns); err != nil {
		t.Fatalf("reap: %v", err)
	}
	rows, _, _ := en.ListActivities(ns, "", 0)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	if rows[0].Status != activityStateFailed {
		t.Fatalf("status=%s want FAILED", rows[0].Status)
	}
}

// A live heartbeat keeps the lease from expiring — the worker stays owner.
func TestReap_HeartbeatKeepsLease(t *testing.T) {
	en, ns := engineFixture(t)
	_, _ = en.StartActivity(ns, "long", "", TypeRef{Name: "echo"}, "gpu", nil, nil, "", "", "", "2s", "", "")
	a, _, _ := en.ClaimNextActivity(ns, "gpu", "w", 0)
	run := a.Execution.RunId
	time.Sleep(1200 * time.Millisecond)
	if err := en.HeartbeatActivity(ns, "long", run, "still working"); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := en.reapExpiredLeases(ns); err != nil {
		t.Fatalf("reap: %v", err)
	}
	got, _, _ := en.DescribeActivity(ns, "long", run)
	if got.Status != activityStateStarted || got.Identity != "w" {
		t.Fatalf("heartbeat lost the lease: status=%s identity=%s", got.Status, got.Identity)
	}
}

// A presence record (heartbeated but never claimed) has no lease and the
// reaper must leave it alone — this is how the fleet registry keeps a GPU
// worker's row alive without it looking like a claimable job.
func TestReap_IgnoresUnclaimedPresence(t *testing.T) {
	en, ns := engineFixture(t)
	pres, _ := en.StartActivity(ns, "spark", "", TypeRef{Name: "fleet.worker"}, "fleet", map[string]any{"gpu": "NVIDIA GB10"}, nil, "", "", "", "5s", "", "")
	run := pres.Execution.RunId
	if err := en.HeartbeatActivity(ns, "spark", run, nil); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := en.reapExpiredLeases(ns); err != nil {
		t.Fatalf("reap: %v", err)
	}
	got, _, _ := en.DescribeActivity(ns, "spark", run)
	if got.Status != activityStateStarted {
		t.Fatalf("presence record was reaped: status=%s", got.Status)
	}
	if got.LeaseExpiry != "" {
		t.Fatalf("presence record acquired a lease: %s", got.LeaseExpiry)
	}
}

// ReleaseActivity returns a STARTED activity to SCHEDULED without incrementing
// Attempt, optionally deferring claims until NotBefore elapses, and rejects a
// mismatched identity with 409 Conflict.
func TestRelease_BusyDeferralAndNotBefore(t *testing.T) {
	en, h := httpFixture(t)
	base := "/v1/tasks/namespaces/default/activities"
	a, err := en.StartActivity("default", "gpu-bench-1", "", TypeRef{Name: "research.bench"}, "gpu:dgx-hanzod", map[string]any{"benchmark": "arc"}, &RetryPolicy{MaximumAttempts: 2}, "", "", "60s", "30s", "", "")
	if err != nil {
		t.Fatalf("StartActivity: %v", err)
	}
	run := a.Execution.RunId

	claimed, ok, err := en.ClaimNextActivity("default", "gpu:dgx-hanzod", "dgx-hanzod", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("ClaimNextActivity: ok=%v err=%v", ok, err)
	}
	if claimed.Attempt != 1 {
		t.Fatalf("attempt=%d want 1", claimed.Attempt)
	}

	// Stale/wrong worker identity must be rejected with 409.
	if code, _ := httpDo(t, h, "POST", base+"/gpu-bench-1/"+run+"/release", map[string]any{
		"reason":       "active_dev_session",
		"identity":     "impostor",
		"delaySeconds": 5,
	}); code != 409 {
		t.Fatalf("wrong identity release code=%d want 409", code)
	}

	// Owner releases with 1s busy deferral -> SCHEDULED, Attempt stays 1, DeferCount=1.
	code, relOut := httpDo(t, h, "POST", base+"/gpu-bench-1/"+run+"/release", map[string]any{
		"reason":       "active_research_run",
		"identity":     "dgx-hanzod",
		"delaySeconds": 1,
	})
	if code != 200 {
		t.Fatalf("release code=%d body=%v", code, relOut)
	}
	got, _, _ := en.DescribeActivity("default", "gpu-bench-1", run)
	if got.Status != activityStateScheduled {
		t.Fatalf("status=%s want SCHEDULED", got.Status)
	}
	if got.Attempt != 1 {
		t.Fatalf("attempt=%d want 1 (busy release must never consume retry budget)", got.Attempt)
	}
	if got.DeferCount != 1 || got.LastBusyReason != "active_research_run" {
		t.Fatalf("deferCount=%d reason=%q", got.DeferCount, got.LastBusyReason)
	}
	if got.Identity != "" || got.LeaseExpiry != "" || got.NotBefore == "" {
		t.Fatalf("expected cleared identity/lease and non-empty NotBefore, got %+v", got)
	}

	// Immediate claim while NotBefore is in the future must skip it.
	if _, ok, err := en.ClaimNextActivity("default", "gpu:dgx-hanzod", "dgx-hanzod", 30*time.Second); err != nil || ok {
		t.Fatalf("claimed before NotBefore elapsed: ok=%v err=%v", ok, err)
	}

	// Wait past the 1s NotBefore window -> claim succeeds on Attempt 1!
	time.Sleep(1200 * time.Millisecond)
	reclaimed, ok, err := en.ClaimNextActivity("default", "gpu:dgx-hanzod", "dgx-hanzod", 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("reclaim after NotBefore: ok=%v err=%v", ok, err)
	}
	if reclaimed.Attempt != 1 || reclaimed.DeferCount != 1 {
		t.Fatalf("reclaimed=%+v, want attempt=1 deferCount=1", reclaimed)
	}

	// Zero-delay release makes the task immediately claimable by an idle peer.
	if err := en.ReleaseActivity("default", "gpu-bench-1", run, "backend_mismatch", "dgx-hanzod", 0); err != nil {
		t.Fatalf("zero-delay release: %v", err)
	}
	peer, ok, err := en.ClaimNextActivity("default", "gpu:dgx-hanzod", "mac-studio-hanzod", 30*time.Second)
	if err != nil || !ok || peer.Identity != "mac-studio-hanzod" || peer.DeferCount != 2 {
		t.Fatalf("peer immediate claim failed: ok=%v err=%v peer=%+v", ok, err, peer)
	}

	events, _, err := en.GetActivityHistory("default", "gpu-bench-1", run, 0, 20, false)
	if err != nil {
		t.Fatalf("GetActivityHistory: %v", err)
	}
	defers := 0
	for _, ev := range events {
		if ev.EventType == "ACTIVITY_TASK_DEFERRED" {
			defers++
		}
	}
	if defers != 2 {
		t.Fatalf("ACTIVITY_TASK_DEFERRED count=%d want 2", defers)
	}
}

// FailActivity requeues with exponential backoff when RetryPolicy has remaining
// attempts, honors NonRetryableErrorTypes immediately, and transitions to
// terminal FAILED once MaximumAttempts is reached.
func TestFailActivity_RetryBackoffAndNonRetryable(t *testing.T) {
	en, ns := engineFixture(t)
	rp := &RetryPolicy{
		InitialInterval:        "150ms",
		BackoffCoefficient:     2.0,
		MaximumInterval:        "1s",
		MaximumAttempts:        3,
		NonRetryableErrorTypes: []string{"InvalidConfig"},
	}
	a, err := en.StartActivity(ns, "retry-job", "", TypeRef{Name: "hanzod.run"}, "gpu-jobs", nil, rp, "", "", "", "30s", "", "")
	if err != nil {
		t.Fatalf("StartActivity: %v", err)
	}
	run := a.Execution.RunId

	if _, ok, _ := en.ClaimNextActivity(ns, "gpu-jobs", "w1", 30*time.Second); !ok {
		t.Fatal("claim 1 failed")
	}
	// Attempt 1 fails -> requeued as Attempt 2 with 150ms backoff.
	if err := en.FailActivity(ns, "retry-job", run, "transient network blip", "w1"); err != nil {
		t.Fatalf("FailActivity attempt 1: %v", err)
	}
	cur, _, _ := en.DescribeActivity(ns, "retry-job", run)
	if cur.Status != activityStateScheduled || cur.Attempt != 2 || cur.NotBefore == "" {
		t.Fatalf("after attempt 1 fail: %+v, want SCHEDULED attempt=2 with NotBefore", cur)
	}
	streaks, _ := en.FailureStreaks(ns)
	if len(streaks) != 1 || !streaks[0].Retrying {
		t.Fatalf("streaks after retryable fail = %+v, want 1 retrying streak", streaks)
	}
	// Must not be claimable during the 150ms backoff window.
	if _, ok, _ := en.ClaimNextActivity(ns, "gpu-jobs", "w1", 30*time.Second); ok {
		t.Fatal("claimed during retry backoff")
	}
	time.Sleep(180 * time.Millisecond)
	if _, ok, _ := en.ClaimNextActivity(ns, "gpu-jobs", "w2", 30*time.Second); !ok {
		t.Fatal("claim 2 after backoff failed")
	}

	// Non-retryable error type terminates immediately even before Attempt 3!
	if err := en.FailActivity(ns, "retry-job", run, "InvalidConfig: missing benchmark", "w2"); err != nil {
		t.Fatalf("FailActivity non-retryable: %v", err)
	}
	cur, _, _ = en.DescribeActivity(ns, "retry-job", run)
	if cur.Status != activityStateFailed || cur.Attempt != 2 {
		t.Fatalf("after non-retryable fail: %+v, want FAILED at attempt=2", cur)
	}
}

// Long-polling POST .../activities/claim with waitSeconds wakes immediately
// via the event broker (<50ms) when StartActivity or ReleaseActivity fires,
// eliminating fixed-interval worker polling timers.
func TestClaim_LongPollWakesImmediatelyOnScheduleAndRelease(t *testing.T) {
	en, h := httpFixture(t)
	base := "/v1/tasks/namespaces/default/activities"

	type claimRes struct {
		code    int
		out     map[string]any
		elapsed time.Duration
	}
	ch := make(chan claimRes, 1)
	go func() {
		t0 := time.Now()
		code, out := httpDo(t, h, "POST", base+"/claim", map[string]any{
			"taskQueue":    "gpu:dgx-hanzod",
			"identity":     "dgx-hanzod",
			"leaseSeconds": 30,
			"waitSeconds":  5,
		})
		ch <- claimRes{code: code, out: out, elapsed: time.Since(t0)}
	}()

	time.Sleep(15 * time.Millisecond)
	tPush := time.Now()
	a, err := en.StartActivity("default", "instant-1", "", TypeRef{Name: "hanzod.run"}, "gpu:dgx-hanzod", map[string]any{"ok": true}, nil, "", "", "", "30s", "", "")
	if err != nil {
		t.Fatalf("StartActivity: %v", err)
	}

	res := <-ch
	wakeLatency := time.Since(tPush)
	if res.code != 200 {
		t.Fatalf("long-poll claim code=%d want 200", res.code)
	}
	if wakeLatency > 50*time.Millisecond {
		t.Fatalf("long-poll wake latency=%s, want <50ms (event-driven, not 5s timeout)", wakeLatency)
	}

	// Second worker long-polls; first worker releases -> second worker wakes immediately!
	ch2 := make(chan claimRes, 1)
	go func() {
		code, out := httpDo(t, h, "POST", base+"/claim", map[string]any{
			"taskQueue":    "gpu:dgx-hanzod",
			"identity":     "evo-hanzod",
			"leaseSeconds": 30,
			"waitSeconds":  5,
		})
		ch2 <- claimRes{code: code, out: out}
	}()
	time.Sleep(15 * time.Millisecond)
	tRel := time.Now()
	if err := en.ReleaseActivity("default", "instant-1", a.Execution.RunId, "yielding_to_peer", "dgx-hanzod", 0); err != nil {
		t.Fatalf("ReleaseActivity: %v", err)
	}
	res2 := <-ch2
	if res2.code != 200 || res2.out["identity"] != "evo-hanzod" {
		t.Fatalf("peer long-poll claim on release failed: %+v", res2)
	}
	if d := time.Since(tRel); d > 50*time.Millisecond {
		t.Fatalf("release wake latency=%s, want <50ms", d)
	}
}
