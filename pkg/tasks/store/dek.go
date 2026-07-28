// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	hanzosqlite "github.com/hanzoai/sqlite"
)

// Every shard file is encrypted under its OWN data-encryption key (DEK),
// generated once when the file is created and never changed. The DEK is
// stored beside the file, wrapped under the key-encryption key its
// Principal derives from the master key (Principal.KEK):
//
//	<shard>.db     SQLCipher ciphertext, encrypted with the DEK
//	<shard>.dek    id(16) || AES-256-GCM(KEK, DEK)
//
// Rotating the master key rewraps the sidecar and leaves the ciphertext
// pages untouched, so rotation is O(1) per shard and cannot brick a file.
//
// The wrapped blob is bound to (Principal, id) as GCM additional data, and
// the id is random rather than derived from the path — so a shard survives
// moving the data directory, and a sidecar lifted into another tenant's
// directory fails the tag instead of unwrapping.

// dekSuffix names the sidecar beside a shard's .db file.
const dekSuffix = ".dek"

// idLen is the length of a shard's random identity, the value the wrapped
// DEK is bound to.
const idLen = 16

// principalShard tags the binding context of a wrapped shard DEK. The KEK
// chain already separates tenants; this separates one tenant's shards from
// each other.
const principalShard = hanzosqlite.PrincipalType("shard")

// dekPath returns the sidecar path for a shard file.
func dekPath(shard string) string { return shard + dekSuffix }

// shardDEK returns the data-encryption key for the shard file at path,
// creating it on first use. A nil master returns a nil key, which opens the
// shard unencrypted — the zero-config dev posture.
func shardDEK(path string, p Principal, master []byte) ([]byte, error) {
	if master == nil {
		return nil, nil
	}
	kek, err := p.KEK(master)
	if err != nil {
		return nil, err
	}
	side := dekPath(path)
	blob, err := os.ReadFile(side)
	switch {
	case err == nil:
		return unwrap(kek, p, blob)
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("store: read %s: %w", side, err)
	}

	dek, err := hanzosqlite.NewDEK()
	if err != nil {
		return nil, err
	}
	id := make([]byte, idLen)
	if _, err := rand.Read(id); err != nil {
		return nil, fmt.Errorf("store: shard id: %w", err)
	}
	wrapped, err := hanzosqlite.WrapDEK(kek, dek, shardAAD(p, id))
	if err != nil {
		return nil, err
	}
	blob = append(append(make([]byte, 0, idLen+len(wrapped)), id...), wrapped...)
	if err := writeNew(side, blob); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// Another opener created it first; theirs is authoritative.
		if blob, err = os.ReadFile(side); err != nil {
			return nil, fmt.Errorf("store: read %s: %w", side, err)
		}
		return unwrap(kek, p, blob)
	}
	return dek, nil
}

// unwrap opens a sidecar blob under p's KEK.
func unwrap(kek []byte, p Principal, blob []byte) ([]byte, error) {
	if len(blob) <= idLen {
		return nil, fmt.Errorf("store: wrapped DEK is %d bytes, too short", len(blob))
	}
	return hanzosqlite.UnwrapDEK(kek, blob[idLen:], shardAAD(p, blob[:idLen]))
}

// shardAAD is the binding context of a wrapped shard DEK: its tenant and
// its identity, in the injective length-prefixed encoding hanzoai/sqlite
// uses for every principal binding.
func shardAAD(p Principal, id []byte) []byte {
	return hanzosqlite.PrincipalAAD(principalShard, p.String()+"/"+hex.EncodeToString(id))
}

// writeNew writes b to path and fails with os.ErrExist if another writer
// got there first. The temp file lands in the destination directory so the
// rename is atomic.
func writeNew(path string, b []byte) error {
	if _, err := os.Stat(path); err == nil {
		return os.ErrExist
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("store: write %s: %w", path, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("store: chmod %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("store: sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", path, err)
	}
	if _, err := os.Stat(path); err == nil {
		return os.ErrExist
	}
	return os.Rename(tmp.Name(), path)
}
