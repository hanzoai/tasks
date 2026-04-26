// Copyright © 2026 Hanzo AI. MIT License.

package workflow

// Version is a deterministic-replay marker. GetVersion returns a stable
// integer for a given changeID across replays, allowing workflow authors
// to evolve workflow code without breaking in-flight executions.
//
// Mirrors go.temporal.io/sdk/workflow.Version semantics so call sites
// migrate by changing the import path only.
type Version int

// DefaultVersion is the version returned by GetVersion the first time a
// changeID is encountered before any new version is introduced. New
// versions start at 0 and increase from there.
const DefaultVersion Version = -1

// GetVersion returns the version recorded for changeID in this workflow
// run, clamped to [minSupported, maxSupported]. The first call records
// maxSupported; all later calls (and replays) return the recorded value.
//
// Workflow authors gate behavioural changes on the returned version:
//
//	v := workflow.GetVersion(ctx, "use-new-activity", workflow.DefaultVersion, 1)
//	if v == workflow.DefaultVersion {
//	    workflow.ExecuteActivity(ctx, oldActivity).Get(ctx, nil)
//	} else {
//	    workflow.ExecuteActivity(ctx, newActivity).Get(ctx, nil)
//	}
//
// Phase-1 (no event-sourced replay yet): the recorded value is held in
// the per-run env. Crashes restart the workflow from the top, at which
// point the workflow re-records maxSupported — this is the documented
// Phase-1 contract: same input replays produce the same path because
// the same maxSupported is presented at the same call site.
func GetVersion(ctx Context, changeID string, minSupported, maxSupported Version) Version {
	if ctx == nil {
		return DefaultVersion
	}
	return ctx.env().GetVersion(changeID, minSupported, maxSupported)
}

// SideEffect runs fn exactly once per workflow execution and records its
// result into history. Replays return the recorded value without re-
// running fn. The returned Future settles synchronously.
//
// Use SideEffect for nondeterministic computations whose result must be
// stable across replays (UUID generation, time.Now, etc.). Activities
// remain the right choice for I/O.
func SideEffect(ctx Context, fn func(ctx Context) any) Future {
	f := NewFuture()
	if ctx == nil {
		f.Settle(nil, errNilContext)
		return f
	}
	encoded, err := ctx.env().SideEffect(fn, ctx)
	if err != nil {
		f.Settle(nil, err)
		return f
	}
	f.Settle(encoded, nil)
	return f
}

// MutableSideEffect runs fn whenever the workflow re-evaluates this
// changeID. It returns the previously-recorded value when eq reports the
// new value is equal to it; otherwise it records the new value.
//
// Use MutableSideEffect for values that may evolve safely between
// replays (configuration tweakables, feature flags) where determinism
// only requires "same value seen by all dependents on the same step,"
// not "same value across the whole run."
func MutableSideEffect(ctx Context, changeID string, fn func(ctx Context) any, eq func(a, b any) bool) Future {
	f := NewFuture()
	if ctx == nil {
		f.Settle(nil, errNilContext)
		return f
	}
	encoded, err := ctx.env().MutableSideEffect(changeID, fn, eq, ctx)
	if err != nil {
		f.Settle(nil, err)
		return f
	}
	f.Settle(encoded, nil)
	return f
}
