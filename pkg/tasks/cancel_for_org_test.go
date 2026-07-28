// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"fmt"
	"testing"
)

// TestCancelActivityForOrg proves the org-scoped cancel MUTATOR: it cancels a job
// in the caller's shard, is rejected once the job is terminal, and never reaches
// across the org boundary — the same tenancy contract ActivitiesForOrg reads under.
func TestCancelActivityForOrg(t *testing.T) {
	emb, err := Embed(context.Background(), EmbedConfig{Address: ""})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(context.Background())

	const org, ns = "acme", "gpu-jobs"
	en := emb.engine.As(Org(org))
	if err := en.RegisterNamespace(Namespace{
		NamespaceInfo: NamespaceInfo{Name: ns, State: "NAMESPACE_STATE_REGISTERED"},
		Config:        NamespaceCfg{WorkflowExecutionRetentionTtl: "720h", APSLimit: 400},
	}); err != nil {
		t.Fatalf("register ns: %v", err)
	}
	if _, err := en.StartActivity(ns, "job1", "run1", TypeRef{Name: "studio.render"},
		"gpu:spark", map[string]any{"prompt": "x"}, nil, "", "", "", "", "", ""); err != nil {
		t.Fatalf("start activity: %v", err)
	}

	// Cancel via the org-scoped wrapper → the job is CANCELED in this org's shard.
	if err := emb.CancelActivityForOrg(org, ns, "job1", "run1", "user canceled", "tester"); err != nil {
		t.Fatalf("CancelActivityForOrg: %v", err)
	}
	rows, err := emb.ActivitiesForOrg(org, ns)
	if err != nil {
		t.Fatalf("ActivitiesForOrg: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != activityStateCanceled {
		t.Fatalf("status = %+v, want %s", rows, activityStateCanceled)
	}

	// A terminal activity refuses a second cancel (matches the HTTP path's 409).
	if err := emb.CancelActivityForOrg(org, ns, "job1", "run1", "again", "tester"); err == nil {
		t.Fatal("second cancel on a terminal activity should error")
	}

	// Tenancy: another org's shard has no such activity — cancel is refused, never
	// reaching across the boundary.
	if err := emb.CancelActivityForOrg("other-org", ns, "job1", "run1", "x", "y"); err == nil {
		t.Fatal("cross-org cancel should error (activity not in the other org's shard)")
	}
}

// TestActivitiesPageForOrgWalksPastFirstPage proves the paginated read returns EVERY
// activity, not just the hash-ordered first 100 ActivitiesForOrg caps at — the
// truncation that hid live jobs and dropped online workers on a busy org.
func TestActivitiesPageForOrgWalksPastFirstPage(t *testing.T) {
	emb, err := Embed(context.Background(), EmbedConfig{Address: ""})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(context.Background())

	const org, ns, n = "acme", "gpu-jobs", 250
	en := emb.engine.As(Org(org))
	if err := en.RegisterNamespace(Namespace{
		NamespaceInfo: NamespaceInfo{Name: ns, State: "NAMESPACE_STATE_REGISTERED"},
		Config:        NamespaceCfg{WorkflowExecutionRetentionTtl: "720h", APSLimit: 400},
	}); err != nil {
		t.Fatalf("register ns: %v", err)
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("job-%03d", i)
		if _, err := en.StartActivity(ns, id, id, TypeRef{Name: "studio.render"},
			"gpu:spark", nil, nil, "", "", "", "", "", ""); err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
	}
	// The single-page read caps at 100 — the truncation this fix addresses.
	first, err := emb.ActivitiesForOrg(org, ns)
	if err != nil {
		t.Fatalf("ActivitiesForOrg: %v", err)
	}
	if len(first) != 100 {
		t.Fatalf("ActivitiesForOrg returned %d, expected the 100-row cap", len(first))
	}
	// The paginated walk returns EVERY row.
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; pages < 100; pages++ {
		page, next, err := emb.ActivitiesPageForOrg(org, ns, cursor, 100)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		for _, a := range page {
			seen[a.Execution.WorkflowId] = true
		}
		if next == "" || len(page) == 0 {
			break
		}
		cursor = next
	}
	if len(seen) != n {
		t.Fatalf("paginated walk saw %d distinct jobs, want %d (all)", len(seen), n)
	}
}
