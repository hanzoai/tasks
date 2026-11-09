// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	mrand "math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	storepkg "github.com/hanzoai/tasks/pkg/tasks/store"
)

// cluster is what one engine needs to know about the other engines that run on the
// same rows.
//
// There are two answers and the engine never asks which it has. A process that owns
// its files is alone: it holds its own locks, it leads everything, and the only
// engine alive is itself (solo). A process that shares a database with its
// replicas is one of several (replica): a lock is a lease every replica sees, a
// leadership is a lease held by one, and an engine is alive while it renews the
// lease that is its membership.
type cluster interface {
	// self names this engine, once per process incarnation: a restart is a new engine,
	// and what the old one owed is owed by nobody until someone adopts it.
	self() string
	// lock excludes every other holder of key, in this process and in every replica,
	// until the returned func is called. It waits as long as it takes, and a holder
	// that dies does not hold it for ever (see lockTTL).
	lock(key string) (unlock func())
	// live is the engines alive now, this one included.
	live() (map[string]bool, error)
	// lead reports whether this engine holds the named leadership right now. Asking
	// renews it; a leader that stops asking loses it within leadTTL.
	lead(name string) bool
}

// solo is the cluster of one process.
type solo struct {
	id string
	mu *keyedMutex
}

func newSolo() *solo { return &solo{id: newMemberID("solo"), mu: newKeyedMutex()} }

func (s *solo) self() string                   { return s.id }
func (s *solo) lock(key string) func()         { return s.mu.lock(key) }
func (s *solo) live() (map[string]bool, error) { return map[string]bool{s.id: true}, nil }
func (s *solo) lead(string) bool               { return true }

func newMemberID(node string) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return node + "-" + hex.EncodeToString(b[:])
}

// timing is the terms of the leases a replica holds, and how often it renews them,
// all in the database's clock.
type timing struct {
	// sweep is how often the scheduler ticks.
	sweep time.Duration
	// beat is how often a member renews its membership; member is how long it stays
	// alive after the last renewal. Two missed beats never drop a live engine, and a
	// dead one is adopted within member + beat.
	beat, member time.Duration
	// lead is a leadership's term, renewed by asking for it every sweep.
	lead time.Duration
	// lock bounds a lock whose holder died. A critical section is a handful of
	// statements; it is not a lease anyone renews, and what a holder that outlived
	// its term could do is bounded by the epoch every dispatch is fenced with.
	lock time.Duration
}

var defaultTiming = timing{sweep: 5 * time.Second, beat: 5 * time.Second, member: 15 * time.Second, lead: 15 * time.Second, lock: 20 * time.Second}

const (
	memberPrefix = "member/"
	leadPrefix   = "lead/"
	lockPrefix   = "lock/"
)

// replica is the cluster of an engine that shares its database with others.
type replica struct {
	sh  *storepkg.Shared
	id  string
	log *slog.Logger
	t   timing

	ctx     context.Context
	stop    context.CancelFunc
	beating atomic.Bool // beat has started
	done    chan struct{}

	mu  *keyedMutex // locks taken inside this process wait here, and do not ask the database
	seq atomic.Uint64
}

// joinCluster makes this engine a member of the deployment and keeps it one until
// stop. The membership is written before it returns, so nothing the engine owns is
// ever stamped with an id that is not alive.
func joinCluster(ctx context.Context, sh *storepkg.Shared, node string, log *slog.Logger, t timing) (*replica, error) {
	r := &replica{sh: sh, id: newMemberID(node), log: log, t: t, mu: newKeyedMutex(), done: make(chan struct{})}
	r.ctx, r.stop = context.WithCancel(context.Background())
	if _, err := sh.Claim(ctx, memberPrefix+r.id, r.id, t.member); err != nil {
		r.stop()
		return nil, err
	}
	return r, nil
}

func (r *replica) self() string { return r.id }

// beat renews the membership every beatEvery and, whenever a member's term has
// lapsed, calls adopt with the lapsed memberships: the work those engines owned is
// now owed by whoever sees it first. It returns when the replica leaves.
func (r *replica) beat(adopt func(lapsed []string)) {
	r.beating.Store(true)
	defer close(r.done)
	tick := time.NewTicker(r.t.beat)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-tick.C:
		}
		if _, err := r.sh.Claim(r.ctx, memberPrefix+r.id, r.id, r.t.member); err != nil && r.ctx.Err() == nil {
			r.log.Warn("tasks: membership not renewed; peers adopt this engine's work when its term ends", "member", r.id, "error", err)
		}
		lapsed, err := r.sh.Lapsed(r.ctx, memberPrefix)
		switch {
		case err != nil && r.ctx.Err() == nil:
			r.log.Warn("tasks: lapsed members cannot be read", "member", r.id, "error", err)
		case len(lapsed) > 0:
			adopt(lapsed)
		}
		if n++; n%24 == 0 { // every two minutes: the locks of holders that died
			_ = r.sh.Forget(r.ctx, lockPrefix, time.Hour)
		}
	}
}

// leave ends the membership now and not at its term: the lease is expired, so a
// peer's next beat finds it lapsed and adopts what this engine owed.
func (r *replica) leave() {
	r.halt()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.sh.Expire(ctx, memberPrefix+r.id, r.id); err != nil {
		r.log.Warn("tasks: membership not given up; it lapses by itself", "member", r.id, "error", err)
	}
}

// haltWait is how long halt waits for the heartbeat to finish a pass it is in. A pass
// that takes up a dead engine's work can be long, and the process stopping must not
// wait for it: the store closing under it ends it.
const haltWait = 3 * time.Second

// halt ends the heartbeat and everything this replica waits for, and leaves the
// membership to lapse.
func (r *replica) halt() {
	r.stop()
	if r.beating.Load() {
		select {
		case <-r.done:
		case <-time.After(haltWait):
		}
	}
}

// retire clears the lapsed memberships that adoption has finished with.
func (r *replica) retire(names []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, name := range names {
		_ = r.sh.Release(ctx, name, strings.TrimPrefix(name, memberPrefix))
	}
}

func (r *replica) live() (map[string]bool, error) {
	names, err := r.sh.Held(r.ctx, memberPrefix)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names)+1)
	for _, n := range names {
		out[strings.TrimPrefix(n, memberPrefix)] = true
	}
	out[r.id] = true
	return out, nil
}

func (r *replica) lead(name string) bool {
	ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	held, err := r.sh.Claim(ctx, leadPrefix+name, r.id, r.t.lead)
	return err == nil && held
}

// lockName is the lease a lock is. The key is hashed: it holds a workflow id, which is
// whatever a caller chose, and a lease name is text that the database holds.
func lockName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return lockPrefix + hex.EncodeToString(sum[:16])
}

// lock waits for the lease on key as long as its holder's term, and no longer than
// twice that when the database does not answer: a critical section whose lock could
// not be had goes ahead, and its own writes fail the same way, where waiting for ever
// would hold every caller behind a database that is gone.
func (r *replica) lock(key string) func() {
	unlockLocal := r.mu.lock(key)
	name := lockName(key)
	holder := r.id + "/" + strconv.FormatUint(r.seq.Add(1), 10)
	wait := 2 * time.Millisecond
	var failing time.Time
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		held, err := r.sh.Claim(ctx, name, holder, r.t.lock)
		cancel()
		if held && err == nil {
			break
		}
		if err != nil {
			if failing.IsZero() {
				failing = time.Now()
			}
			if time.Since(failing) > 2*r.t.lock {
				r.log.Error("tasks: a lock could not be taken; going ahead without it", "key", key, "error", err)
				return unlockLocal
			}
		} else {
			failing = time.Time{}
		}
		time.Sleep(wait + time.Duration(mrand.IntN(int(wait))))
		wait = min(wait*2, 100*time.Millisecond)
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.sh.Release(ctx, name, holder)
		unlockLocal()
	}
}
