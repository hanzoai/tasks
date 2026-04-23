// Copyright © 2026 Hanzo AI. MIT License.

package workflow

import (
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// DemoLocalActivity is a named fn used so the stub env can register a
// queued response keyed by function name.
func DemoLocalActivity(_ any) (string, error) { return "ok", nil }

// TestLocalActivityOptionsFlowThrough verifies options attached via
// WithLocalActivityOptions are distinct from ActivityOptions and do
// not collide keys.
func TestLocalActivityOptionsFlowThrough(t *testing.T) {
	t.Parallel()
	env := NewStubEnv()
	base := NewContextFromEnv(env)
	lo := LocalActivityOptions{
		StartToCloseTimeout: 2 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	}
	ctx := WithLocalActivityOptions(base, lo)
	got := localActivityOptionsFromCtx(ctx)
	if got.StartToCloseTimeout != 2*time.Second {
		t.Fatalf("StartToCloseTimeout = %v", got.StartToCloseTimeout)
	}
	if got.RetryPolicy == nil || got.RetryPolicy.MaximumAttempts != 3 {
		t.Fatalf("RetryPolicy = %+v", got.RetryPolicy)
	}
	// A sibling ActivityOptions attached later must not replace the
	// local activity options.
	ao := WithActivityOptions(ctx, ActivityOptions{StartToCloseTimeout: time.Second})
	still := localActivityOptionsFromCtx(ao)
	if still.StartToCloseTimeout != 2*time.Second {
		t.Fatalf("local options got clobbered: %+v", still)
	}
}

// TestExecuteLocalActivityRemotePath confirms Phase-1 dispatches local
// activities via the env's ExecuteActivity — same wire as remote.
func TestExecuteLocalActivityRemotePath(t *testing.T) {
	t.Parallel()
	env := NewStubEnv()
	env.OnActivity(DemoLocalActivity).Return("ok", nil)
	ctx := WithLocalActivityOptions(NewContextFromEnv(env), LocalActivityOptions{
		StartToCloseTimeout: time.Second,
	})
	f := ExecuteLocalActivity(ctx, DemoLocalActivity, "in")
	var out string
	if err := f.Get(ctx, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q", out)
	}
}

// TestExecuteLocalActivityNilCtx returns a failed future (matches the
// ExecuteActivity nil-context behaviour).
func TestExecuteLocalActivityNilCtx(t *testing.T) {
	t.Parallel()
	f := ExecuteLocalActivity(nil, DemoLocalActivity)
	var out any
	err := f.Get(NewContextFromEnv(NewStubEnv()), &out)
	if err == nil {
		t.Fatal("expected error on nil ctx")
	}
}

// TestWithLocalActivityOptionsNilParent panics on nil parent, matching
// WithActivityOptions / WithValue semantics.
func TestWithLocalActivityOptionsNilParent(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil parent")
		}
	}()
	WithLocalActivityOptions(nil, LocalActivityOptions{})
}
