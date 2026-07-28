// Copyright © 2026 Hanzo AI. MIT License.

package store_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/hanzoai/tasks/pkg/tasks/store"
)

// secret is the value the encryption tests look for on disk. It is long and
// distinctive so finding it is proof the page was written in the clear.
var secret = []byte("workflow-input-that-must-not-appear-on-disk-4a1f9c")

const sqliteHeader = "SQLite format 3\x00"

func master(b byte) []byte { return bytes.Repeat([]byte{b}, store.MasterKeyLen) }

// write puts secret into a tenant's shard and closes the manager so the file
// is sealed. It returns the shard's path.
func write(t *testing.T, dir string, key []byte, p store.Principal) string {
	t.Helper()
	mgr, err := store.New(dir, key)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	sh, err := mgr.Get(ctx, p, "default")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := sh.Put(ctx, "wf/default/a/1", secret); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := mgr.ShardPath(p, "default")
	if err := mgr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// Without a master key the shard is plaintext — which is what makes the
// encrypted case below a real measurement rather than a tautology.
func TestShard_PlaintextWithoutMaster(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, nil, store.Org("acme"))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !bytes.Contains(raw, secret) {
		t.Fatal("an unencrypted shard should hold the value verbatim; the probe is not measuring the file it thinks it is")
	}
	if !bytes.HasPrefix(raw, []byte(sqliteHeader)) {
		t.Fatal("an unencrypted shard should carry the SQLite header")
	}
}

// With a master key the file on disk is ciphertext: no SQLite header, and
// the value written through the shard appears nowhere in it.
func TestShard_EncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, master(0x11), store.Org("acme"))

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) == 0 {
		t.Fatal("shard file is empty")
	}
	if bytes.Contains(raw, secret) {
		t.Fatalf("%s holds the value in the clear", path)
	}
	if bytes.HasPrefix(raw, []byte(sqliteHeader)) {
		t.Fatalf("%s is a plaintext SQLite database", path)
	}
	if _, err := os.Stat(path + ".dek"); err != nil {
		t.Fatalf("wrapped DEK sidecar missing: %v", err)
	}
}

// The same master key reads it back; a different one cannot.
func TestShard_RoundTripsUnderItsMaster(t *testing.T) {
	dir := t.TempDir()
	p := store.Org("acme")
	write(t, dir, master(0x11), p)

	mgr, err := store.New(dir, master(0x11))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	sh, err := mgr.Get(ctx, p, "default")
	if err != nil {
		t.Fatalf("reopen under its own master: %v", err)
	}
	got, ok, err := sh.Get(ctx, "wf/default/a/1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("read %q, want %q", got, secret)
	}
	_ = mgr.Close()

	wrong, err := store.New(dir, master(0x22))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = wrong.Close() })
	if _, err := wrong.Get(ctx, p, "default"); err == nil {
		t.Fatal("a shard opened under the wrong master must fail, not return an empty database")
	}
}

// A sidecar is bound to its tenant: lifting one into another tenant's
// directory must fail the tag rather than unwrap.
func TestShard_SidecarBoundToItsTenant(t *testing.T) {
	dir := t.TempDir()
	key := master(0x11)
	victim := store.Org("acme")
	thief := store.Org("evil")
	victimPath := write(t, dir, key, victim)
	thiefPath := write(t, dir, key, thief)

	blob, err := os.ReadFile(victimPath + ".dek")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	// Give the thief the victim's shard AND its sidecar.
	body, err := os.ReadFile(victimPath)
	if err != nil {
		t.Fatalf("read shard: %v", err)
	}
	if err := os.WriteFile(thiefPath, body, 0o600); err != nil {
		t.Fatalf("plant shard: %v", err)
	}
	if err := os.WriteFile(thiefPath+".dek", blob, 0o600); err != nil {
		t.Fatalf("plant sidecar: %v", err)
	}

	mgr, err := store.New(dir, key)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := mgr.Get(context.Background(), thief, "default"); err == nil {
		t.Fatal("a sidecar lifted into another tenant's directory must not unwrap")
	}
}

// Every tenant's shard is encrypted, including the root one, and each under
// a key of its own.
func TestShard_EveryTenantEncrypted(t *testing.T) {
	dir := t.TempDir()
	key := master(0x33)
	tenants := []store.Principal{
		{},
		{Org: "acme"},
		{Org: "acme", Project: "web"},
		{Org: "acme", Project: "web", User: "z"},
	}
	seen := map[string]store.Principal{}
	for _, p := range tenants {
		path := write(t, dir, key, p)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if bytes.Contains(raw, secret) || bytes.HasPrefix(raw, []byte(sqliteHeader)) {
			t.Fatalf("%+v: %s is not encrypted", p, path)
		}
		blob, err := os.ReadFile(path + ".dek")
		if err != nil {
			t.Fatalf("%+v: sidecar missing: %v", p, err)
		}
		if prev, dup := seen[string(blob)]; dup {
			t.Fatalf("%+v and %+v share a wrapped DEK", prev, p)
		}
		seen[string(blob)] = p
	}
}

// A master key of the wrong size is refused where it enters, not at the
// first shard open.
func TestManager_MasterKeyLength(t *testing.T) {
	for _, n := range []int{1, 16, 31, 33, 64} {
		if _, err := store.New(t.TempDir(), bytes.Repeat([]byte{1}, n)); err == nil {
			t.Fatalf("a %d-byte master key should be refused", n)
		}
	}
	mgr, err := store.New(t.TempDir(), master(0x44))
	if err != nil {
		t.Fatalf("a %d-byte master key should be accepted: %v", store.MasterKeyLen, err)
	}
	if !mgr.Encrypted() {
		t.Fatal("Encrypted() should report a keyed manager")
	}
	_ = mgr.Close()
}
