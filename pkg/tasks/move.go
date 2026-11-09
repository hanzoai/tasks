// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
)

// An engine that has kept its rows in shard files moves to a shared database once:
// MoveFiles copies every row of every shard under a data directory into the database,
// reads them back, and only then says so. The files are left where they are.
//
// What the rows do not carry is who owns the work in flight, because a file had one
// process and needed no owner. So an engine opened on the moved rows finds every open
// run without one, which Recover reads as work no live engine holds, and takes it up:
// the activities that were running are handed to a worker again and the runs owed a
// decision get one, exactly as after a restart.
//
// It is for the window in which one process still owns the files. A second process
// that opened them would be a second writer to rows already copied.

// MoveReport is what a move did.
type MoveReport struct {
	// Already is set when the rows had been moved before this call took its lock:
	// nothing was copied.
	Already bool          `json:"already,omitempty"`
	Tenants int           `json:"tenants"`
	Shards  int           `json:"shards"`
	Rows    int64         `json:"rows"`
	Bytes   int64         `json:"bytes"`
	Took    time.Duration `json:"took"`
}

const (
	movedKey  = "moved"
	movingKey = "moving"
	moveLease = "move"
	// movePage is how many rows are read from a shard, and written, at a time.
	movePage = 500
	// moveTerm is how long the move's lease lasts between renewals, which come with
	// every page.
	moveTerm = 2 * time.Minute
)

// ErrMoving is the answer when another process holds the move.
var ErrMoving = errors.New("tasks: another process is moving these rows")

// ErrForeign is the refusal to move files into a database that already holds an
// engine's rows that the move did not write: a replica with no files was born on it
// before the files were moved, and its rows and the files' cannot both be the engine.
var ErrForeign = errors.New("tasks: the database holds rows of an engine that did not come from these files")

// Moving reports whether a move of files into db has begun and not finished: an
// engine opened on the database now would be opened beside rows still being copied,
// or beside the remains of a move that stopped.
func Moving(ctx context.Context, db *sql.DB) (bool, error) {
	sh, err := storepkg.OpenShared(ctx, db)
	if err != nil {
		return false, err
	}
	if _, done, err := sh.Meta(ctx, movedKey); err != nil || done {
		return false, err
	}
	_, ok, err := sh.Meta(ctx, movingKey)
	return ok, err
}

// FilesIn reports whether dir holds shard files to move.
func FilesIn(dir string) bool { return storepkg.HasShards(dir) }

// Moved reports whether db already holds the rows that were moved into it.
func Moved(ctx context.Context, db *sql.DB) (bool, error) {
	sh, err := storepkg.OpenShared(ctx, db)
	if err != nil {
		return false, err
	}
	_, ok, err := sh.Meta(ctx, movedKey)
	return ok, err
}

// MoveFiles copies every row of the shard files under dir into db, then reads every
// tenant's rows back from db and holds their count and checksum to the files'. Only
// then does it record that the move is done; a failure records nothing, and the
// move starts again from the files, which it has not changed. master is the key the
// files are encrypted under, nil when the process already holds it.
func MoveFiles(ctx context.Context, db *sql.DB, dir string, master []byte) (*MoveReport, error) {
	sh, err := storepkg.OpenShared(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	}
	if prev, ok, err := sh.Meta(ctx, movedKey); err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	} else if ok {
		var r MoveReport
		_ = json.Unmarshal([]byte(prev), &r)
		r.Already = true
		return &r, nil
	}
	holder := newMemberID("move")
	held, err := sh.Claim(ctx, moveLease, holder, moveTerm)
	if err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	}
	if !held {
		return nil, ErrMoving
	}
	defer func() { _ = sh.Release(context.Background(), moveLease, holder) }()

	// Whose rows are in the database now? None, or the remains of an earlier attempt of
	// this move, which are written again from the files; anything else was written by an
	// engine that was born on the database, and is not overwritten or merged.
	if _, again, err := sh.Meta(ctx, movingKey); err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	} else if n, err := sh.Count(ctx); err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	} else if n > 0 && !again {
		return nil, fmt.Errorf("%w (%d rows)", ErrForeign, n)
	} else if again {
		if err := sh.Clear(ctx); err != nil {
			return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
		}
	}
	if err := sh.SetMeta(ctx, movingKey, nowRFC3339()); err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	}

	mgr, err := storepkg.New(dir, master)
	if err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: open %s: %w", dir, err)
	}
	defer mgr.Close()

	start := time.Now()
	rep := &MoveReport{}
	tenants, err := mgr.ListPrincipals(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	}
	for _, p := range tenants {
		shards, err := mgr.ListShards(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
		}
		var rows int64
		var sum uint64
		for _, shard := range shards {
			rep.Shards++
			after := ""
			for {
				var page []storepkg.Row
				err := shard.Scan(ctx, "", after, movePage, func(key string, value []byte) error {
					page = append(page, storepkg.Row{Key: key, Value: append([]byte(nil), value...)})
					return nil
				})
				if err != nil {
					return nil, fmt.Errorf("tasks.MoveFiles: read %s/%s: %w", p, shard.Namespace(), err)
				}
				if len(page) == 0 {
					break
				}
				if err := sh.PutMany(ctx, p, page); err != nil {
					return nil, fmt.Errorf("tasks.MoveFiles: write %s/%s: %w", p, shard.Namespace(), err)
				}
				for _, r := range page {
					rows, sum = rows+1, sum+storepkg.RowSum(r.Key, r.Value)
					rep.Bytes += int64(len(r.Key) + len(r.Value))
				}
				after = page[len(page)-1].Key
				if held, err := sh.Claim(ctx, moveLease, holder, moveTerm); err != nil || !held {
					return nil, fmt.Errorf("tasks.MoveFiles: the move's lease was lost: %w", errors.Join(err, ErrMoving))
				}
			}
		}
		n, got, err := sh.Digest(ctx, p)
		if err != nil {
			return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
		}
		if n != rows || got != sum {
			return nil, fmt.Errorf("tasks.MoveFiles: %s holds %d rows (sum %x) after the copy and the files held %d (sum %x)", p, n, got, rows, sum)
		}
		rep.Tenants++
		rep.Rows += rows
	}
	rep.Took = time.Since(start)
	body, _ := json.Marshal(rep)
	if err := sh.SetMeta(ctx, movedKey, string(body)); err != nil {
		return nil, fmt.Errorf("tasks.MoveFiles: %w", err)
	}
	return rep, nil
}
