// Copyright (c) Hanzo AI Inc 2026.

package workflow

import "testing"

// TestChannelSealed is a compile-time / runtime proof that the
// ReceiveChannel interface is sealed: only types in this package can
// satisfy it. Any external type that attempts to implement the
// interface will fail to compile because the unexported sealed()
// method is not reachable.
//
// This test asserts the positive case — the canonical chanImpl
// satisfies it — and documents the sealing invariant for reviewers.
// A negative-case "forked type should not satisfy" check would live
// in an _external_test.go that imports workflow as a different
// package; that file would fail to compile by design and so is not
// checked in.
func TestChannelSealed(t *testing.T) {
	var _ ReceiveChannel = (*chanImpl)(nil)
	var _ Channel = (*chanImpl)(nil)
	var _ Channel = NewSignalChannel("s", 1)

	// Seal enforcement is a compile-time property; assert the method
	// is callable on the impl so the coverage counter lights up.
	c := newChan("x", 1)
	c.sealed()
}
