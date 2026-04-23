// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"testing"
	"time"
)

// TestInterruptChSameInstance confirms repeated calls return the same
// channel — callers that wire a worker chain across N Run calls all
// observe the same interrupt signal.
func TestInterruptChSameInstance(t *testing.T) {
	t.Parallel()
	a := InterruptCh()
	b := InterruptCh()
	if a != b {
		t.Fatal("InterruptCh must return the same channel on repeated calls")
	}
}

// TestInterruptChShape checks the returned channel matches the type
// expected by Worker.Run (<-chan any).
func TestInterruptChShape(t *testing.T) {
	t.Parallel()
	ch := InterruptCh()
	// Drain non-blocking to verify the channel is receivable.
	select {
	case <-ch:
		t.Fatal("InterruptCh must not be closed at construction")
	case <-time.After(10 * time.Millisecond):
	}
}
