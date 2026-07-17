// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// TestStore_ConcurrentCrossOrgNoRace guards the fix for a production crash:
// `fatal error: concurrent map writes` at store.shardFor. The prior store held
// an unread `bootNS` set guarded by a value `mu`; withOrg shared the map by
// reference but COPIED the mutex, so N org-scoped views each locked a DIFFERENT
// mutex while writing the ONE shared map — no mutual exclusion → the runtime
// fataled (crashing the whole cloud process / api.hanzo.ai). The dead set is
// gone; this exercises the exact pattern (many org views hitting shardFor at
// once) so the crash can never regress. Run with -race for good measure; the
// original bug crashed the binary outright, not merely tripped the detector.
func TestStore_ConcurrentCrossOrgNoRace(t *testing.T) {
	s, err := newStoreFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	defer s.close()

	const orgs, iters = 12, 40
	var wg sync.WaitGroup
	for o := 0; o < orgs; o++ {
		wg.Add(1)
		go func(o int) {
			defer wg.Done()
			os := s.withOrg(fmt.Sprintf("org-%d", o))
			for i := 0; i < iters; i++ {
				// put → shardFor: the exact path that raced across org views.
				if err := os.put(fmt.Sprintf("ns/probe-%d", i), map[string]any{"i": i}); err != nil {
					t.Errorf("org-%d put: %v", o, err)
					return
				}
			}
		}(o)
	}
	wg.Wait()
}

// TestStore_FactoryDefault — TASKSD_STORE unset → sqlite shards.
func TestStore_FactoryDefault(t *testing.T) {
	t.Setenv("TASKSD_STORE", "")
	s, err := newStoreFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	defer s.close()
	// Routing requires a namespaced key.
	if err := s.put("ns/probe", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// TestStore_PersistsAcrossOpen confirms that sqlite-backed shards
// survive close+reopen — the workflow-state-after-restart property.
func TestStore_PersistsAcrossOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks-persist")

	s1, err := newStoreFromEnv(dir)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	en1 := newEngine(s1)
	if err := en1.RegisterNamespace(Namespace{
		NamespaceInfo: NamespaceInfo{Name: "persist", State: "NAMESPACE_STATE_REGISTERED"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := en1.StartWorkflow("persist", "wf-keep", "", TypeRef{Name: "Demo"}, "q", nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = s1.close()

	s2, err := newStoreFromEnv(dir)
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	defer s2.close()
	en2 := newEngine(s2)
	rows, err := en2.ListNamespaces()
	if err != nil {
		t.Fatalf("list ns: %v", err)
	}
	gotNS := false
	for _, n := range rows {
		if n.NamespaceInfo.Name == "persist" {
			gotNS = true
			break
		}
	}
	if !gotNS {
		t.Fatalf("namespace lost across restart")
	}
	wfs, err := en2.ListWorkflows("persist")
	if err != nil {
		t.Fatalf("list wf: %v", err)
	}
	if len(wfs) != 1 || wfs[0].Execution.WorkflowId != "wf-keep" {
		t.Fatalf("workflow lost across restart: %+v", wfs)
	}
}
