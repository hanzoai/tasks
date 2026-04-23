// Package sdk is the Hanzo Tasks workflow SDK. Native ZAP, no
// upstream temporal.io imports anywhere in this tree.
//
// Layout:
//
//	pkg/sdk/client    — Dial, Options, StartWorkflowOptions, WorkflowRun
//	pkg/sdk/worker    — New, Start, Stop, RegisterWorkflow, RegisterActivity
//	pkg/sdk/workflow  — Context, Future, Selector, Channel, Timer,
//	                    Signal, ExecuteActivity, WithActivityOptions
//	pkg/sdk/activity  — GetLogger, RecordHeartbeat, GetInfo
//	pkg/sdk/temporal  — RetryPolicy, ActivityError, error constructors
//
// All traffic on the wire is ZAP. Schemas live under ../../schema.
// Transport is luxfi/zap on _tasks._tcp:9652. No gRPC, no
// protobuf, no gRPC-Web.
//
// Migration contract (applies to base / commerce / ta / any
// Hanzo Go service that ran Temporal workflows):
//
//	go.temporal.io/sdk/client    → github.com/hanzoai/tasks/pkg/sdk/client
//	go.temporal.io/sdk/worker    → github.com/hanzoai/tasks/pkg/sdk/worker
//	go.temporal.io/sdk/workflow  → github.com/hanzoai/tasks/pkg/sdk/workflow
//	go.temporal.io/sdk/activity  → github.com/hanzoai/tasks/pkg/sdk/activity
//	go.temporal.io/sdk/temporal  → github.com/hanzoai/tasks/pkg/sdk/temporal
//
// CI enforces zero `go.temporal.io` imports outside pkg/sdk once
// migration finishes.
package sdk
