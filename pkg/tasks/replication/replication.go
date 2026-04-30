// Copyright © 2026 Hanzo AI. MIT License.

// Package replication is the consensus-replication boundary for the
// Tasks shard manager. Drivers (local, quasar) implement Replicator;
// the shard manager calls Propose before committing every mutation and
// installs Subscribe handlers to apply frames received from peers.
//
// The Decision values mirror github.com/luxfi/consensus core/types so
// the quasar driver can pass them through without reshaping.
package replication

import (
	"context"
	"errors"
)

// Decision is the consensus outcome for a proposed frame.
type Decision uint8

const (
	DecisionUndecided Decision = iota
	DecisionAccept
	DecisionReject
	DecisionTimeout
)

// ErrRejected and ErrTimeout normalize Replicator failure modes.
var (
	ErrRejected = errors.New("replication: frame rejected by quorum")
	ErrTimeout  = errors.New("replication: consensus timeout")
)

// Frame is the wire envelope a Shard ships through consensus. The
// payload is the application-level mutation; ShardKey routes the
// frame to the right namespace shard on apply.
type Frame struct {
	OrgID     string `json:"org"`
	Namespace string `json:"ns"`
	Seq       uint64 `json:"seq"`
	Op        string `json:"op"`  // "put" | "del"
	Key       string `json:"key"` // canonical key (no org prefix)
	Value     []byte `json:"val,omitempty"`
}

// Handler applies an accepted frame to the local shard.
type Handler func(ctx context.Context, f Frame) error

// Replicator is the interface every driver implements. Propose blocks
// until the cluster decides; Subscribe registers an applier called for
// every accepted frame including those originated locally (so a single
// code path persists, regardless of leader).
type Replicator interface {
	// Propose ships frame through consensus and returns the decision.
	Propose(ctx context.Context, frame Frame) (Decision, error)
	// Subscribe registers a handler invoked once per accepted frame.
	Subscribe(h Handler)
	// Close releases driver resources.
	Close() error
}
