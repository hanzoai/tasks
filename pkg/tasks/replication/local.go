// Copyright © 2026 Hanzo AI. MIT License.

package replication

import (
	"context"
	"sync"
)

// LocalReplicator is the single-node passthrough. Every Propose returns
// DecisionAccept synchronously and fans the frame out to local
// subscribers — the same code path the quasar driver uses on accept,
// so callers cannot tell the difference.
type LocalReplicator struct {
	mu          sync.RWMutex
	handlers    []Handler
	accepted    uint64
	rejected    uint64
	frameSeqMu  sync.Mutex
	nextSeq     uint64
}

// NewLocal returns a single-node Replicator.
func NewLocal() *LocalReplicator { return &LocalReplicator{} }

// Propose immediately accepts and dispatches.
func (r *LocalReplicator) Propose(ctx context.Context, f Frame) (Decision, error) {
	if f.Seq == 0 {
		r.frameSeqMu.Lock()
		r.nextSeq++
		f.Seq = r.nextSeq
		r.frameSeqMu.Unlock()
	}
	r.mu.RLock()
	hs := append([]Handler(nil), r.handlers...)
	r.mu.RUnlock()
	for _, h := range hs {
		if err := h(ctx, f); err != nil {
			r.mu.Lock()
			r.rejected++
			r.mu.Unlock()
			return DecisionReject, err
		}
	}
	r.mu.Lock()
	r.accepted++
	r.mu.Unlock()
	return DecisionAccept, nil
}

// Subscribe appends an applier; thread-safe with Propose.
func (r *LocalReplicator) Subscribe(h Handler) {
	r.mu.Lock()
	r.handlers = append(r.handlers, h)
	r.mu.Unlock()
}

// Close is a no-op; no resources held.
func (r *LocalReplicator) Close() error { return nil }

// Stats returns counters used by /v1/tasks/cluster.
func (r *LocalReplicator) Stats() (accepted, rejected uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.accepted, r.rejected
}
