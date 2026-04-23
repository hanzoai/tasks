// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
)

// RegisterWorkflowOptions customises a workflow registration.
type RegisterWorkflowOptions struct {
	// Name overrides the registry key. Empty uses the reflected
	// Go function name (see funcName).
	Name string

	// DisableAlreadyRegisteredCheck lets a caller replace a prior
	// registration without panicking — useful for hot-reload in
	// development. Default false: double-registration is a bug.
	DisableAlreadyRegisteredCheck bool
}

// RegisterActivityOptions customises an activity registration.
type RegisterActivityOptions struct {
	// Name overrides the registry key.
	Name string
}

// registry holds the worker's workflow and activity maps. All
// operations are guarded by a single RWMutex; registrations happen
// pre-Start and reads dominate at runtime, so contention is nil.
type registry struct {
	mu         sync.RWMutex
	workflows  map[string]any
	activities map[string]any
}

// newRegistry returns an empty registry.
func newRegistry() *registry {
	return &registry{
		workflows:  make(map[string]any),
		activities: make(map[string]any),
	}
}

// registerWorkflow adds fn to the workflow map under opts.Name, or
// under its reflected function name if opts.Name is empty. Panics
// on double-registration unless opts.DisableAlreadyRegisteredCheck
// is set — matches the upstream Temporal contract.
func (r *registry) registerWorkflow(fn any, opts RegisterWorkflowOptions) {
	if fn == nil {
		panic("hanzo/tasks/worker: RegisterWorkflow called with nil fn")
	}
	name := opts.Name
	if name == "" {
		name = funcName(fn)
	}
	if name == "" {
		panic("hanzo/tasks/worker: cannot register workflow with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.workflows[name]; exists && !opts.DisableAlreadyRegisteredCheck {
		panic(fmt.Sprintf("hanzo/tasks/worker: workflow %q already registered", name))
	}
	r.workflows[name] = fn
}

// registerActivity adds fn to the activity map.
func (r *registry) registerActivity(fn any, opts RegisterActivityOptions) {
	if fn == nil {
		panic("hanzo/tasks/worker: RegisterActivity called with nil fn")
	}
	name := opts.Name
	if name == "" {
		name = funcName(fn)
	}
	if name == "" {
		panic("hanzo/tasks/worker: cannot register activity with empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.activities[name]; exists {
		panic(fmt.Sprintf("hanzo/tasks/worker: activity %q already registered", name))
	}
	r.activities[name] = fn
}

// workflowFn returns the registered workflow function for name, or
// nil and false if none.
func (r *registry) workflowFn(name string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.workflows[name]
	return fn, ok
}

// activityFn returns the registered activity function for name.
func (r *registry) activityFn(name string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	fn, ok := r.activities[name]
	return fn, ok
}

// funcName returns the Go function name of fn. Format matches the
// upstream Temporal SDK: the trailing "PackagePath.FunctionName" is
// trimmed to just "FunctionName". Identical to the helper in
// workflow.reflect but kept locally so this package can stay
// consumption-agnostic about workflow.
func funcName(fn any) string {
	rv := reflect.ValueOf(fn)
	if !rv.IsValid() || rv.Kind() != reflect.Func {
		return ""
	}
	rf := runtime.FuncForPC(rv.Pointer())
	if rf == nil {
		return ""
	}
	full := rf.Name()
	if i := strings.LastIndex(full, "."); i >= 0 && i < len(full)-1 {
		return full[i+1:]
	}
	return full
}
