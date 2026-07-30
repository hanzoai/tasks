# Hanzo Tasks — Canonical Contract

The single durable async substrate for every Hanzo Go service.

This document is law. If your service needs retries, durability, scheduling,
cron, or any async work that must survive a process restart, you use
`github.com/hanzoai/tasks`. There is no second async system.

## 1. When to use tasks vs sync

Sync HTTP handler (no tasks):

- Total work fits in <100ms.
- Caller blocks for the response and the result is the whole point of the call.
- Single round-trip with no external I/O that can flake (OTP code generation,
  JWT issue, field validation, in-memory CLOB match, JWKS lookup).
- Failure is the response — the caller decides what to do.

Tasks (durable workflow):

- Any side-effect that must survive a pod restart or panic.
- Anything with retry policy (webhook delivery, mint/burn, settlement,
  notification send, third-party API fan-out).
- Anything scheduled (cron, delay, `send_at`, daily/quarterly sweeps).
- Anything that fans out across multiple steps where a partial completion
  is worse than no completion (saga-style flows).
- Long jobs (>5s) where the HTTP caller should not block.

Rule of thumb: if you find yourself writing `go func() { ... }()` to fire
work in the background, you are writing a bug. Use tasks.

## 2. Task definition shape

Every workflow is named, namespaced, and has a typed input + output.

```go
// pkg/tasks/<feature>.go in your service.
package <feature>tasks

import (
    "time"

    "github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// Input is the workflow + activity payload. JSON-serializable, no
// pointers, no interfaces.
type Input struct {
    ID         string         `json:"id"`
    TenantSlug string         `json:"tenant_slug"`
    // … domain fields …
}

// Output is the workflow return shape.
type Output struct {
    ID     string `json:"id"`
    Status string `json:"status"`
    Error  string `json:"error,omitempty"`
}

// FeatureWorkflow is the durable entry point. Must be deterministic on
// its inputs — all I/O happens inside activities.
func FeatureWorkflow(ctx workflow.Context, in Input) (Output, error) {
    actCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
        StartToCloseTimeout: 60 * time.Second,
        RetryPolicy: &workflow.RetryPolicy{
            InitialInterval:    5 * time.Second,
            BackoffCoefficient: 2.0,
            MaximumInterval:    2 * time.Minute,
            MaximumAttempts:    5,
        },
    })
    var out Output
    if err := workflow.ExecuteActivity(actCtx, "Deliver", in).Get(actCtx, &out); err != nil {
        return Output{ID: in.ID, Status: "failed", Error: err.Error()}, err
    }
    return out, nil
}
```

Defaults — apply unless your workflow explicitly contradicts them:

| Field | Default | When to change |
|---|---|---|
| `StartToCloseTimeout` | 60s | I/O heavy >60s; never above 10m |
| `InitialInterval` | 5s | Fast-retry only if upstream <1s |
| `BackoffCoefficient` | 2.0 | Never lower (thundering herd) |
| `MaximumInterval` | 2m | Long-tail jobs may bump to 30m |
| `MaximumAttempts` | 5 | Idempotent ops can go to 10 |

Idempotency is the activity's responsibility. The tasks server replays on
retry; your activity must detect "already done" and return the prior result.

## 3. Worker registration pattern

One worker per service, started during boot, drained on shutdown.

```go
// in your service's main.go boot path
import (
    tasksclient "github.com/hanzoai/tasks/pkg/sdk/client"
    tasksworker "github.com/hanzoai/tasks/pkg/sdk/worker"
)

cli, err := tasksclient.Dial(tasksclient.Options{
    Address:   os.Getenv("TASKS_ADDR"),     // "tasks.hanzo.svc:9999" or "/run/tasks.sock"
    Namespace: tenantNamespace(orgID),       // see §6
})
if err != nil {
    return fmt.Errorf("tasks dial: %w", err)
}

wk := tasksworker.New(cli, "<service>-default", tasksworker.Options{
    Logger: logger,
})
wk.RegisterWorkflow(FeatureWorkflow)
wk.RegisterActivity(activities.Deliver)
if err := wk.Start(); err != nil {
    return fmt.Errorf("tasks worker start: %w", err)
}
defer wk.Stop()
```

Task queue name: `<service>-<purpose>` (e.g. `notify-send`, `bd-webhooks`,
`ta-chain`, `ats-settlement`). One queue per workflow family. Do not
multiplex unrelated workflows on the same queue.

## 4. Client API

The sync caller's interaction with tasks is exactly two calls.

```go
// Enqueue — returns immediately with task id.
run, err := cli.ExecuteWorkflow(ctx, tasksclient.StartWorkflowOptions{
    ID:        "<feature>-" + in.ID, // deterministic ID = dedup key
    TaskQueue: "<service>-<purpose>",
}, "FeatureWorkflow", in)
if err != nil {
    return "", fmt.Errorf("tasks enqueue: %w", err)
}
taskID := run.GetID()

// Optional wait for completion (sync-over-async).
var out Output
if err := run.Get(ctx, &out); err != nil {
    return out, err
}
```

`ExecuteWorkflow` is idempotent on the workflow ID. Re-submitting the same
ID with the workflow still running returns the existing handle. Use the
business key (`message_id`, `withdrawal_id`, `intent_id`) as the workflow
ID — that is your idempotency token.

## 5. ZAP transport

Production: ZAP only. `tasksd` listens on TCP/9999 by default -- or on a
unix socket when `--zap` is given a path, which is the right shape when
caller and engine share a host; the SDK
dials directly. No HTTP, no JSON envelope on the hot path, no gRPC.

Local dev: `TASKS_ADDR=""` switches the SDK to in-process mode (no
durability, used for tests and dev loops). Production must set
`TASKS_ADDR=tasks.hanzo.svc:9999` (or the per-cluster service name).

Auth flows via the same JWT the caller already holds. The SDK attaches
`Authorization: Bearer <jwt>` from `tasksclient.Options.AuthToken` or
from the context's outbound header bag. `tasksd` validates against IAM
JWKS (see hanzoai/tasks LLM.md §"Auth: IAM only").

## 6. Per-tenant namespace isolation

Namespaces map 1:1 to IAM orgs. This is non-negotiable.

| Layer | Value |
|---|---|
| IAM org | `mlc` |
| Tasks namespace | `mlc` |
| SQLite store | `data/mlc.db` |

Workers dial with `Namespace: <orgSlug>`. All schedules, workflows, and
visibility queries are namespace-scoped at the server. An `org-a`
worker cannot see `org-b` workflows even if both share a binary. The
strip + write identity middleware in `tasksd` enforces this on every RPC.

Multi-tenant services hold one client + worker per active org. Onboarding
a new org allocates the namespace lazily on first `ExecuteWorkflow`.

Single-tenant services (notify when serving Hanzo only) may run a single
`default` namespace; the moment they serve a second tenant, switch to
per-org namespacing — no retrofitting allowed in production.

## 7. Failure modes

- **Activity error returned**: tasks server retries per the workflow's
  RetryPolicy. After `MaximumAttempts`, the workflow run is marked
  `failed` and the result is queryable via `cli.DescribeWorkflowExecution`.
- **Activity panic**: caught by the SDK; counted as an attempt; retried.
- **Worker process killed mid-activity**: the heartbeat times out; the
  task is rescheduled on another worker. Activities MUST be idempotent.
- **Workflow exceeds `WorkflowExecutionTimeout`**: terminated; downstream
  saga must reconcile or fire a compensating workflow.
- **Server unreachable on enqueue**: `ExecuteWorkflow` returns an error.
  The caller decides — usually: persist the intent to local storage and
  resubmit on a recovery cron, never silently drop.

Dead-letter behavior: there is no separate DLQ collection. Failed runs
stay in the visibility store with `status=failed` and are queryable. The
operator surface (CRD `LiquidTasks` / `HanzoTasks` `.status`) surfaces
counts of `failed_24h` per namespace per queue.

Alerting: `tasks_workflow_runs_failed_total{namespace,queue}` Prometheus
metric → alerts at >0.5% failure ratio over 5m or any 10 consecutive
failures of the same workflow ID. The alert lands in the owning team's
PagerDuty service via Hanzo Insights.

## 8. Observability

UI: `tasks.hanzo.ai` (hanzoai/tasks Web UI). Filter by namespace, task
queue, status, time range. Inspect input/output/history for any run.

CLI:

```bash
# describe a stuck workflow
tasksctl workflow describe --namespace <org> <workflow-id>

# replay events for debugging
tasksctl workflow history --namespace <org> <workflow-id> --json

# queue depth (per-queue backlog)
tasksctl queue stats --namespace <org> <queue-name>

# force-fail a runaway workflow
tasksctl workflow terminate --namespace <org> <workflow-id> --reason <text>
```

Metrics (Prometheus, scraped from `tasksd` `/metrics`):

| Metric | Labels |
|---|---|
| `tasks_workflow_runs_started_total` | namespace, workflow_type |
| `tasks_workflow_runs_completed_total` | namespace, workflow_type, status |
| `tasks_workflow_runs_failed_total` | namespace, workflow_type |
| `tasks_activity_attempts_total` | namespace, activity_type, outcome |
| `tasks_queue_backlog` | namespace, queue |
| `tasks_workflow_run_duration_seconds` | namespace, workflow_type (histogram) |

Traces: every workflow + activity emits an OTEL span. Span links connect
the originating HTTP handler → workflow start → each activity attempt.

Logs: workflow + activity logs are structured slog records with
`workflow_id`, `run_id`, `namespace`, `task_queue`, `attempt`. They
land in the cluster's logging backend (Loki, in our case).

## 9. What this replaces

This contract supersedes:

- Per-service `pkg/taskqueue/` and `pkg/tasks/` wrappers that wrap a
  fork of the SDK with HTTP fallbacks. The wrappers will be removed
  once direct callers migrate to `github.com/hanzoai/tasks/pkg/sdk`.
- `go.temporal.io/sdk/*` imports anywhere outside this repo. Any
  service that still imports the legacy SDK gets migrated in the
  dispatched follow-up work; new code must not import it.
- Ad-hoc `time.NewTicker` + `go func()` for retries, cron, or scheduled
  send. If you wrote one of those, you owe a tasks workflow.

## 10. Versioning

The Hanzo Tasks module pins to `v1.x.x` forever per the global Go
package policy (no `v2`+ semver-import-versioning). New features land
as minor bumps; fixes as patches. See LLM.md §"Versioning policy".
