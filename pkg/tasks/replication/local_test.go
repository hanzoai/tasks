// Copyright © 2026 Hanzo AI. MIT License.

package replication_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/hanzoai/tasks/pkg/tasks/replication"
)

func TestLocal_AcceptsAndDispatches(t *testing.T) {
	r := replication.NewLocal()
	var seen atomic.Int64
	r.Subscribe(func(ctx context.Context, f replication.Frame) error {
		seen.Add(1)
		return nil
	})
	dec, err := r.Propose(context.Background(), replication.Frame{Key: "wf/a/b/c"})
	if err != nil {
		t.Fatal(err)
	}
	if dec != replication.DecisionAccept {
		t.Fatalf("decision = %v, want Accept", dec)
	}
	if seen.Load() != 1 {
		t.Fatalf("subscriber not invoked: %d", seen.Load())
	}
}

func TestLocal_AssignsSequence(t *testing.T) {
	r := replication.NewLocal()
	var lastSeq atomic.Uint64
	r.Subscribe(func(ctx context.Context, f replication.Frame) error {
		lastSeq.Store(f.Seq)
		return nil
	})
	for i := 0; i < 3; i++ {
		_, _ = r.Propose(context.Background(), replication.Frame{Key: "k"})
	}
	if lastSeq.Load() != 3 {
		t.Fatalf("expected seq=3, got %d", lastSeq.Load())
	}
}

func TestLocal_HandlerErrorRejects(t *testing.T) {
	r := replication.NewLocal()
	r.Subscribe(func(ctx context.Context, f replication.Frame) error {
		return errors.New("nope")
	})
	dec, err := r.Propose(context.Background(), replication.Frame{Key: "k"})
	if err == nil || dec != replication.DecisionReject {
		t.Fatalf("expected reject, got dec=%v err=%v", dec, err)
	}
}

func TestLocal_StatsCount(t *testing.T) {
	r := replication.NewLocal()
	for i := 0; i < 5; i++ {
		_, _ = r.Propose(context.Background(), replication.Frame{Key: "k"})
	}
	a, rj := r.Stats()
	if a != 5 || rj != 0 {
		t.Fatalf("stats = (%d, %d), want (5, 0)", a, rj)
	}
}

func TestLocal_CloseIdempotent(t *testing.T) {
	r := replication.NewLocal()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
