// Package temporal provides native Hanzo Tasks workflow primitives
// (retry policy, error model, failure serialisation) with zero
// dependency on any go.temporal.io/* package.
//
// This package is consumed directly by user code (workflows,
// activities) and indirectly by the rest of the SDK
// (pkg/sdk/workflow, pkg/sdk/activity, pkg/sdk/client,
// pkg/sdk/worker). It is deliberately tiny and has no imports
// outside the Go standard library.
package temporal

import "time"

// RetryPolicy controls how the engine retries a workflow or
// activity that returns an error. Semantics mirror the classic
// Temporal retry model so existing hanzoai/base and hanzoai/commerce
// call sites work unchanged after the import swap.
//
// Wire form (schema/tasks.zap:RetryPolicy) uses int64 milliseconds;
// Go callers work with time.Duration. Conversion happens in serde.go.
type RetryPolicy struct {
	// InitialInterval is the delay before the first retry.
	// Zero means "use DefaultRetryPolicy().InitialInterval".
	InitialInterval time.Duration

	// BackoffCoefficient multiplies the interval on each retry.
	// Must be >= 1.0. Zero means "use default (2.0)".
	BackoffCoefficient float64

	// MaximumInterval caps the exponential backoff. Zero means
	// "100 * InitialInterval" (matches upstream default).
	MaximumInterval time.Duration

	// MaximumAttempts bounds the total retry count including the
	// first execution. 0 = unlimited, 1 = no retries.
	MaximumAttempts int32

	// NonRetryableErrorTypes lists error Code values that skip
	// retry regardless of MaximumAttempts. See errors.go.
	NonRetryableErrorTypes []string
}

// DefaultRetryPolicy returns the retry policy used when a
// workflow/activity has not configured one explicitly.
func DefaultRetryPolicy() *RetryPolicy {
	return &RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    100 * time.Second,
		MaximumAttempts:    0, // unlimited
	}
}

// Normalize returns a copy of p with zero fields filled in from
// DefaultRetryPolicy. The input is not mutated. Callers who want
// to know the effective policy (for dispatch, for logging) use
// this. Callers who want to honour user-supplied zeros pass the
// policy through untouched.
func (p *RetryPolicy) Normalize() *RetryPolicy {
	d := DefaultRetryPolicy()
	if p == nil {
		return d
	}
	out := *p
	if out.InitialInterval <= 0 {
		out.InitialInterval = d.InitialInterval
	}
	if out.BackoffCoefficient < 1 {
		out.BackoffCoefficient = d.BackoffCoefficient
	}
	if out.MaximumInterval <= 0 {
		out.MaximumInterval = 100 * out.InitialInterval
	}
	if out.MaximumAttempts < 0 {
		out.MaximumAttempts = 0
	}
	return &out
}

// ShouldRetry reports whether an error produced at attempt n (1-based)
// under policy p should be retried. It is a pure function: it does
// not sleep, log, or call the network. Callers schedule the retry.
func (p *RetryPolicy) ShouldRetry(err error, attempt int32) bool {
	if err == nil {
		return false
	}
	eff := p.Normalize()
	if eff.MaximumAttempts > 0 && attempt >= eff.MaximumAttempts {
		return false
	}
	if terr, ok := AsError(err); ok {
		if terr.NonRetryable {
			return false
		}
		for _, t := range eff.NonRetryableErrorTypes {
			if t == terr.Code {
				return false
			}
		}
	}
	return true
}

// NextInterval returns the delay before retry number `attempt`
// (1-based — attempt=1 is the delay before the first retry, which
// happens after the initial execution failed).
func (p *RetryPolicy) NextInterval(attempt int32) time.Duration {
	eff := p.Normalize()
	if attempt < 1 {
		attempt = 1
	}
	// interval = InitialInterval * BackoffCoefficient^(attempt-1),
	// clamped at MaximumInterval. Computed in float64 to avoid
	// overflow for small coefficients and moderate attempts.
	interval := float64(eff.InitialInterval)
	for i := int32(1); i < attempt; i++ {
		interval *= eff.BackoffCoefficient
		if interval >= float64(eff.MaximumInterval) {
			return eff.MaximumInterval
		}
	}
	return time.Duration(interval)
}
