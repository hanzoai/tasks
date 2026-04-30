// Copyright © 2026 Hanzo AI. MIT License.

// Package routing implements sticky leader selection for
// (org, namespace, taskQueue) triples. The leader is the validator
// whose hash(org|ns|tq|seed) is smallest in the current
// validator set; on membership change the table is recomputed and a
// MembershipChanged event lets the dispatcher re-target subscribers.
package routing

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"sync"
)

// NodeID is the validator id used by the router. Wire is opaque; in
// practice this is the same identifier the consensus engine uses.
type NodeID string

// Key uniquely identifies a leader assignment.
type Key struct {
	Org       string
	Namespace string
	TaskQueue string
}

// Router picks a deterministic leader for every Key.
type Router interface {
	LeaderFor(k Key) (NodeID, bool)
	IsLeader(k Key) bool
	Validators() []NodeID
	SetMembership(local NodeID, validators []NodeID)
	OnChange(fn func())
}

// hashRouter is the consistent-hash implementation. Picks the validator
// with the smallest sha256(key|seed|node) — equivalent to one round of
// rendezvous hashing, which gives 1/N reassignment on membership
// change with no virtual-node overhead.
type hashRouter struct {
	mu          sync.RWMutex
	local       NodeID
	validators  []NodeID
	subscribers []func()
	seed        []byte
}

// NewHash returns a deterministic leader router seeded by seed (any
// stable bytes; cluster id, k8s service name, etc.).
func NewHash(seed []byte) Router {
	cp := append([]byte(nil), seed...)
	return &hashRouter{seed: cp}
}

// SetMembership replaces the validator set and broadcasts a change event.
func (r *hashRouter) SetMembership(local NodeID, validators []NodeID) {
	cp := append([]NodeID(nil), validators...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	r.mu.Lock()
	r.local = local
	r.validators = cp
	subs := append([]func(){}, r.subscribers...)
	r.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

// LeaderFor returns the validator for k, false if no validators are known.
func (r *hashRouter) LeaderFor(k Key) (NodeID, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.validators) == 0 {
		return "", false
	}
	var best NodeID
	var bestScore uint64
	for i, v := range r.validators {
		score := scoreOf(k, r.seed, v)
		if i == 0 || score < bestScore {
			bestScore = score
			best = v
		}
	}
	return best, true
}

// IsLeader reports whether the local node owns k.
func (r *hashRouter) IsLeader(k Key) bool {
	leader, ok := r.LeaderFor(k)
	if !ok {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return leader == r.local
}

// Validators returns the cluster snapshot.
func (r *hashRouter) Validators() []NodeID {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := append([]NodeID(nil), r.validators...)
	return out
}

// OnChange registers a callback fired after every SetMembership.
func (r *hashRouter) OnChange(fn func()) {
	r.mu.Lock()
	r.subscribers = append(r.subscribers, fn)
	r.mu.Unlock()
}

// scoreOf is the deterministic hash function. Uses sha256 truncated to
// uint64 — collision probability across realistic cluster sizes is
// negligible and the cost is dwarfed by the consensus round.
func scoreOf(k Key, seed []byte, node NodeID) uint64 {
	h := sha256.New()
	h.Write([]byte(k.Org))
	h.Write([]byte{0})
	h.Write([]byte(k.Namespace))
	h.Write([]byte{0})
	h.Write([]byte(k.TaskQueue))
	h.Write([]byte{0})
	h.Write(seed)
	h.Write([]byte{0})
	h.Write([]byte(node))
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}
