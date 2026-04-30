// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"path/filepath"
	"testing"
)

// TestStore_FactoryDefault — TASKSD_STORE unset → memdb.
func TestStore_FactoryDefault(t *testing.T) {
	t.Setenv("TASKSD_STORE", "")
	s, err := newStoreFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	defer s.close()
	if err := s.put("k", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}
}

// TestStore_ZapdbPersistsAcrossOpen confirms that zapdb-backed stores
// survive close+reopen — the workflow-state-after-restart property the
// audit flagged as P0.
func TestStore_ZapdbPersistsAcrossOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tasks-persist")
	t.Setenv("TASKSD_STORE", "zapdb")

	// First engine: register a namespace, start a workflow.
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

	// Second engine on the same dir: namespace + workflow must reappear.
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
