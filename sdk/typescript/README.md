# @hanzoai/tasks — TypeScript SDK

Durable workflow execution for [Hanzo Tasks](https://github.com/hanzoai/tasks)
over the native **ZAP** transport. Zero gRPC, zero `go.temporal.io`. Built as a
drop-in for `@temporalio/*` + `nestjs-temporal-core`, so a NestJS/Postiz app
(e.g. `social-orchestrator`) migrates off upstream Temporal as a dependency
swap rather than a rewrite.

The wire is byte-compatible with `pkg/sdk` (the Go SDK) and `luxfi/zap`
v0.2.0 — a `tasksd` server cannot tell a Go client from a TypeScript one.

## Install

```bash
npm add @hanzoai/tasks
```

## Client

```ts
import { TasksClient } from "@hanzoai/tasks";

const client = await TasksClient.connect({ address: "tasks:9999", namespace: "default" });

const handle = await client.startWorkflow("PublishPost", {
  taskQueue: "social",
  workflowId: "post-123",
  args: [{ postId: "123" }],
  memo: { org: "acme" },
});

await handle.signal("approve", { by: "editor" });
const state = await handle.query("getState");
await handle.result(); // blocks until terminal
await client.close();
```

`@temporalio/client`-shaped shim (`Connection` + `Client.workflow.*`):

```ts
import { Connection, Client } from "@hanzoai/tasks";

const connection = await Connection.connect({ address: "tasks:9999" });
const client = await Client.create({ connection, namespace: "default" });
await client.workflow.start("PublishPost", { taskQueue: "social", workflowId: "post-123", args: [{ postId: "123" }] });
```

## Worker

```ts
import { Worker } from "@hanzoai/tasks";
import * as activities from "./activities";

const worker = await Worker.create({
  address: "tasks:9999",
  namespace: "default",
  taskQueue: "social",
  activities,
  workflows: { PublishPost, ScheduleThread },
});
await worker.run(); // blocks until shutdown()
```

## Workflow primitives

Import inside workflow functions — the API mirrors `@temporalio/workflow`:

```ts
import {
  proxyActivities, sleep, condition, defineSignal, defineQuery,
  setHandler, workflowInfo, getVersion,
} from "@hanzoai/tasks";

const { publish } = proxyActivities<typeof activities>({ startToCloseTimeout: "1 minute" });

const approve = defineSignal<[{ by: string }]>("approve");

export async function PublishPost(input: { postId: string }) {
  let approved = false;
  setHandler(approve, () => { approved = true; });
  await condition(() => approved);
  await sleep("5s");
  return publish(input.postId);
}
```

### Execution model

Workflows are **event-sourced replay deciders** (identical to the Go SDK and
Temporal): on each workflow task the server delivers the run's full history and
the worker replays the function from the top. Durable primitives are assigned a
deterministic per-run sequence number in program order; if the outcome is
already in history it resolves, otherwise a command is emitted and the async
function suspends until the next task. **Workflow code must be deterministic** —
no `Date.now()`, `Math.random()`, `setTimeout`, or direct I/O; use activities
and the primitives above.

## Errors

```ts
import { ApplicationFailure, TaskError } from "@hanzoai/tasks";
throw ApplicationFailure.nonRetryable("bad input", "ValidationError");
```

Failures serialise to the exact `{ v, p }` envelope of `pkg/sdk/temporal`.

## Status — complete vs remaining

Wired and tested against the real ZAP transport (Go engine ≥ v1.49.0):

| Primitive | Status |
|---|---|
| Client: start / signal / signalWithStart / query / describe / cancel / terminate / list | ✅ complete |
| Client: namespace register/describe/list, health | ✅ complete |
| Client: `searchAttributes` + `workflowIdConflictPolicy` on start / signalWithStart | ✅ complete |
| Worker: subscribe, activity dispatch + heartbeat, respond | ✅ complete |
| Workflow decider: `proxyActivities` (durable activities) | ✅ complete |
| Durable `sleep` / `startTimer` (activity-backed) | ✅ complete¹ |
| `condition` (parks until predicate holds) | ✅ complete |
| `defineSignal` / `setHandler` — signal delivered to a running workflow | ✅ complete |
| `continueAsNew` — closes the run, server starts the successor | ✅ complete |
| `startChild` / `executeChild` — detached (ABANDON) child workflow | ✅ complete |
| Typed search attributes (`TypedSearchAttributes`, `defineSearchAttributeKey`) — stored + visibility-queryable | ✅ complete |
| Retry policy, activity failure propagation (`ActivityFailure`→`ApplicationFailure`), cancellation | ✅ complete |
| `@temporalio/*` shim: `Connection`, `Client`, `NativeConnection`, `Worker`, `ApplicationFailure`, `ActivityFailure` | ✅ complete |

Remaining:

- **Durable timers as a native command.** `sleep`/`startTimer` are backed by an
  internal `__hanzo_timer__` sleeper activity — durable and deterministic, but a
  worker crash mid-sleep re-runs the delay from zero (fires late, never lost).
  A native `TIMER_STARTED`/`TIMER_FIRED` server command removes the caveat. ¹
- **Child result await.** `startChild`/`executeChild` are ABANDON (detached):
  they resolve on the child's *start*, not its completion — the parent does not
  block on the child's result. This matches how social uses repeat-post children.
- **`WorkflowHandle.result()` value** — resolves `undefined` on completion
  because `DescribeWorkflowResponse` carries no result field on the v1 wire
  (same gap as the Go SDK). It throws on failure/cancel/terminate.
- **Worker-side query handlers** — `defineQuery`/`setHandler(query)` register,
  but answering a delivered query requires history in the query delivery
  (server gap); the client-side `query()` RPC works.

## License

MIT.
