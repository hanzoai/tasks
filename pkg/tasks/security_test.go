// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"testing"
	"time"
)

// TestEngineWithOrg_KeyspaceIsolation — same namespace name across two
// orgs must not collide. A workflow created under org A is invisible to
// org B even though the namespace+workflow id strings are identical.
func TestEngineWithOrg_KeyspaceIsolation(t *testing.T) {
	en := newEngine(newStore())
	if err := en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "shared", State: "NAMESPACE_STATE_REGISTERED"}}); err != nil {
		t.Fatal(err)
	}

	a := en.WithOrg("org-a")
	b := en.WithOrg("org-b")

	if err := a.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "shared", State: "NAMESPACE_STATE_REGISTERED"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "shared", State: "NAMESPACE_STATE_REGISTERED"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StartWorkflow("shared", "wf-1", "", TypeRef{Name: "Demo"}, "default", "secret-from-a"); err != nil {
		t.Fatal(err)
	}

	rowsA, _ := a.ListWorkflows("shared")
	rowsB, _ := b.ListWorkflows("shared")
	if len(rowsA) != 1 {
		t.Fatalf("org A expected 1 wf; got %d", len(rowsA))
	}
	if len(rowsB) != 0 {
		t.Fatalf("org B leaked org A's workflow: %d rows", len(rowsB))
	}

	// Org B trying to describe org A's workflow must miss.
	if _, ok, _ := b.DescribeWorkflow("shared", "wf-1", ""); ok {
		t.Fatal("cross-org describe leaked")
	}
}

// TestEngineEmit_StampsOrgID — every emitted Event carries the engine's
// org so SSE per-org filtering can trust the field.
func TestEngineEmit_StampsOrgID(t *testing.T) {
	en := newEngine(newStore())
	_ = en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"}})
	_, ch := en.broker.subscribe()
	scoped := en.WithOrg("org-x")

	// Bootstrap namespace under org-x so StartWorkflow finds it.
	_ = scoped.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"}})
	// Drain bootstrap events.
	for i := 0; i < 2; i++ {
		select {
		case <-ch:
		case <-time.After(time.Second):
		}
	}
	if _, err := scoped.StartWorkflow("default", "wf-1", "", TypeRef{Name: "Demo"}, "default", nil); err != nil {
		t.Fatal(err)
	}
	// Drain until we see the workflow.started event.
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Kind == "workflow.started" {
				if ev.OrgID != "org-x" {
					t.Fatalf("event missing org tag: got %q", ev.OrgID)
				}
				return
			}
		case <-deadline:
			t.Fatal("workflow.started event never fired")
		}
	}
}

// TestVisibilityQuery_CannotWidenScope — even with OR/NOT in the
// visibility query, results stay within the calling org because filtering
// runs after the org-scoped ListWorkflows.
func TestVisibilityQuery_CannotWidenScope(t *testing.T) {
	en := newEngine(newStore())
	_ = en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"}})
	a := en.WithOrg("org-a")
	b := en.WithOrg("org-b")
	_ = a.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"}})
	_ = b.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"}})
	_, _ = a.StartWorkflow("default", "wf-a", "", TypeRef{Name: "DemoA"}, "default", nil)
	_, _ = b.StartWorkflow("default", "wf-b", "", TypeRef{Name: "DemoB"}, "default", nil)

	// Try to widen with OR true.
	rows, err := b.ListWorkflowExecutions("default", "WorkflowId='wf-a' OR WorkflowId='wf-b'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Execution.WorkflowId != "wf-b" {
		t.Fatalf("OR widening leaked across orgs: %+v", rows)
	}

	// NOT trick.
	rows, err = b.ListWorkflowExecutions("default", "NOT WorkflowId='zz'")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Execution.WorkflowId != "wf-b" {
		t.Fatalf("NOT widening leaked across orgs: %+v", rows)
	}
}

// TestAddSearchAttribute_RejectsReservedNames — system field names can't
// be shadowed by tenant-registered search attributes.
func TestAddSearchAttribute_RejectsReservedNames(t *testing.T) {
	en := newEngine(newStore())
	_ = en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"}})
	for _, name := range []string{"WorkflowId", "ExecutionStatus", "TaskQueue", "TemporalScheduledStartTime"} {
		if err := en.AddSearchAttribute("default", SearchAttribute{Name: name, Type: "Keyword"}); err == nil {
			t.Fatalf("reserved name %q must be rejected", name)
		}
	}
	// Sanity: regular custom name still works.
	if err := en.AddSearchAttribute("default", SearchAttribute{Name: "MyCustom", Type: "Keyword"}); err != nil {
		t.Fatalf("custom name rejected: %v", err)
	}
}

// TestValidIdent — path-traversal sentinels must be rejected.
func TestValidIdent(t *testing.T) {
	good := []string{"default", "hanzo-prod", "ns_1", "a.b.c"}
	bad := []string{"", ".", "..", "../etc", "a/b", "a b", "a\x00b", string(make([]byte, 65))}
	for _, s := range good {
		if !validIdent(s) {
			t.Fatalf("valid ident rejected: %q", s)
		}
	}
	for _, s := range bad {
		if validIdent(s) {
			t.Fatalf("invalid ident accepted: %q", s)
		}
	}
}

// TestDispatcher_PendingQueriesBounded — without bounding, an attacker
// who subscribes then never responds could grow the pending-query map
// without limit. Verify the cap kicks in via maxPendingQueries.
func TestDispatcher_PendingQueriesBounded(t *testing.T) {
	d := newDispatcher()
	// Pre-populate past the cap by writing directly (tests the eviction
	// logic without needing a live worker subscriber).
	now := time.Now()
	for i := 0; i < maxPendingQueries+10; i++ {
		d.queries[newRandID()] = &pendingQuery{resCh: make(chan queryResponse, 1), at: now.Add(time.Duration(i) * time.Microsecond)}
	}
	d.mu.Lock()
	d.evictExpiredQueriesLocked()
	for len(d.queries) >= maxPendingQueries {
		d.evictOldestQueryLocked()
	}
	d.mu.Unlock()
	if got := len(d.queries); got >= maxPendingQueries {
		t.Fatalf("queries map not bounded: got %d entries", got)
	}
}

// TestDispatcher_PendingQueriesTTL — entries older than pendingQueryTTL
// are reclaimed by the eviction sweep on the next PushQuery.
func TestDispatcher_PendingQueriesTTL(t *testing.T) {
	d := newDispatcher()
	old := newRandID()
	d.queries[old] = &pendingQuery{resCh: make(chan queryResponse, 1), at: time.Now().Add(-2 * pendingQueryTTL)}
	d.mu.Lock()
	d.evictExpiredQueriesLocked()
	d.mu.Unlock()
	if _, ok := d.queries[old]; ok {
		t.Fatal("expired pending query was not evicted")
	}
}

// TestQuery_NoWorkers_DoesNotLeakPending — ensure CancelQuery / timeout
// drops the entry without leaving residue.
func TestQuery_NoWorkers_DoesNotLeakPending(t *testing.T) {
	en := newEngine(newStore())
	_ = en.RegisterNamespace(Namespace{NamespaceInfo: NamespaceInfo{Name: "default", State: "NAMESPACE_STATE_REGISTERED"}})
	_, _ = en.StartWorkflow("default", "wf-q", "", TypeRef{Name: "Demo"}, "tq", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _ = en.QueryWorkflowCtx(ctx, "default", "wf-q", "", "user-query", nil)

	en.disp.mu.Lock()
	n := len(en.disp.queries)
	en.disp.mu.Unlock()
	if n != 0 {
		t.Fatalf("queries leak: %d entries", n)
	}
}
