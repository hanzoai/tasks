// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"sync"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	luxlog "github.com/luxfi/log"
)

// workerEnv is the worker-owned CoroutineEnv (satisfies
// pkg/sdk/workflow.CoroutineEnv). Phase 1 delegates the actual
// coroutine plumbing (channels, selectors, futures) to a
// workflow.StubEnv — the stub's single-goroutine semantics are
// precisely what Phase 1 needs. The Phase-2 replay engine replaces
// the delegation with a history-driven scheduler without changing
// this file's public surface.
//
// workerEnv sits in the worker package (not workflow) because its
// knowledge is worker-specific: the activity transport to dispatch
// to, the task's logger bindings, the workflow metadata pulled off
// the poll response.
type workerEnv struct {
	// delegate is the current runtime. Phase 1 = stub; Phase 2 =
	// replay scheduler. Kept as an interface so both live under one
	// roof.
	delegate workflow.CoroutineEnv

	mu   sync.Mutex
	info workflow.Info

	// transport is retained so a Phase-2 evolution can route
	// ExecuteActivity calls through the wire (v1's stub env runs
	// activities in-process from registered mocks).
	transport client.WorkerTransport

	// logger is the per-workflow logger the env returns from Logger().
	logger luxlog.Logger
}

// newWorkerEnv constructs a workerEnv for a workflow task.
func newWorkerEnv(transport client.WorkerTransport, info workflow.Info, logger luxlog.Logger) *workerEnv {
	if logger == nil {
		logger = luxlog.Noop()
	}
	stub := workflow.NewStubEnv()
	stub.SetInfo(info)
	return &workerEnv{
		delegate:  stub,
		info:      info,
		transport: transport,
		logger:    logger,
	}
}

// Now returns the delegate's clock. In Phase 1 the StubEnv returns
// a deterministic time.Time that advances on each Now() call (driven
// by the test harness) or stays frozen. Phase 2 will return the
// history's committed timestamp.
func (e *workerEnv) Now() time.Time { return e.delegate.Now() }

// Logger returns the env-scoped logger. Prefers the explicitly-set
// worker logger over the delegate's logger so per-task fields stick.
func (e *workerEnv) Logger() luxlog.Logger {
	if e.logger != nil {
		return e.logger
	}
	return e.delegate.Logger()
}

// Sleep delegates.
func (e *workerEnv) Sleep(d time.Duration) error { return e.delegate.Sleep(d) }

// NewTimer delegates.
func (e *workerEnv) NewTimer(d time.Duration) workflow.Future { return e.delegate.NewTimer(d) }

// ExecuteActivity delegates. In Phase 1 the stub runs activities
// from its registered mocks. Phase 2 routes to the server.
func (e *workerEnv) ExecuteActivity(opts workflow.ActivityOptions, activity any, args []any) workflow.Future {
	return e.delegate.ExecuteActivity(opts, activity, args)
}

// GetSignalChannel delegates.
func (e *workerEnv) GetSignalChannel(name string) workflow.ReceiveChannel {
	return e.delegate.GetSignalChannel(name)
}

// NewChannel delegates.
func (e *workerEnv) NewChannel(name string, buffered int) workflow.Channel {
	return e.delegate.NewChannel(name, buffered)
}

// Select delegates.
func (e *workerEnv) Select(cases []workflow.SelectCase) int {
	return e.delegate.Select(cases)
}

// NewCancelScope delegates.
func (e *workerEnv) NewCancelScope() (any, workflow.CancelFunc) {
	return e.delegate.NewCancelScope()
}

// CurrentScope delegates.
func (e *workerEnv) CurrentScope() any { return e.delegate.CurrentScope() }

// ScopeDone delegates.
func (e *workerEnv) ScopeDone(scope any) <-chan struct{} { return e.delegate.ScopeDone(scope) }

// ScopeErr delegates.
func (e *workerEnv) ScopeErr(scope any) error { return e.delegate.ScopeErr(scope) }

// WorkflowInfo returns the info captured at task receipt. If the
// delegate has a richer info (e.g. attempt counter), we merge.
func (e *workerEnv) WorkflowInfo() workflow.Info {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.info.WorkflowID != "" {
		return e.info
	}
	return e.delegate.WorkflowInfo()
}
