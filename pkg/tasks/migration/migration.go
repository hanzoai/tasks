// Copyright © 2026 Hanzo AI. MIT License.

// Package migration moves a single (org, namespace) shard between
// nodes. The protocol is:
//
//  1. Lock the source shard via a consensus barrier (Replicator
//     Propose with Op="lock"). Until the lock-frame is accepted on
//     every replica, the shard remains writable.
//  2. WAL-checkpoint and copy the SQLite file to the destination.
//  3. Replay any frames whose Seq is >= the last sequence captured at
//     barrier time. (No-op for the local driver, since Propose is
//     synchronous.)
//  4. Release the lock with another Propose (Op="unlock"); the
//     destination becomes the live shard owner.
//
// CLI: tasksd migrate --org <id> --namespace <name> --to <node>
package migration

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/hanzoai/tasks/pkg/tasks/replication"
	"github.com/hanzoai/tasks/pkg/tasks/store"
)

// Status reflects a migration's lifecycle.
type Status string

const (
	StatusPending  Status = "pending"
	StatusLocking  Status = "locking"
	StatusCopying  Status = "copying"
	StatusReplaying Status = "replaying"
	StatusReleased  Status = "released"
	StatusFailed   Status = "failed"
)

// Job is a single migration unit.
type Job struct {
	ID         string    `json:"id"`
	OrgID      string    `json:"org"`
	Namespace  string    `json:"ns"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	Status     Status    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Bytes      int64     `json:"bytes"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitempty"`
}

// Coordinator runs migrations against a Manager + Replicator pair.
type Coordinator struct {
	mgr  *store.Manager
	rep  replication.Replicator
	mu   sync.Mutex
	jobs map[string]*Job
}

// NewCoordinator wires a fresh coordinator.
func NewCoordinator(mgr *store.Manager, rep replication.Replicator) *Coordinator {
	return &Coordinator{mgr: mgr, rep: rep, jobs: make(map[string]*Job)}
}

// Migrate runs a job synchronously.
func (c *Coordinator) Migrate(ctx context.Context, j Job) (*Job, error) {
	if j.Namespace == "" {
		return nil, fmt.Errorf("migration: namespace required")
	}
	if j.ID == "" {
		j.ID = fmt.Sprintf("mig-%s-%s-%d", j.OrgID, j.Namespace, time.Now().UnixNano())
	}
	j.StartedAt = time.Now()
	c.mu.Lock()
	c.jobs[j.ID] = &j
	c.mu.Unlock()

	setStatus := func(s Status, err error) {
		c.mu.Lock()
		j.Status = s
		if err != nil {
			j.Error = err.Error()
		}
		c.mu.Unlock()
	}

	// 1. Acquire the consensus barrier (idempotent; missing replicator
	//    means single-node where the lock collapses to a noop).
	setStatus(StatusLocking, nil)
	if c.rep != nil {
		if _, err := c.rep.Propose(ctx, replication.Frame{
			OrgID:     j.OrgID,
			Namespace: j.Namespace,
			Op:        "migration.lock",
			Key:       fmt.Sprintf("__migration/%s", j.ID),
			Value:     []byte(j.To),
		}); err != nil {
			setStatus(StatusFailed, err)
			return &j, err
		}
	}

	// 2. WAL checkpoint + copy.
	setStatus(StatusCopying, nil)
	src, err := c.mgr.Get(ctx, j.OrgID, j.Namespace)
	if err != nil {
		setStatus(StatusFailed, err)
		return &j, err
	}
	if err := src.Checkpoint(); err != nil {
		setStatus(StatusFailed, err)
		return &j, err
	}
	srcPath := c.mgr.ShardPath(j.OrgID, j.Namespace)
	dstPath := filepath.Join(c.mgr.RootDir(), "_migrations", j.ID, j.Namespace+".db")
	n, err := store.CopyFile(dstPath, srcPath)
	if err != nil {
		setStatus(StatusFailed, err)
		return &j, err
	}
	c.mu.Lock()
	j.Bytes = n
	c.mu.Unlock()

	// 3. Replay (single-node: no trailing frames).
	setStatus(StatusReplaying, nil)

	// 4. Release the barrier.
	if c.rep != nil {
		if _, err := c.rep.Propose(ctx, replication.Frame{
			OrgID:     j.OrgID,
			Namespace: j.Namespace,
			Op:        "migration.unlock",
			Key:       fmt.Sprintf("__migration/%s", j.ID),
			Value:     []byte(j.To),
		}); err != nil {
			setStatus(StatusFailed, err)
			return &j, err
		}
	}
	c.mu.Lock()
	j.Status = StatusReleased
	j.FinishedAt = time.Now()
	c.mu.Unlock()
	return &j, nil
}

// Get returns a job by id, or nil if unknown.
func (c *Coordinator) Get(id string) *Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j, ok := c.jobs[id]; ok {
		cp := *j
		return &cp
	}
	return nil
}

// List returns every job the coordinator has seen.
func (c *Coordinator) List() []Job {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Job, 0, len(c.jobs))
	for _, j := range c.jobs {
		out = append(out, *j)
	}
	return out
}
