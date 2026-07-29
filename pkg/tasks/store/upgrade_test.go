// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the move against a REAL store: data is written through
// a manager, the tree is pushed back into the pre-upgrade shape, and the
// same data has to come back out afterwards. Asserting on paths alone would
// prove the files landed somewhere, not that the engine can read them.

// oldLayout writes value at key for each principal/namespace pair, closes
// the manager, then lifts every shard file two levels up — which is exactly
// the tree the older binary left behind. Returns the root.
func oldLayout(t *testing.T, shards map[Principal]string, key, value string) string {
	t.Helper()
	root := t.TempDir()
	m, err := New(root, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for p, ns := range shards {
		s, err := m.Get(context.Background(), p, ns)
		if err != nil {
			t.Fatalf("Get(%s/%s): %v", p, ns, err)
		}
		if err := s.Put(context.Background(), key, []byte(value)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for p := range shards {
		deep := filepath.Join(root, p.dir())     // <root>/<org>/_/_
		flat := filepath.Dir(filepath.Dir(deep)) // <root>/<org>
		entries, err := os.ReadDir(deep)
		if err != nil {
			t.Fatalf("ReadDir(%s): %v", deep, err)
		}
		for _, e := range entries {
			if err := os.Rename(filepath.Join(deep, e.Name()), filepath.Join(flat, e.Name())); err != nil {
				t.Fatalf("flatten %s: %v", e.Name(), err)
			}
		}
		if err := os.RemoveAll(filepath.Join(flat, Sentinel)); err != nil {
			t.Fatalf("RemoveAll: %v", err)
		}
	}
	return root
}

// readBack opens a fresh manager on root and returns the value at key.
func readBack(t *testing.T, root string, p Principal, ns, key string) string {
	t.Helper()
	m, err := New(root, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Close()
	s, err := m.Get(context.Background(), p, ns)
	if err != nil {
		t.Fatalf("Get(%s/%s): %v", p, ns, err)
	}
	v, ok, err := s.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("Get(%s): %v", key, err)
	}
	if !ok {
		return ""
	}
	return string(v)
}

// TestUpgradeRestoresPreUpgradeShards is the whole point: a store the older
// binary wrote is invisible to this one — the manager creates empty shards
// beside the full ones and the sweeper enumerates nothing — until the move
// runs. Both tenant shapes are covered, an org and the root, because the
// root's directory is itself the sentinel and is the easy one to get wrong.
func TestUpgradeRestoresPreUpgradeShards(t *testing.T) {
	shards := map[Principal]string{
		Org("acme"): "default",
		{}:          "default",
	}
	root := oldLayout(t, shards, "sc/default/nightly", "backup")

	// The premise: before the move this store has no tenants at all, which
	// is what stops the sweeper. Asserted before anything opens a shard,
	// because opening one CREATES it — that is the whole failure mode.
	m, err := New(root, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ps, err := m.ListPrincipals(context.Background())
	m.Close()
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	if len(ps) != 0 {
		t.Fatalf("pre-move principals = %v, want none — the sweeper sees nothing", ps)
	}

	moves, err := Upgrade(root, nil)
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if len(moves) != 2 {
		t.Fatalf("moves = %+v, want one per shard", moves)
	}
	for _, mv := range moves {
		if mv.Skipped {
			t.Fatalf("move %+v was skipped on a clean tree", mv)
		}
	}

	for p, ns := range shards {
		if got := readBack(t, root, p, ns, "sc/default/nightly"); got != "backup" {
			t.Fatalf("post-move read for %s = %q, want backup", p, got)
		}
	}
	m2, err := New(root, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m2.Close()
	ps, err = m2.ListPrincipals(context.Background())
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	if len(ps) != 2 {
		t.Fatalf("post-move principals = %v, want acme and root", ps)
	}
}

// TestUpgradeCarriesTheJournal proves a shard's WAL travels with it. A
// database that lands in its new home without the journal holding its last
// commits has silently lost writes, which is worse than not moving at all.
func TestUpgradeCarriesTheJournal(t *testing.T) {
	root := oldLayout(t, map[Principal]string{Org("acme"): "default"}, "k", "v")
	wal := filepath.Join(root, "acme", "default.db-wal")
	if err := os.WriteFile(wal, []byte("journal"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Upgrade(root, nil); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "acme", Sentinel, Sentinel, "default.db-wal")); err != nil {
		t.Fatalf("journal did not travel with its database: %v", err)
	}
	if _, err := os.Stat(wal); !os.IsNotExist(err) {
		t.Fatalf("journal left behind at %s (err=%v)", wal, err)
	}
}

// TestUpgradeIsIdempotent: running it again on a tree already in this
// layout is a scan and nothing else, and the data is untouched.
func TestUpgradeIsIdempotent(t *testing.T) {
	root := oldLayout(t, map[Principal]string{Org("acme"): "default"}, "k", "v")
	if _, err := Upgrade(root, nil); err != nil {
		t.Fatalf("first Upgrade: %v", err)
	}
	moves, err := Upgrade(root, nil)
	if err != nil {
		t.Fatalf("second Upgrade: %v", err)
	}
	if len(moves) != 0 {
		t.Fatalf("second run moved %+v, want nothing", moves)
	}
	if got := readBack(t, root, Org("acme"), "default", "k"); got != "v" {
		t.Fatalf("read after two runs = %q, want v", got)
	}
}

// TestUpgradeRefusesAnOccupiedTarget covers the collision an operator will
// actually hit: the new binary BOOTED before the move ran, so it created an
// empty shard exactly where the full one belongs. Overwriting would discard
// whatever was started after that boot; skipping quietly would leave the
// store looking upgraded and still empty. So neither: both files survive,
// the run continues for every other tenant, and the error says what to do.
func TestUpgradeRefusesAnOccupiedTarget(t *testing.T) {
	shards := map[Principal]string{Org("acme"): "default", Org("globex"): "default"}
	root := oldLayout(t, shards, "k", "real")

	// The boot. Reading through a manager creates <root>/acme/_/_/default.db.
	if got := readBack(t, root, Org("acme"), "default", "k"); got != "" {
		t.Fatalf("the empty shard read %q, want empty", got)
	}

	moves, err := Upgrade(root, nil)
	if !errors.Is(err, ErrOccupied) {
		t.Fatalf("Upgrade err = %v, want ErrOccupied", err)
	}
	if !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("error %q does not say what to do", err)
	}

	var skipped, moved int
	for _, m := range moves {
		if m.Skipped {
			skipped++
			continue
		}
		moved++
	}
	if skipped != 1 || moved != 1 {
		t.Fatalf("moves = %+v, want acme skipped and globex moved", moves)
	}
	// Neither file was consumed.
	if _, err := os.Stat(filepath.Join(root, "acme", "default.db")); err != nil {
		t.Fatalf("source was consumed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "acme", Sentinel, Sentinel, "default.db")); err != nil {
		t.Fatalf("destination was consumed: %v", err)
	}
	// Isolation: the collision did not cost the other tenant its move.
	if got := readBack(t, root, Org("globex"), "default", "k"); got != "real" {
		t.Fatalf("globex read = %q, want real", got)
	}
}

// TestUpgradeRefusesAMasterKey: the move does not encrypt, so running it
// with a key set would produce the one state that cannot be recovered by
// running it again — a plaintext file wrapped in a fresh DEK sidecar and
// then opened as ciphertext. It refuses, says why, and touches nothing.
func TestUpgradeRefusesAMasterKey(t *testing.T) {
	root := oldLayout(t, map[Principal]string{Org("acme"): "default"}, "k", "v")
	_, err := Upgrade(root, make([]byte, MasterKeyLen))
	if err == nil {
		t.Fatal("Upgrade with a master key returned nil")
	}
	for _, want := range []string{"master key", "plaintext"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not say %q", err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "acme", "default.db")); err != nil {
		t.Fatalf("refusal moved something anyway: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "acme", Sentinel)); !os.IsNotExist(err) {
		t.Fatalf("refusal created %s (err=%v)", Sentinel, err)
	}
}

// TestUpgradeLeavesNonTenantDirsAlone: the rule is "a directory holding a
// shard file directly is a pre-upgrade tenant". _migrations/<id>/<ns>.db
// keeps its shards one level further down, so it is not one, and the rule
// leaves it alone without knowing its name.
func TestUpgradeLeavesNonTenantDirsAlone(t *testing.T) {
	root := oldLayout(t, map[Principal]string{Org("acme"): "default"}, "k", "v")
	job := filepath.Join(root, "_migrations", "mig-1")
	if err := os.MkdirAll(job, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	copied := filepath.Join(job, "default.db")
	if err := os.WriteFile(copied, []byte("copy"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := Upgrade(root, nil); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("migration copy moved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "_migrations", Sentinel)); !os.IsNotExist(err) {
		t.Fatalf("_migrations was treated as a tenant (err=%v)", err)
	}
}
