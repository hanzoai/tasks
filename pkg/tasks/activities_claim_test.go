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
