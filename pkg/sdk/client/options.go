// Package client is the Hanzo Tasks workflow client. Native ZAP transport,
// zero go.temporal.io/* and zero google.golang.org/grpc imports.
//
// The wire is ZAP framed per schema/tasks.zap. Opcodes 0x0060-0x009F are
// owned by this package; 0x0050-0x005F stay reserved for the legacy
// one-shot/schedule surface owned by pkg/tasks.
//
// v1 wire note: request and response payloads are transported as a single
// bytes field holding JSON. Native ZAP serde for every RPC in schema/tasks.zap
// replaces JSON in a follow-up without changing opcodes.
package client

import (
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
)

// Options configures Dial. Fields mirror the upstream Temporal Options
// surface so callers can swap import paths without changing arguments.
type Options struct {
	// HostPort is the "host:port" address of the Hanzo Tasks frontend
	// exposing the ZAP transport (default port 9652).
	HostPort string

	// Namespace scopes every RPC issued by the returned Client. Empty
	// string is rewritten to "default" at Dial time.
	Namespace string

	// Identity is sent as the caller identity on long-poll / worker RPCs.
	// Optional; defaults to "hanzo-tasks-sdk".
	Identity string

	// DialTimeout bounds the initial ZAP connect. Zero means "no bound".
	DialTimeout time.Duration

	// CallTimeout bounds a single request/response round trip. Zero
	// means "no bound per call" — callers are expected to pass their
	// own ctx deadline.
	CallTimeout time.Duration

	// Transport overrides the default ZAP transport. Primarily for
	// testing — leave nil in production.
	Transport Transport
}

// StartWorkflowOptions mirrors the upstream StartWorkflowOptions struct
// for ExecuteWorkflow. Only the fields currently mapped onto the wire
// (schema/tasks.zap StartWorkflowRequest + Timeouts + RetryPolicy) are
// meaningful; unlisted fields are ignored by the v1 wire.
type StartWorkflowOptions struct {
	// ID is the workflow ID. Empty string asks the server to generate
	// one (returned as WorkflowRun.GetID()).
	ID string

	// TaskQueue is required; workers poll this queue for tasks.
	TaskQueue string

	// WorkflowExecutionTimeout is the total allowed wall-clock time
	// across all runs (Continue-As-New chains).
	WorkflowExecutionTimeout time.Duration

	// WorkflowRunTimeout is the allowed wall-clock time for a single
	// run.
	WorkflowRunTimeout time.Duration

	// WorkflowTaskTimeout bounds a single workflow-task attempt on a
	// worker. Default on server is 10s.
	WorkflowTaskTimeout time.Duration

	// RetryPolicy overrides the workflow-level retry policy (distinct
	// from per-activity retry).
	RetryPolicy *temporal.RetryPolicy

	// CronSchedule makes the workflow recurring. Empty string runs once.
	CronSchedule string

	// Memo is opaque metadata attached to the execution.
	Memo map[string]any

	// SearchAttributes are indexed by the visibility store.
	SearchAttributes map[string]any
}
