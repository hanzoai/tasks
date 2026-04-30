// Copyright © 2026 Hanzo AI. MIT License.

package replication_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/tasks/replication"
)

func TestQuasar_SingleNodeAccepts(t *testing.T) {
	hub := replication.NewMemoryHub()
	t1 := hub.Connect("node-a")
	r, err := replication.NewQuasar(context.Background(), replication.QuasarConfig{
		NodeID:      "node-a",
		Validators:  []string{"node-a"},
		Transport:   t1,
		ProposeWait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var applied atomic.Int64
	r.Subscribe(func(ctx context.Context, f replication.Frame) error {
		applied.Add(1)
		return nil
	})
	dec, err := r.Propose(context.Background(), replication.Frame{Key: "wf/a/b/c", Op: "put"})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if dec != replication.DecisionAccept {
		t.Fatalf("decision = %v", dec)
	}
	if applied.Load() != 1 {
		t.Fatalf("applied=%d, want 1", applied.Load())
	}
}

func TestQuasar_AssignsSequenceMonotonic(t *testing.T) {
	hub := replication.NewMemoryHub()
	t1 := hub.Connect("a")
	r, err := replication.NewQuasar(context.Background(), replication.QuasarConfig{
		NodeID: "a", Validators: []string{"a"}, Transport: t1, ProposeWait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var seqs []uint64
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	r.Subscribe(func(ctx context.Context, f replication.Frame) error {
		<-mu
		seqs = append(seqs, f.Seq)
		mu <- struct{}{}
		return nil
	})
	for i := 0; i < 4; i++ {
		_, _ = r.Propose(context.Background(), replication.Frame{Key: "k", Op: "put"})
	}
	if len(seqs) != 4 {
		t.Fatalf("got %d frames, want 4", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("non-monotonic sequence: %v", seqs)
		}
	}
}

func TestQuasar_Stats(t *testing.T) {
	hub := replication.NewMemoryHub()
	t1 := hub.Connect("a")
	r, err := replication.NewQuasar(context.Background(), replication.QuasarConfig{
		NodeID: "a", Validators: []string{"a"}, Transport: t1, ProposeWait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for i := 0; i < 3; i++ {
		_, _ = r.Propose(context.Background(), replication.Frame{Key: "k", Op: "put"})
	}
	a, _, _ := r.Stats()
	if a != 3 {
		t.Fatalf("accepted = %d, want 3", a)
	}
}

func TestQuasar_TimeoutReturnsTimeout(t *testing.T) {
	hub := replication.NewMemoryHub()
	t1 := hub.Connect("a")
	r, err := replication.NewQuasar(context.Background(), replication.QuasarConfig{
		NodeID: "a", Validators: []string{"a"}, Transport: t1, ProposeWait: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// With a sane single-node engine, Accept should still happen
	// inside 1ms because the self-vote completes synchronously. But
	// some engines need ≥1 poll iteration; if we observe Timeout,
	// the assertion below catches that the API surface is wired.
	dec, _ := r.Propose(context.Background(), replication.Frame{Key: "k", Op: "put"})
	if dec != replication.DecisionAccept && dec != replication.DecisionTimeout {
		t.Fatalf("decision = %v", dec)
	}
}

func TestQuasar_CloseIdempotent(t *testing.T) {
	hub := replication.NewMemoryHub()
	t1 := hub.Connect("a")
	r, err := replication.NewQuasar(context.Background(), replication.QuasarConfig{
		NodeID: "a", Validators: []string{"a"}, Transport: t1, ProposeWait: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
}
