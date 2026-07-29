// Copyright © 2026 Hanzo AI. MIT License.

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The one-time move a store written by an older binary needs before this
// one can read it. That binary addressed a shard by org alone:
//
//	<root>/<org>/<ns>.db
//	<root>/_/<ns>.db        the unscoped embedded / dev tenant
//
// This one addresses it by the whole Principal, three levels deep, every
// unset leg written as the sentinel (see principal.go):
//
//	<root>/<org>/_/_/<ns>.db
//	<root>/_/_/_/<ns>.db
//
// So the move inserts two sentinel levels, uniformly. Nothing is read,
// re-encoded or re-keyed — a rename within one filesystem, not a page
// rewritten.
//
// Without it the manager finds nothing where it looks and creates empty
// shards beside the full ones: every mid-flight execution, every schedule
// and every namespace ceases to exist while its bytes sit one directory
// up, and the sweeper enumerates zero tenants, so no schedule fires again.
// Nothing is destroyed and the move brings all of it back.

// Move is one file's relocation.
type Move struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Skipped is set when the destination was already occupied. Both files
	// survive untouched and Upgrade returns ErrOccupied, because only an
	// operator can say which one is authoritative: the destination can only
	// have got there by this binary booting before the move ran, which
	// creates an EMPTY shard where the full one belongs — and anything
	// started after that boot is in the empty one.
	Skipped bool `json:"skipped,omitempty"`
}

// ErrOccupied reports that at least one shard had a file at its
// destination. Nothing was overwritten and nothing was deleted; the run
// carried on past it, so the other tenants still moved.
var ErrOccupied = errors.New("store: a shard already had a file at its destination")

// Upgrade moves every shard under root into the principal-addressed layout
// and reports what it moved. Idempotent: a moved shard leaves nothing at
// its old path, and a directory already in this layout holds no shard file
// at its top level, so a re-run costs a scan and nothing else.
//
// master must be nil. A store written by the older binary is PLAINTEXT —
// encryption arrived with this layout — and moving a file does not encrypt
// it. Boot once with the key unset and the shards read; boot with the key
// set and each one gets a fresh DEK sidecar and is then opened as
// SQLCipher ciphertext, which it is not. Move first, encrypt second.
func Upgrade(root string, master []byte) ([]Move, error) {
	if master != nil {
		return nil, fmt.Errorf("store.Upgrade: refusing to move %s while a master key is set: "+
			"shards written before this layout are plaintext and moving them does not encrypt them, "+
			"so the next boot would wrap a fresh DEK around a file it then failed to decrypt — "+
			"run the move with the key unset", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store.Upgrade: read %s: %w", root, err)
	}
	var moves []Move
	occupied := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return moves, fmt.Errorf("store.Upgrade: read %s: %w", dir, err)
		}
		names := shardFiles(files)
		if names == nil {
			continue
		}
		dst := filepath.Join(dir, Sentinel, Sentinel)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return moves, fmt.Errorf("store.Upgrade: mkdir %s: %w", dst, err)
		}
		for _, name := range names {
			m := Move{From: filepath.Join(dir, name), To: filepath.Join(dst, name)}
			if _, err := os.Stat(m.To); err == nil {
				// Isolation, like the cron sweep's: one collision must not
				// cost every other tenant its move. Reported at the end.
				m.Skipped, occupied = true, occupied+1
			} else if err := os.Rename(m.From, m.To); err != nil {
				return moves, fmt.Errorf("store.Upgrade: move %s: %w", m.From, err)
			}
			moves = append(moves, m)
		}
	}
	if occupied > 0 {
		return moves, fmt.Errorf("%w: %d of %d; this binary booted before the move ran and created "+
			"empty shards where the full ones belong. Nothing was overwritten or deleted. Stop the "+
			"service, decide which file of each pair is authoritative, remove the other, and re-run",
			ErrOccupied, occupied, len(moves))
	}
	return moves, nil
}

// shardFiles returns what to move out of a pre-upgrade tenant directory,
// journals before their database, or nil when the directory holds no shard
// at all — which is how a directory already in this layout, and
// _migrations/, are left alone without naming either of them.
//
// Journals first because a run that stops half way must never leave a
// database in its new home without the WAL holding its last commits. The
// engine reads only the new layout, so a database left behind is merely
// unmoved and a re-run finishes the job; a database moved without its WAL
// has lost writes.
func shardFiles(files []os.DirEntry) []string {
	var dbs, journals []string
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if strings.HasSuffix(f.Name(), shardSuffix) {
			dbs = append(dbs, f.Name())
			continue
		}
		journals = append(journals, f.Name())
	}
	if len(dbs) == 0 {
		return nil
	}
	return append(journals, dbs...)
}
