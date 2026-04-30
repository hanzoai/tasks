// Copyright © 2026 Hanzo AI. MIT License.

package routing_test

import (
	"sync/atomic"
	"testing"

	"github.com/hanzoai/tasks/pkg/tasks/routing"
)

func TestRouter_LeaderDeterministic(t *testing.T) {
	r := routing.NewHash([]byte("seed"))
	r.SetMembership("a", []routing.NodeID{"a", "b", "c"})
	k := routing.Key{Org: "o", Namespace: "n", TaskQueue: "q"}
	first, ok := r.LeaderFor(k)
	if !ok {
		t.Fatal("no leader")
	}
	for i := 0; i < 100; i++ {
		got, _ := r.LeaderFor(k)
		if got != first {
			t.Fatalf("leader changed without membership change: %s vs %s", got, first)
		}
	}
}

func TestRouter_LeaderChangesOnMembership(t *testing.T) {
	r := routing.NewHash([]byte("seed"))
	r.SetMembership("a", []routing.NodeID{"a"})
	k := routing.Key{Org: "o", Namespace: "n", TaskQueue: "q"}
	leader1, _ := r.LeaderFor(k)
	r.SetMembership("a", []routing.NodeID{"b", "c", "d"})
	leader2, _ := r.LeaderFor(k)
	if leader2 == leader1 {
		t.Fatalf("leader did not change after membership swap: %s", leader1)
	}
	if leader2 != "b" && leader2 != "c" && leader2 != "d" {
		t.Fatalf("leader %q not in new set", leader2)
	}
}

func TestRouter_IsLeader(t *testing.T) {
	r := routing.NewHash([]byte("seed"))
	r.SetMembership("a", []routing.NodeID{"a", "b", "c"})
	k := routing.Key{Org: "o", Namespace: "n", TaskQueue: "q"}
	expected, _ := r.LeaderFor(k)
	r.SetMembership(expected, []routing.NodeID{"a", "b", "c"})
	if !r.IsLeader(k) {
		t.Fatal("expected local node to be leader")
	}
}

func TestRouter_OnChangeFires(t *testing.T) {
	r := routing.NewHash([]byte("seed"))
	var fired atomic.Int64
	r.OnChange(func() { fired.Add(1) })
	r.SetMembership("a", []routing.NodeID{"a"})
	r.SetMembership("a", []routing.NodeID{"a", "b"})
	if fired.Load() != 2 {
		t.Fatalf("OnChange fired %d times, want 2", fired.Load())
	}
}

func TestRouter_ZeroValidatorsNoLeader(t *testing.T) {
	r := routing.NewHash(nil)
	r.SetMembership("a", nil)
	if _, ok := r.LeaderFor(routing.Key{Org: "o", Namespace: "n", TaskQueue: "q"}); ok {
		t.Fatal("LeaderFor returned ok with empty validator set")
	}
}
