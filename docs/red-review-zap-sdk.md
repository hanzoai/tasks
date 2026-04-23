# Red Review — ZAP SDK (pkg/sdk) on feat/zap-internal

Reviewer: Red (adversarial)
Branch state at review: `a9a061e06 feat(sdk/worker): native poll/dispatch runtime, zero upstream deps`
HEAD: `a9a061e06`. All five Blue commits landed:
- `7174e9d81 feat(sdk/temporal)` — Blue#1
- `86961b48b feat(sdk/activity)` — Blue#3
- `f928a20fc feat(sdk/workflow)` — Blue#4
- `a3c9d818d feat(sdk/client)` — Blue#2
- `a9a061e06 feat(sdk/worker)` — Blue#5 (landed during review)

Build + race-test pass against the delivered tree:

```
ok  github.com/hanzoai/tasks/pkg/sdk/activity
ok  github.com/hanzoai/tasks/pkg/sdk/client
ok  github.com/hanzoai/tasks/pkg/sdk/temporal
ok  github.com/hanzoai/tasks/pkg/sdk/worker
ok  github.com/hanzoai/tasks/pkg/sdk/workflow
```

That is the thinnest possible definition of "green." The findings below are everything the green bar does not catch.

---

## 1. Import purity

Scope: `pkg/sdk/**`.

```
$ rg 'go\.temporal\.io' pkg/sdk     # all hits are comments only
$ rg 'google\.golang\.org/grpc' pkg/sdk   # all hits are comments only
```

**Result: clean.** No `go.temporal.io/*` imports, no `google.golang.org/grpc` imports anywhere under `pkg/sdk/`. References are all in package-doc comments and migration notes. The policy stated in `pkg/sdk/doc.go` is honoured.

Severity: n/a (pass).

Caveat — this only covers `pkg/sdk/`. Outside the SDK, `go.temporal.io/sdk` and `go.temporal.io/api` are still pervasive in `tests/`, `service/worker/workerdeployment/`, and `temporaltest/`. Flagging because the task spec says "zero go.temporal.io imports anywhere in the binary", and the binary still links those. Example hits:

```
service/worker/workerdeployment/workflow.go
service/worker/workerdeployment/version_workflow.go
service/worker/workerdeployment/client.go
service/worker/workerdeployment/util.go
tests/xdc/...
tests/testcore/functional_test_base.go
temporaltest/embedded/server.go
temporaltest/internal/lite_server.go
```

CI cannot enforce `go.temporal.io/sdk = 0 hits` binary-wide until these are migrated or vendored behind the new surface.

---

## 2. Interface coverage — gap matrix

Caller repos surveyed:
- `~/work/hanzo/base/plugins/tasks/` — `workflows.go`, `activities.go`, `worker.go`, `durable.go`.
- `~/work/hanzo/commerce/billing/workflows/` — `dunning.go`, `register.go`, `subscription.go`.
- `~/work/hanzo/commerce/infra/tasks.go`.
- `~/work/hanzo/ta` — **does not exist** on this machine. Review was performed against base + commerce only.

Caller symbol universe (extracted via grep):

```
activity.GetLogger
activity.RecordHeartbeat
client.Client, client.Close, client.Dial, client.Options
client.ExecuteWorkflow, client.SignalWorkflow, client.CancelWorkflow,
client.TerminateWorkflow, client.SignalWithStartWorkflow,
client.GetWorkflow, client.QueryWorkflow, client.CheckHealth
client.StartWorkflowOptions, client.WorkflowRun, client.WorkflowRunGetOptions
temporal.RetryPolicy
worker.InterruptCh, worker.New, worker.Options, worker.Worker (w.Run / w.Start / w.Stop)
workflow.ActivityOptions, workflow.Context, workflow.Future, workflow.ReceiveChannel
workflow.ExecuteActivity, workflow.ExecuteChildWorkflow, workflow.LocalActivityOptions
workflow.GetLogger, workflow.GetSignalChannel, workflow.NewSelector, workflow.NewTimer
workflow.Now, workflow.WithActivityOptions, workflow.WithCancel

Type constructors via external packages (must be satisfied or factored):
go.temporal.io/api/workflowservice/v1.ListWorkflowExecutionsRequest
client.DescribeWorkflowExecution → *workflowservice.DescribeWorkflowExecutionResponse
response.WorkflowExecutionInfo.GetStatus().String()
run.Get(ctx, result) and run.GetWithOptions(ctx, result, opts)
```

Gap table — SDK symbol callers need but no Blue agent shipped:

| Needed by caller | Status | Severity | Note |
|---|---|---|---|
| `workflow.ExecuteChildWorkflow` | **missing** | HIGH | commerce/billing/workflows/subscription.go L157 calls `workflow.ExecuteChildWorkflow(ctx, DunningWorkflow, ...)`. Absent. |
| `workflow.LocalActivityOptions` (type + `WithLocalActivityOptions`) | **missing** | HIGH | commerce/infra/tasks.go L346-355. Absent. |
| `client.QueryWorkflow` | **missing** | HIGH | commerce/infra/tasks.go L189-202. Absent from `client.Client`. |
| `client.SignalWithStartWorkflow` | **missing** | HIGH | commerce/infra/tasks.go L146-169. Absent. |
| `client.GetWorkflow(ctx, id, runID) WorkflowRun` | **missing** | HIGH | commerce/infra/tasks.go L127-134. Absent. |
| `client.CheckHealth(ctx, req) (*resp, error)` | **shape mismatch** | HIGH | commerce/infra/tasks.go L255. SDK has `Health(ctx)(string,string,error)`; commerce expects the upstream shape. |
| `client.WorkflowRun.GetWithOptions(ctx, valuePtr, opts)` | **missing** | MEDIUM | commerce/infra/tasks.go L317-319. Absent from `client.WorkflowRun`. |
| `client.WorkflowRunGetOptions` | **missing** | MEDIUM | commerce/infra/tasks.go L317. Absent. |
| `worker.InterruptCh() <-chan interface{}` (package-level) | **missing** | HIGH | commerce/infra/tasks.go L236, L243. The package-level constructor is absent; `Worker.Run(interruptCh <-chan any)` exists but the idiom `w.Run(worker.InterruptCh())` won't compile. |
| `worker.Options.MaxConcurrentWorkflowTaskExecutionSize` | **missing** | MEDIUM | commerce/infra/tasks.go L214. Only `MaxConcurrentWorkflowTaskPollers` is exposed; commerce expects the execution-size knob. |
| `worker.Options.MaxConcurrentLocalActivityExecutionSize` | **missing** | MEDIUM | commerce/infra/tasks.go L215. Absent. |
| `worker.Options.WorkerActivitiesPerSecond` | **missing** | MEDIUM | commerce/infra/tasks.go L216. Absent. |
| `worker.Options.WorkerLocalActivitiesPerSecond` | **missing** | MEDIUM | commerce/infra/tasks.go L217. Absent. |
| `worker.Options.TaskQueueActivitiesPerSecond` | **missing** | MEDIUM | commerce/infra/tasks.go L218. Absent. |
| `worker.Options.EnableSessionWorker` | **missing** | MEDIUM | commerce/infra/tasks.go L219. Absent. |
| `client.ListWorkflow(ctx, *workflowservice.ListWorkflowExecutionsRequest) (*Resp, error)` | **shape mismatch** | HIGH | base/plugins/tasks/durable.go L314, L373. base imports the upstream `workflowservice` package and passes a protobuf request. SDK ships `ListWorkflows(ctx, query, pageSize, token)` with a totally different signature + zero `workflowservice` types. base will not compile post-migration without a rewrite. |
| `client.DescribeWorkflowExecution(ctx, id, runID)` | **shape mismatch** | HIGH | base/plugins/tasks/durable.go L256-275. base calls `desc.WorkflowExecutionInfo.GetStatus().String()` — protobuf-generated accessor chain. SDK returns a flat `*WorkflowExecutionInfo` with `Status WorkflowStatus` (int8). base needs a full conversion layer. |
| `workflow.GetLogger` | present | — | OK. |
| `workflow.GetSignalChannel` | present | — | OK. |
| `workflow.NewSelector.AddFuture/AddReceive/AddDefault/Select` | present | — | OK. |
| `workflow.NewTimer / WithCancel / WithActivityOptions / ExecuteActivity` | present | — | OK. |
| `workflow.Now` | present | — | OK. |
| `workflow.ReceiveChannel.Receive(ctx, valPtr)` → **signature mismatch** | **bug** | HIGH | See Finding #6.2 below. SDK's `ReceiveChannel.Receive` returns `(ok bool)`; base/plugins/tasks/workflows.go calls it for its side effect only (`ch.Receive(ctx, &data)` ignoring return) which is source-compatible, but Temporal's upstream returns `void` — a two-valued returns force unused-returns linters to complain and any Go code that used `ch.Receive(ctx, &v)` as an expression fragment breaks. Low-blast but must be documented. |
| `temporal.RetryPolicy` | present | — | OK. Shape matches. |
| `activity.GetLogger`, `activity.RecordHeartbeat`, `activity.GetInfo` | present | — | OK. |

**Before `base` or `commerce` can flip their imports, every HIGH row above is a compile-time blocker.** That is the feature matrix CI needs to gate on before enforcing the `go.temporal.io/sdk = 0 hits` rule.

---

## 3. Wire-contract adherence (schema/tasks.zap vs. implementation)

### 3.1 Opcode-to-RPC mapping — **pass**

All 20 RPCs declared in `schema/tasks.zap` are backed by an opcode in the client or worker_transport. No RPC in the schema is orphaned; no opcode in the implementation is orphaned. Allocation:

```
0x0050-0x005F  legacy pkg/tasks one-shot (reserved; unchanged)
0x0060-0x0065  startWorkflow, signal, cancel, terminate, describe, listWorkflows
0x0070-0x0073  createSchedule, listSchedules, deleteSchedule, pauseSchedule
0x0080-0x0082  registerNamespace, describeNamespace, listNamespaces
0x0090         health
0x00A0-0x00A5  pollWorkflowTask, pollActivityTask, respond* (workflow/activity), heartbeat
```

Note on stability: `transport.go` historically declared worker opcodes at `0x0070-0x007F` (first landing of Blue#2 stored them there), which collided with client schedule opcodes. Blue#2's second revision moved them to `0x00A0-0x00A5`. If any pre-GA server build is already deployed that used the `0x0070-0x007F` layout, it will silently route worker polls to `createSchedule/listSchedules/...` and produce decode errors that look transient. The append-only contract in `schema/tasks.zap` must be respected — document the relocation in the schema file so no server build references the old numbers.

### 3.2 Request/response struct layout — **mismatches**

The schema is the authoritative wire contract. Implementations deviate:

**3.2.1 Payloads wrapping lost — MEDIUM, cross-service compat issue**

Schema:
```
struct RespondActivityTaskCompletedRequest
  taskToken Bytes
  result Payloads                 # = struct { items List(Payload) }

struct RecordActivityTaskHeartbeatRequest
  taskToken Bytes
  details Payloads

struct ActivityTask
  ...
  input Payloads

struct WorkflowTask
  ...
  workflowType WorkflowType       # struct { name Text }
  workflowExecution WorkflowExecution   # struct { workflowId, runId }
```

Implementation (`pkg/sdk/client/transport.go` + `worker_transport.go`):
```
RespondActivityTaskCompletedRequest.Result []byte          # raw bytes, not Payloads
RecordActivityTaskHeartbeatRequest.Details []byte          # raw bytes, not Payloads
ActivityTask.Input []byte                                  # raw bytes, not Payloads
WorkflowTask.WorkflowTypeName string                       # flat, not WorkflowType
WorkflowTask.WorkflowID / RunID string                     # flat, not WorkflowExecution
PollWorkflowTaskRequest.TaskQueueName / TaskQueueKind     # flat, not TaskQueue
```

Every Payloads → Bytes collapse is a **schema mismatch** that will produce frames no schema-conformant server (or alternate worker implementation) can decode. In particular:
- A MIME-type roundtrip (`metadata Bytes` inside Payload) is lost — the worker has no way to honour non-JSON payloads coming off the wire.
- A server that emits the canonical schema's nested object will produce zap frames with fields at offsets the client does not read; client will see empty strings/bytes for `WorkflowTypeName`, `WorkflowID`, etc. The workflow type lookup will then fall through to the "no workflow registered" path (dispatch.go L51-66) and the server will see every workflow silently "complete with empty commands."

**3.2.2 High-level RPC bodies are JSON-blob-in-ZAP-envelope, not schema-native**

Schema:
```
startWorkflow (req StartWorkflowRequest) -> (resp StartWorkflowResponse)
```
`StartWorkflowRequest` is a typed ZAP struct with 8 fields; `StartWorkflowResponse` is `{ runId Text }`.

Implementation (`pkg/sdk/client/client.go` L235-254):
```go
// envelope:
//   field 0  Bytes  = JSON body
//   field 8  Uint32 = status
//   field 12 Bytes  = error detail
```

Every high-level RPC (startWorkflow, signal, cancel, terminate, describe, list, schedule/*, namespace/*, health) is sent as a single `Bytes` field holding JSON. The schema structs are **not populated at all** — their field offsets never appear in the wire. A server that parses `StartWorkflowRequest` per the schema sees an unknown-field frame. A ZAP peer that validates schema will reject these frames outright.

The workaround is documented ("v1 wire note: single Bytes field") but the schema file (`schema/tasks.zap`) does not say that — the schema currently lies about what's on the wire.

**3.2.3 Health RPC response shape — MEDIUM**

Schema: `health () -> (service Text, status Text)` — two typed fields on the response object.

Client (`pkg/sdk/client/schedule.go` L212-222): sends `{}` as a JSON body and unmarshals a JSON `{"service":"…","status":"…"}` out of the body field. Schema-conformant server emitting `(service Text, status Text)` at field offsets N, M will not populate the JSON body. The client will then decode `""` for service/status.

**3.2.4 Response envelope status at Uint32 field 8 — MINOR schema drift**

The wire envelope declares `status` at field offset 8 and `error` at field offset 12. The schema has no "envelope" concept; each RPC's response is whatever its schema struct declares. The current envelope forces every response to carry 24 bytes of object header + status + error slots that the schema does not declare, and steals field offset 0 for the JSON body. If a future schema RPC declares a struct with a Bytes field at offset 0, the wire will be ambiguous.

**Action for Blue (wire contract):** Either (a) rewrite the schema to declare this JSON-envelope explicitly as the v1 contract and state that every high-level RPC embeds its real body in field 0, or (b) land native ZAP serde for each RPC that matches the declared struct. Until one of the two is done, `schema/tasks.zap` is a lie. Pick one and make it true.

---

## 4. Determinism — where replay will diverge

### 4.1 Phase-1 "replay" is a stub, by declaration

`pkg/sdk/worker/dispatch.go` L71:
```go
env := workflow.NewStubEnv()
ctx2 := workflow.NewContextFromEnv(env)
```

Every time a WorkflowTask is dispatched the worker constructs a fresh `workflow.StubEnv` and runs the user's workflow function from scratch against it. There is no event log, no replay, no history scheduler. This is acknowledged in `pkg/sdk/workflow/env.go` L24-32 as Phase 1. Accepting that, here are the concrete traps user code will hit today:

### 4.2 Activities never touch the wire in Phase 1 — CRITICAL behavioural surprise

`dispatch.go` line 71 uses `workflow.NewStubEnv()` — **the same StubEnv used by tests, with no activity registrations populated at runtime.** `stub.go` L245-275 resolves `ExecuteActivity` by looking up `e.activities[name]` and returns the pre-registered response; if none exists it settles the Future with `(nil, nil)` — **success, empty payload.** That means in production today:

- A workflow that calls `workflow.ExecuteActivity(ctx, "ExecuteTask", task)` gets a Future that settles immediately with `nil, nil`.
- `f.Get(ctx, &result)` decodes `nil` into `result` — no-op, no error.
- `task.State` stays whatever default it was initialised to.
- Server receives an empty commands response (`dispatch.go` L101 hard-codes `Commands: nil`).

Base's `AgentTaskWorkflow` (base/plugins/tasks/workflows.go L65) would enter this path with `actFuture` settling to nil immediately, then Selector would fire the future case and set `task.State = TaskCompleted` with `task.Output = nil` — **silent task "success" with no activity ever running.** A production user hitting this path would see workflows flip to completed instantly, with no side effects, no audit trail, and no way to distinguish a successful activity from a stubbed one.

This is the biggest blast radius of the delivery. It's not a bug by intent — it's the declared Phase-1 shape — but a service that imports this SDK and doesn't realise the StubEnv is the runtime will ship broken workflows to production.

**Fix priority:** the worker must either (a) dispatch activities over the wire via its own CoroutineEnv, or (b) refuse to start when the registered workflow calls `ExecuteActivity` and the dispatch env is `StubEnv`. Either way, the current behaviour (silent no-op) is unacceptable.

### 4.3 `StubEnv.Select` busy-spins — HIGH, production DoS

`stub.go` L292-312:
```go
for {
    if idx := readyIndex(cases); idx >= 0 {
        return idx
    }
    select {
    case <-sc.done: return -1
    case <-time.After(time.Millisecond): // re-check
    }
}
```

Every 1 ms the selector re-scans its cases. `time.After(time.Millisecond)` allocates a new timer each iteration — no caching. With N workers running N workflows each with M selector loops:
- CPU: N·M·1000 Hz of re-scan work + timer allocation.
- Memory: one `*time.Timer` leak per iteration until GC'd.

Under moderate load (100 concurrent workflows × 3 selectors each) this burns a core just to spin. On a Kubernetes pod with a CPU limit this will throttle the real workflow path.

**Fix priority:** replace the spin with a condvar or a signal channel fan-in that parks until one of the cases can emit. See the standard Temporal dispatcher idiom — one goroutine per workflow driven by a `runChan` that the settlement/signal paths write to.

### 4.4 `StubEnv.NewTimer` fires instantly + advances clock — determinism violation

`stub.go` L235-243:
```go
func (e *StubEnv) NewTimer(d time.Duration) Future {
    f := NewFuture()
    e.AdvanceClock(d)
    f.Settle(nil, nil)
    return f
}
```

In a real workflow, a timer fires at the right workflow time and the selector unblocks. In Phase-1 the timer is already settled when `NewTimer` returns. That means:

- Base's `AgentTaskWorkflow` builds `workflow.NewTimer(timerCtx, 24h)` and tops the selector with it. In production, the selector sees the timer as ready immediately; `timerFuture.Get` returns `nil`; the callback runs the timeout path; workflow status becomes "timeout" on first poll. The real 24-hour wait collapses to a few microseconds.
- Commerce's `DunningWorkflow` schedules `workflow.NewTimer(timerCtx, delay)` between dunning attempts. Today the delay is zero; dunning fires every retry back-to-back as fast as the worker can poll. The retry schedule (`24h / 72h / 168h`) is ignored. A payment processor that dunning-spams is an automatic customer incident.
- Commerce's `SubscriptionLifecycleWorkflow.Phase2` is a forever-loop with `NewTimer(PeriodEnd.Sub(Now))`. With instant timers, `workflow.Now(ctx)` returns a stub clock that jumps on each call. `sleepDuration = params.PeriodEnd.Sub(now)` evaluates once with the stub clock; the timer fires immediately; the next iteration sees the clock already past PeriodEnd. The workflow immediately invokes `RenewSubscriptionActivity`. In production this bills every subscriber on their first workflow task rather than at period end.

**Fix priority:** Phase-1 is not safe for billing workflows. Block shipping commerce/billing on this SDK until the worker owns a real event-time scheduler.

### 4.5 `workflow.Now(ctx)` vs `time.Now()` — no enforcement

The doc (`workflow/env.go` L36-38) forbids calling `time.Now`, `rand.Read`, spawning goroutines inside workflow functions. There is no linter or runtime check. Existing caller code already drifts:

- `base/plugins/tasks/workflows.go` L109-110, L120-121: `now := time.Now().UTC(); task.CompletedAt = &now` inside the workflow function. Today this calls wall-clock time even though the workflow is supposedly deterministic. The symptom: on replay (Phase 2) different runs stamp different completion times.
- `commerce/billing/workflows/dunning.go` — no wall clock, but the workflow builds `dunningSchedule = []time.Duration{...}` as a package var which is compiled in; if the binary restarts with a different value (e.g. A/B test config flag), replays would use a different schedule.

**Fix priority:** ship the `tasks-vet` linter mentioned in the env.go doc before Phase 2, and fail `base` compilation. CI can grep for `time.Now()` inside functions whose first parameter is `workflow.Context`.

### 4.6 Workflow arg decode path conflates history with input — HIGH

`dispatch.go` L77-78:
```go
args, decodeErr := decodeWorkflowArgs(fn, ctx2, task.History)
```

Schema declares `WorkflowTask.history Bytes` as "encoded WorkflowHistory frame" — the entire event log. The client decodes it as a JSON array of workflow arguments. Every WorkflowTask's `history` field becomes the input JSON. So:

- On any non-first workflow task (after a signal), the server must package the start-input into `history` again or the worker will re-decode yesterday's history bytes as this morning's args.
- Server implementations of `pollWorkflowTask` that emit a real event log would produce `history` bytes the client cannot decode as `[]json.RawMessage` — `appendDecodedArgs` falls back to "treat the whole input as one arg" (L412-416), which silently binds the history bytes to arg[0]. The user's activity then receives garbage.

**Fix priority:** separate the input and the history in the schema (`WorkflowTask.input Payloads` + `history Bytes`) and in the transport, and have dispatch read `input` for arg decoding. The current shape is not even self-consistent for Phase 1.

### 4.7 `invokeFunc` swallows the workflow's return error — HIGH

`dispatch.go` L94:
```go
// Invoke synchronously. Errors from the workflow itself are captured
// by the engine — the worker's job is to ship commands; errors
// manifest as commands in Phase 2.
_ = invokeFunc(fn, args)
```

The workflow returns `(T, error)`; the worker discards `err`. A workflow that returns `fmt.Errorf("boom")` today will respond to the server with "success, empty commands." The server will mark the workflow completed rather than failed. There is zero feedback loop for a workflow-level error path.

Commerce's `DunningWorkflow` returns nil on its happy paths but returns a real error when the dunning workflow itself crashes (`return err` inside Phase 2 of subscription.go L147). That error is silently dropped.

**Fix priority:** encode workflow errors into a `FailureCommand` today — a one-commit Phase-1 fix, not a Phase-2 deferral.

---

## 5. Ranked attack surface

### 5.1 [CRITICAL] Workflow activities are no-ops in production dispatch
Description: `pkg/sdk/worker/dispatch.go` L71 uses `workflow.NewStubEnv()` as the workflow runtime, which means every `workflow.ExecuteActivity` settles to `(nil,nil)` without ever contacting the matching service. Workflows appear to succeed; no activity runs.
Location: pkg/sdk/worker/dispatch.go:71, pkg/sdk/workflow/stub.go:245-275
Attack Complexity: None — default behaviour.
Exploitability: Any service that imports this SDK and calls `ExecuteActivity`.
Impact: Silent data corruption — billing/dunning/task workflows appear complete while doing nothing. Tasks marked `Completed` without the executor running. Payment processor emits invoices that never post.
Detectability: No. Telemetry will show workflow completions + zero activity invocations, a pattern nobody is looking for.
Fix Hint: The worker must dispatch activities over the wire (pollActivityTask / respondActivityTaskCompleted) or must refuse to Start when a registered workflow calls ExecuteActivity against a StubEnv runtime.

### 5.2 [CRITICAL] Billing timers collapse to zero duration
Description: `StubEnv.NewTimer` settles the Future immediately and advances the stub clock by the requested duration. Any workflow using `workflow.NewTimer` executes its "after timer fires" path on the first poll. Commerce subscription/dunning schedules collapse.
Location: pkg/sdk/workflow/stub.go:235-243; consumed at pkg/sdk/worker/dispatch.go:71
Attack Complexity: None — default behaviour.
Exploitability: First commerce deploy.
Impact: Dunning fires back-to-back without the 24h/72h/168h gaps; subscription renewal happens at workflow start rather than period end; customer billed early; dunning emails spammed.
Detectability: Visible if monitoring tracks dunning interval histograms. Otherwise not.
Fix Hint: Do not ship commerce/billing on Phase 1. Worker must own an event-time scheduler that parks timers until the clock advances.

### 5.3 [HIGH] Missing SDK symbols block base and commerce migration
Description: `client.SignalWithStartWorkflow`, `client.GetWorkflow`, `client.QueryWorkflow`, `client.CheckHealth`, `workflow.ExecuteChildWorkflow`, `workflow.LocalActivityOptions`, `worker.InterruptCh`, and multiple `worker.Options` fields are absent. See §2 for the full table.
Location: pkg/sdk/{client,workflow,worker}/
Attack Complexity: n/a — this is a compile-time gap.
Exploitability: n/a.
Impact: `go build` of hanzoai/base and hanzoai/commerce fails after import swap. CI cannot enforce `go.temporal.io/sdk = 0 hits` until every row is closed.
Fix Hint: Prioritise this list over Phase-2 replay. Without it the migration cannot begin.

### 5.4 [HIGH] Schema lies: `StartWorkflowRequest` fields never touch the wire
Description: Every high-level RPC is sent as a JSON body inside a generic `{body, status, error}` envelope (client.go L235-254). The `startWorkflowRequest` struct declared in `schema/tasks.zap` — with its Payloads, Timeouts, RetryPolicy sub-structs — is not emitted on the wire. A schema-conformant server will reject these frames as malformed. A schema-driven client in another language will not interoperate.
Location: schema/tasks.zap:77-95; pkg/sdk/client/client.go:235-254
Attack Complexity: High (cross-implementation interop only).
Exploitability: Any non-Go ZAP peer.
Impact: The canonical wire contract is unenforced. Wire-format drift is invisible.
Fix Hint: Document the JSON envelope in the schema OR ship native ZAP serde for every RPC. Pick one.

### 5.5 [HIGH] Worker encodes `input Payloads` as raw Bytes
Description: Schema declares `ActivityTask.input Payloads` (a list of `{metadata Bytes, data Bytes}`); transport.go collapses it to `Input []byte`. MIME types and multi-argument encoding (two Payload entries = two args) are lost. Workers in other languages that produce schema-conformant Payloads will see empty inputs.
Location: pkg/sdk/client/transport.go:145-155; pkg/sdk/client/worker_transport.go:194-211
Attack Complexity: n/a.
Exploitability: Interop.
Impact: Multi-language worker interop broken. Activity args that are not flat JSON are unrepresentable.
Fix Hint: Honour the schema; encode `Payloads` as a nested list of `{metadata, data}`.

### 5.6 [HIGH] WorkflowTask.WorkflowType is flattened, losing schema layering
Description: Schema: `WorkflowTask.workflowType: WorkflowType` where `WorkflowType = struct { name Text }`. Implementation: flat `WorkflowTypeName string`. A server emitting the nested struct will produce a frame where `WorkflowTypeName` at the client's field 24 offset is empty — the worker will look up an empty-string name in its registry and fall through to "no workflow registered" (dispatch.go L51-66), silently responding with empty commands.
Location: pkg/sdk/client/transport.go:133-140; pkg/sdk/client/worker_transport.go:177-191
Attack Complexity: n/a.
Exploitability: Any real server that emits the schema-conformant WorkflowTask.
Impact: Every workflow silently becomes a no-op on the worker. Same symptom as §5.1 via a different path.
Fix Hint: Encode/decode nested WorkflowExecution + WorkflowType per schema.

### 5.7 [HIGH] Workflow errors silently swallowed in dispatch
Description: `_ = invokeFunc(fn, args)` in dispatch.go L94 discards the workflow's return error. Workflows that explicitly `return err` are reported as completed.
Location: pkg/sdk/worker/dispatch.go:94
Attack Complexity: None.
Exploitability: Any workflow that returns an error.
Impact: Failure states invisible to the engine. Retry policies never trigger. Audit logs show success.
Fix Hint: Emit a `FailureCommand` on non-nil error. Trivial fix, do not defer to Phase 2.

### 5.8 [HIGH] `PollWorkflowTaskRequest.taskQueue` flattened — schema mismatch
Description: Schema: `taskQueue: TaskQueue` (`name Text, kind Int8`). Implementation: flat `TaskQueueName string` + `TaskQueueKind int8` encoded as separate top-level fields. A server reading a schema-conformant nested TaskQueue struct sees an empty task-queue name on every worker poll.
Location: pkg/sdk/client/transport.go:116-130; pkg/sdk/client/worker_transport.go:112-134
Impact: No workers receive tasks. Poll returns unrouted.
Fix Hint: Encode as nested TaskQueue per schema.

### 5.9 [MEDIUM] `StubEnv.Select` busy-spin + timer-allocation storm
Description: 1 ms spin + `time.After` allocation per cycle per workflow. Scales as N·M·1000 per-second where N = live workflows, M = selectors per workflow. On a typical 100-workflow service this is ~300k allocations/second in GC, plus 1 CPU burned.
Location: pkg/sdk/workflow/stub.go:292-312
Attack Complexity: None.
Exploitability: Any high-throughput workload.
Impact: CPU throttling, GC pressure, elevated p99 on every unrelated operation in the process.
Fix Hint: Replace with a condvar + per-env "readable" channel that the settlers write to.

### 5.10 [MEDIUM] Heartbeat sink captures closure with live context — leak-prone
Description: `dispatch.go` L191-201 closes over `ctx, task, w.transport` inside `scope.HeartbeatSink`. After the activity returns, if user code spawned a goroutine that still holds the scope and calls `activity.RecordHeartbeat`, the sink fires on a stale task token with a stale ctx. If the worker has recycled the task slot the heartbeat hits the wrong task (best case: stale-token rejection; worst case: server lookup accepts it and updates the wrong activity's heartbeat time).
Location: pkg/sdk/worker/dispatch.go:189-201
Attack Complexity: Medium — requires user code to leak a goroutine.
Exploitability: Low in practice, but reproducible.
Impact: Stale-token writes on the wire; cross-task state confusion under goroutine-leak conditions.
Fix Hint: Nil out `scope.HeartbeatSink` after dispatch returns, or gate it on an atomic `stopped` flag.

### 5.11 [MEDIUM] `json.Marshal` failures silently dropped in heartbeat sink
Description: `payload, _ := json.Marshal(details)` (dispatch.go L192). If marshal fails (channel, func, cyclic reference in user details) the heartbeat ships an empty payload. User's liveness-with-details becomes liveness-without-details; detail-dependent server logic sees empty.
Location: pkg/sdk/worker/dispatch.go:192
Fix Hint: Wrap the error path — log at warn, use an empty-but-valid heartbeat or fall back to `temporal.Encode` shape.

### 5.12 [MEDIUM] Client.DescribeWorkflow shape incompatible with base caller
Description: base/plugins/tasks/durable.go L256-275 calls `desc.WorkflowExecutionInfo.GetStatus().String()` (upstream protobuf accessors). SDK returns `*client.WorkflowExecutionInfo` with `Status client.WorkflowStatus` (int8). Every base call site breaks post-migration.
Location: pkg/sdk/client/workflow.go:25-49
Impact: base is not migrable with the current client surface.
Fix Hint: Provide a conversion helper or change the ListWorkflow/DescribeWorkflow return shape to be a superset of what base needs.

### 5.13 [MEDIUM] Opcode allocation history includes a reuse
Description: Blue#2's first revision of `transport.go` declared worker poll opcodes at `0x0070-0x0075`, which already belong to schedule ops. Second revision moved worker to `0x00A0-0x00A5`. If any server build shipped against the first revision, it will route worker polls to `createSchedule/listSchedules/...` — silent miswiring that returns decode errors.
Location: pkg/sdk/client/transport.go:21-32 (current state)
Fix Hint: Pin an explicit "opcode history" block in schema/tasks.zap stating what each opcode was used for and when. Opcodes are append-only; document the false-start.

### 5.14 [MEDIUM] Info TaskToken is copied on return but raw-referenced in-worker
Description: `activity.GetInfo` deep-copies TaskToken to the caller (good). Inside the worker, `scope.Info.TaskToken = copyBytes(task.TaskToken)` is a fresh copy — but the HeartbeatSink closure (`dispatch.go` L195) captures `task.TaskToken` directly, not via scope. An adversarial activity that tampers with `scope.Info.TaskToken` via reflection cannot affect the sink, but if future refactors point the sink at `scope.Info.TaskToken`, the raw-vs-copy distinction becomes load-bearing for isolation.
Location: pkg/sdk/worker/dispatch.go:175-201
Fix Hint: Route all sink writes through a single `tokenForTask(task)` accessor.

### 5.15 [MEDIUM] `workflow.NewFuture` is exported but documented as internal
Description: `future.go` L60 exports `NewFuture() Settleable` for "worker / StubEnv" use; user workflow code is not expected to call it. It is reachable from any import of `github.com/hanzoai/tasks/pkg/sdk/workflow`. A user who calls `workflow.NewFuture()` and Settles it from a goroutine will bypass the entire scheduler, break replay determinism, and see no warning.
Location: pkg/sdk/workflow/future.go:60
Fix Hint: Move NewFuture into an `internal/` subpackage or a separate `workflow/test` package that user code does not pull in by default.

### 5.16 [LOW] Workflow+activity input decoder silently binds garbage to arg[0]
Description: `appendDecodedArgs` (dispatch.go L404-430) tolerates malformed JSON by treating the whole input as `arg[0]` when the function signature takes exactly one non-context argument. If the server starts sending a real history log that happens to not parse as a JSON array, the workflow runs with garbage bound to its first argument.
Location: pkg/sdk/worker/dispatch.go:407-417
Impact: Silent data corruption.
Fix Hint: Only take the "single arg" fallback when the input looks like a JSON object or scalar, not a binary blob.

### 5.17 [LOW] `dispatch.go` dispatch panic path responds with "completed, empty commands" not a failure
Description: Panics in `dispatchWorkflowTask` are logged but the deferred handler does not respond to the frontend. The poll loop returns from the task with no response sent. The server will eventually re-poll-timeout the task and retry, but the error is invisible.
Location: pkg/sdk/worker/dispatch.go:39-49
Impact: Server sees "timed out workflow tasks" rather than a specific panic failure. Debugging harder.
Fix Hint: On panic, ship a `RespondWorkflowTaskCompleted` with a failure-command payload (or a `RespondWorkflowTaskFailed` RPC if the schema adds one).

### 5.18 [INFO] Blue#5 landed on the slowest path — commit timing
Description: Blue#5 (worker) landed their commit after Blues 1/2/3/4 had already committed. During the review window the worker package sat in `git status` as `?? pkg/sdk/worker/` for ~8 minutes, then a9a061e06 landed. Not a bug — a scheduling observation: the other Blues should not have force-pushed during that window since a rebase would have discarded Blue#5's working tree.
Location: timing of a9a061e06 vs. a3c9d818d
Impact: None post-fact (all five landed cleanly). Flagged as operational hygiene for future parallel-Blue runs: commit incrementally, do not force-push a sibling.
Fix Hint: Nothing to fix on the code. Coordinator — serialize the final push window.

---

## Blue Handoff

### What Blue got right
- **Import purity inside pkg/sdk** is clean — zero `go.temporal.io` imports, zero `google.golang.org/grpc` imports (Finding §1).
- **Opcode coverage** is exhaustive — all 20 schema RPCs have an opcode (Finding §3.1).
- **`temporal` package** (Blue#1) is production-quality: retry normalisation respects zero-field defaults (retry.go L59-77), error classification + sentinel matching + round-trip serde + fail-secure decode of hostile bytes are all correct and tested. Good.
- **`activity` package** (Blue#3) is tight: defensive TaskToken deep-copy on every `GetInfo` return (activity.go L151-159), scopeKey as a private type to prevent cross-package impersonation (L116-118), nil-safe `Scope.Heartbeats()` (L73-89). Race-tested concurrent Heartbeat collection passes.
- **`workflow.chanImpl`** (Blue#4) correctly handles the rendezvous + close-after-drain semantics. Race-tested FIFO + unbuffered + closed-channel paths pass.
- **`client` test doubles** (Blue#2) are clean enough to support migration work without real ZAP — `stubTransport` pattern is the right shape for caller tests.

### What Blue missed (that Blue did not flag)
1. **The worker's dispatch env is `StubEnv`** (Finding §5.1). This is the whole runtime — activities don't hit the wire. The package doc frames it as "Phase 1" but the user-visible effect is that every workflow silently succeeds with no side effects. This is not a Phase-2 deferral; it is a shipping showstopper.
2. **Timers fire instantly** (Finding §5.2). Direct corollary of StubEnv — commerce/billing dunning schedules collapse to zero delay.
3. **Workflow return errors are discarded** (Finding §5.7). Trivial one-commit Phase-1 fix that Blue#5 did not do.
4. **Commerce + base cannot compile** on the shipped client/worker surface — `SignalWithStartWorkflow`, `GetWorkflow`, `QueryWorkflow`, `ExecuteChildWorkflow`, `LocalActivityOptions`, `worker.InterruptCh`, and the rate-limiting worker.Options fields are all absent (§2 gap table).
5. **`schema/tasks.zap` does not match what the client actually sends** — high-level RPCs are JSON blobs inside a generic envelope, not the schema's declared structs (§3.2.2).
6. **Worker request structs (PollWorkflowTask, ActivityTask, WorkflowTask) flatten `TaskQueue`, `WorkflowType`, `WorkflowExecution`, `Payloads`** — every real server will produce frames the worker can't decode (§3.2.1, §5.6, §5.8).
7. **`StubEnv.Select` busy-spins** at 1 kHz with a per-iteration timer allocation (§5.9). Not safe at production load.
8. Blue#5 landed last during the review window (§5.18) — operational hygiene only, not a code issue.

### Fix priority for Blue (ordered)

1. **Blue#5: land real activity dispatch over the wire** (§5.1). Replace `workflow.NewStubEnv()` in dispatch.go L71 with a worker-owned `CoroutineEnv` whose `ExecuteActivity` submits the activity to the frontend via `pollActivityTask`-initiated flow. Or — if Phase-2 replay is the final plan — fail fast at worker startup when the registered workflow calls ExecuteActivity against a stub env.
2. **Blue#5: land real timer scheduling** (§5.2). Stub timers are a billing bug.
3. **Blue#5: do not swallow workflow errors** (§5.7). One-line fix.
4. **Blue#2: ship the missing client methods** — `SignalWithStartWorkflow`, `GetWorkflow`, `QueryWorkflow`, `ExecuteChildWorkflow` (in workflow pkg), `CheckHealth` in its commerce-expected shape, `WorkflowRunGetOptions`, `WorkflowRun.GetWithOptions`.
5. **Blue#5: ship the missing worker surface** — `worker.InterruptCh()`, `worker.Options.MaxConcurrentWorkflowTaskExecutionSize`, `MaxConcurrentLocalActivityExecutionSize`, `WorkerActivitiesPerSecond`, `WorkerLocalActivitiesPerSecond`, `TaskQueueActivitiesPerSecond`, `EnableSessionWorker`.
6. **Blue#4: ship `workflow.LocalActivityOptions` + `WithLocalActivityOptions`** and `workflow.ExecuteChildWorkflow`.
7. **Foundation/Blue#2: reconcile schema with wire.** Either make the high-level RPCs emit schema-conformant frames OR restate the schema to declare the JSON-in-Bytes envelope as the v1 contract. The schema as it stands lies.
8. **Foundation/Blue#2+5: fix worker wire struct flattening** (Payloads, TaskQueue, WorkflowType, WorkflowExecution) (§3.2.1, §5.5, §5.6, §5.8).
9. **Blue#4: replace `StubEnv.Select` busy-spin** with a wait-group or condvar-based parking primitive.
10. **Blue#5: ship workflow-arg vs. history-bytes separation in the schema + transport** (§4.6).

### Re-review scope after fixes
- pkg/sdk/worker/: dispatch.go, env.go, pollers.go — especially any new `workerEnv` that replaces StubEnv.
- pkg/sdk/workflow/stub.go: Select implementation + NewTimer.
- pkg/sdk/client/: the new high-level RPC surface (QueryWorkflow, SignalWithStartWorkflow, GetWorkflow, CheckHealth, WorkflowRun.GetWithOptions).
- pkg/sdk/workflow/: ExecuteChildWorkflow + LocalActivityOptions.
- schema/tasks.zap: whatever shape you pick for the wire contract reconciliation.
- A compile check of `hanzoai/base` and `hanzoai/commerce` against the SDK post-fix — that is the true CI gate.

---
RED COMPLETE. Findings ready for Blue.
Total: 2 critical, 8 high, 7 medium, 2 low, 1 info
Top 3 for Blue to fix:
1. [CRITICAL] pkg/sdk/worker/dispatch.go uses workflow.NewStubEnv() as the runtime — every ExecuteActivity silently no-ops in production; workflows complete without side effects.
2. [CRITICAL] StubEnv.NewTimer collapses durations to zero — commerce/billing dunning/renewal schedules run back-to-back; customers billed early and spam-dunned.
3. [HIGH] 11+ symbols required by hanzoai/base and hanzoai/commerce (SignalWithStartWorkflow, GetWorkflow, QueryWorkflow, ExecuteChildWorkflow, LocalActivityOptions, worker.InterruptCh, worker.Options rate-limiting fields, CheckHealth, WorkflowRun.GetWithOptions) are missing — callers will not compile post-import-swap. CI cannot enforce `go.temporal.io/sdk = 0 hits` until the surface is complete.
Re-review needed: yes — full pkg/sdk/{worker,workflow,client} after Blue#5 commits and the critical runtime+timer fixes land. Also re-scan import purity repo-wide once service/worker/workerdeployment/ and tests/ are migrated off upstream.
Recommendation: do-not-ship. Phase-1 stub runtime is unsafe for any service that actually relies on activity execution or timer scheduling. Fix-then-ship once §5.1, §5.2, §5.7, and the §2 gap table are closed.
