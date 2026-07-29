# Hanzo Tasks

Durable workflow execution engine for AI agent orchestration.

**Upstream**: Temporal (MIT). Pre-v3.0 history is the Temporal fork; v3.0+ is native ZAP-only with the upstream gRPC/proto/runtime fully ripped out. Public Go module pinned at v1.x forever (semver-import-versioning policy).

## Module
`github.com/hanzoai/tasks`

## Versioning policy (2026-04-30)
- **Stay on v1.x.x forever.** Per global CLAUDE.md "NEVER bump Go packages above v1.x.x."
- Major version bump in semver-import-versioning would require module-path suffix (`/v2`, `/v3`); we don't do that.
- Old v2.x and v3.x tags (transition era) were deleted on 2026-04-30. Old v1.x tags (`v1.0.0`–`v1.42.0`) are upstream-Temporal-era artifacts and remain in git history for blame.
- New Hanzo-Tasks releases continue from `v1.43.0` and bump minor for features, patch for fixes.
- `v1.51.2` added `Embedded.CancelActivityForOrg`; `v1.51.3` added `Embedded.ActivitiesPageForOrg` — the org-scoped cancel + paginated read cloud's fleet queue surface (hanzoai/cloud `clients/visor`) depends on.
- `v1.52.3` added `View.FailureStreaks(ns)` + the `FailureStreak` type — the durable "what is broken right now" read a host polls instead of hand-decoding SQLite.

## Known gaps
- **Terminal standalone-activity GC is unimplemented.** Completed/failed/canceled standalone activities under the `act/<ns>/` key family (and their `ahist/<ns>/…` history) are never pruned — the namespace `WorkflowExecutionRetentionTtl` (default `720h`) has **no sweeper enforcing it** for standalone activities. The store therefore grows unbounded with volume (one row per job, e.g. every `studio.render` in the `gpu-jobs` namespace), and every `ListActivities` / `ActivitiesForOrg` / `ActivitiesPageForOrg` scans the full set. Consumers can bound the READ (cloud's fleet queue recency-sorts + caps terminal history, and now walks all pages via `ActivitiesPageForOrg`), but the underlying storage + scan cost still grow. **Fix (queued v1.51.4):** a background sweeper alongside `runScheduler` that deletes terminal standalone activities older than the namespace retention TTL, plus their history — idempotent, org-shard-scoped. Surfaced in the hanzoai/cloud per-GPU-queue review.

## Cron sweeper observability (v1.52.2)

The sweeper reports what it cannot return to a caller. Three `slog` messages
off `EmbedConfig.Logger` (`slog.Default()` when unset), all from
`pkg/tasks/engine.go`:

| Message | Level | Meaning |
|---------|-------|---------|
| `tasks: cron schedule action failed` | ERROR | One schedule's fire did not reach a worker. Carries `org`, `namespace`, `scheduleId`, `workflowType`, `taskQueue`, `consecutiveFailures`, `error`. |
| `tasks: cron schedule action recovered` | INFO | A previously failing schedule fired and reached a worker. Carries `afterConsecutiveFailures`. |
| `tasks: schedule sweep failed, no schedule fired this tick` | ERROR | The whole sweep failed — cron is dead for every org at once. |

A fire fails in one of two ways, and both produce the same line because the
operator's question is the same: **the start itself errored**, or **the start
succeeded and no worker is subscribed to `(namespace, taskQueue)`** — the run
exists and nothing will ever execute it. The `error` field says which. The
second half catches a cron naming a namespace nobody serves (namespaces are
created on first use, so a typo is no longer a start error), a schedule whose
worker died, and a task queue renamed out from under an entry. The check is
made AFTER the start, because enqueueing prunes subscriptions whose peer the
transport can no longer reach.

Alert on the first; `consecutiveFailures` is the "how long has this been
dead" number. Per-schedule **isolation is unchanged** — a broken entry is
still skipped and retried next sweep; only the silence is gone.

Anchoring follows the start, not the report: a fire that ERRORED is not
re-anchored (it stays due and retries every tick), a fire that STARTED is
re-anchored even when it went unreached — the run exists, and re-firing on
the strength of a report would spawn one every 5s.

Throttle: first failure, then one line per `failReportThrottle` (60) sweeps.
Unthrottled a broken 5s-tick schedule is ~17k lines/day. The counter clears
on a reached fire and on `DeleteSchedule`, so a relapse alerts immediately.

Why this exists: cloud's `clients/cron` fired a JobWorkflow whose activity
re-read the entry's ConfigMap; the cloud ServiceAccount had no RBAC to read
ConfigMaps, so every fire failed for **11 days** across six nightly backups
while the engine emitted nothing and every dashboard read healthy. It was
found only by hand-reading the engine's durable records.

## Activity / workflow failure observability (v1.52.3)

The sweeper fix above covers the SCHEDULER. It does not cover the incident:
`StartWorkflow` **succeeded** all 4489 times, so the scheduler was healthy
and correctly silent — the fault was one layer down, in the ACTIVITY. That
layer's only output was `e.emit(Event{Kind:"activity.failed"})` into the SSE
broker: ephemeral, dropped when nobody is subscribed, and nobody was.

**Guarantee: durable evidence, plus throttled log lines.** Both, not either.

### Durable — `fail/<ns>/<fingerprint>`, read via `View.FailureStreaks(ns)`

One `FailureStreak` row per failing identity, upserted on every failure and
**deleted on the first success**. It needs no subscriber and survives
restart, so `ConsecutiveFailures` can say `4489` where an in-memory counter
would have said `1`. A non-empty listing IS the list of what is broken; the
keyspace is bounded by how much is broken now, not by how often it failed
(so it does not have the unbounded-growth problem noted under Known gaps).

Identity is the recurring **shape** of the work — `(workflowType,
activityType, taskQueue, scheduleId)` — never the run: every cron fire mints
a fresh runId, so a run-keyed counter can never say "dead for 11 days".
Each row carries `org`, `namespace`, `workflowType`, `activityType`,
`taskQueue`, `scheduleId`, `consecutiveFailures`, `attempt`, `retrying`,
`persistent`, `firstFailureTime`, `lastFailureTime`, `lastError`, and
`lastWorkflowId`/`lastRunId` to go read.

`ActivityType` empty ⇒ the **workflow itself** failed (a decider that faults
before scheduling anything — the hole an activity-only report would leave).

### Log lines — `pkg/tasks/failure.go`

| Message | Level | Meaning |
|---------|-------|---------|
| `tasks: activity failed` | WARN | An activity attempt failed. `retrying` says whether the engine will try again. |
| `tasks: activity failing persistently` | **ERROR** | **Alert on this.** Failing continuously for ≥ `failPersistentAfter` (15 min). |
| `tasks: activity recovered` | INFO | Streak ended. Carries `afterConsecutiveFailures`. |
| `tasks: workflow failed` / `… failing persistently` / `… recovered` | WARN / ERROR / INFO | Same three, for a workflow failing with no activity to blame. |

**WARN vs ERROR is the distinction the incident turned on**: "failed once and
will retry" is normal operation and must not page anyone; "has failed every
attempt for a long time" is the outage. Persistence is measured by **age,
not count** — a nightly backup reaches a count of only 2 in two days yet is
unambiguously broken, while one run burning its 10 default attempts (≈5.5
min of backoff) must not trip it. The persistent **transition** always
reports, whatever the throttle says; waiting for failure #60 would be the
original bug wearing a different hat.

### Throttle

`shouldReport` is the one rule for the whole package (schedules included):
first failure, then every `failReportThrottle` (60). Measured: 180 failures
→ **4 lines**; stubbing the throttle off → 180. The throttle bounds LINES
only — the durable count stays exact.

Two counters on purpose: the in-memory `failTracker` is the throttle
authority (a store that is itself failing cannot starve it into reporting
every occurrence, and a restart makes a still-broken activity re-alert once
after a deploy), while the durable row is the evidence and always supplies
the count on the line.

### Not changed

Retry and isolation **semantics are identical** — which branch is taken,
when, and how many times. `TestFailureReportingPreservesRetrySemantics`
pins the attempt count, terminal status, history events and dispatch count.
The SSE `emit` calls all remain.

Schedule-started runs now carry a `ScheduleId` search attribute (sweeper and
`TriggerSchedule`), which is what lets a failure name the dead cron entry —
and makes `ScheduleId = "nightly-backup"` a queryable filter over executions.

## Quick Start
```bash
go build ./cmd/tasksd/
./tasksd start
```

## Integration
- Playground connects to the Hanzo Tasks server via the durable-execution SDK
- Base embeds Tasks for durable cron/batch execution
- Each playground space = a Tasks namespace
- Each agent = a Tasks worker

## Rebrand Notes (2026-03-19, updated 2026-04-26)
- Upstream server packages replaced with `github.com/hanzoai/tasks` in all Go files, go.mod, go.sum
- `cmd/server` renamed to `cmd/tasksd`, binary name is `tasksd`
- Docker images: `ghcr.io/hanzoai/tasks`
- 2026-04-26: dropped compat. `temporal/`→`tasks/`, `temporaltest/`→`tasktests/`,
  `tests/` (upstream-compat suite, 3.7 MB) deleted. No backward-compat shim
  remains. `tests/testutils/` retained (cert/TLS/IO helpers used by real
  unit tests).

## Production Deployment (2026-03-19)

### Live at tasks.hanzo.ai
- **Cluster**: hanzo-k8s (do-sfo3-hanzo-k8s), namespace `hanzo`
- **Server**: `ghcr.io/hanzoai/tasks:latest` -- ZAP on 9999, HTTP on 7243.
  `--zap` takes an ADDRESS: `:9999` for TCP, or a path to bind a unix
  socket for service-to-service traffic that never leaves the host.
- **UI**: `ghcr.io/hanzoai/tasks-ui:latest` -- port 8080
- **Database**: PostgreSQL at `sql.hanzo.svc:5432` (databases: `tasks`, `tasks_visibility`)
- **Secrets**: KMS-managed via `tasks-secrets` (POSTGRES_PASSWORD, TASKS_AUTH_SECRET, IAM_CLIENT_SECRET)

### Domains
| URL | Service | Protocol |
|-----|---------|----------|
| tasks.hanzo.ai | tasks-ui (Web UI) | HTTPS |
| tasks-api.hanzo.ai | tasks (ZAP) | ZAP+TLS |

### IAM Integration
- **Provider**: hanzo.id (OIDC)
- **Client ID**: `app-tasks`
- **Callback**: `https://tasks.hanzo.ai/auth/sso/callback`
- **Scopes**: openid, profile, email
- **JWKS URI**: `https://hanzo.id/v1/iam/.well-known/jwks` (in-cluster: `http://iam.hanzo.svc/v1/iam/.well-known/jwks`)
- **Discovery**: `https://hanzo.id/.well-known/openid-configuration`
- **Registration script**: `scripts/register-iam.sh`

#### Auth Flow (two layers)
1. **UI (OIDC login)**: Tasks UI (`tasks-ui` container) handles the browser OIDC flow.
   User visits tasks.hanzo.ai, gets redirected to hanzo.id for login, callback returns
   JWT tokens. Configured via `TASKS_AUTH_*` env vars on the tasks-ui container.
2. **Server (JWT validation)**: Tasks server validates the JWT bearer token on every ZAP/HTTP
   request using JWKS keys fetched from hanzo.id. Configured via the wire-protocol env vars
   below (legacy names preserved for upstream config compatibility). These env vars feed into
   the embedded config template at `common/config/config_template_embedded.yaml` ->
   `global.authorization`.

#### Embedded Config Template Auth Env Vars (server)

> Canonical `TASKS_*` env var names. The binary reads these directly via the embedded
> config template parser.

| Env Var | Purpose | Value in K8s |
|---------|---------|-------------|
| `TASKS_JWT_KEY_SOURCE1` | JWKS URI for key fetching | `http://iam.hanzo.svc/v1/iam/.well-known/jwks` |
| `TASKS_JWT_KEY_REFRESH` | Key refresh interval | `5m` |
| `TASKS_AUTH_AUTHORIZER` | Authorizer type | `default` |
| `TASKS_AUTH_CLAIM_MAPPER` | Claim mapper type | `default` |
| `TASKS_JWT_PERMISSIONS_CLAIM` | JWT claim for permissions | `permissions` |

#### UI Auth Env Vars (tasks-ui container)
| Env Var | Purpose | Value in K8s |
|---------|---------|-------------|
| `TASKS_AUTH_ENABLED` | Enable OIDC login | `true` |
| `TASKS_AUTH_PROVIDER_URL` | OIDC issuer | `https://hanzo.id` |
| `TASKS_AUTH_CLIENT_ID` | OIDC client ID | `app-tasks` |
| `TASKS_AUTH_CLIENT_SECRET` | OIDC client secret | (from tasks-secrets) |
| `TASKS_AUTH_CALLBACK_URL` | OIDC callback | `https://tasks.hanzo.ai/auth/sso/callback` |
| `TASKS_AUTH_SCOPES` | OIDC scopes | `openid,profile,email` |

#### Namespace-to-Org Mapping
Tasks namespaces map 1:1 to Hanzo orgs. Users see only namespaces matching their IAM
org memberships. The JWT `permissions` claim carries `namespace:role` pairs (e.g.,
`hanzo:admin`, `lux:read`). The default claim mapper parses these into the internal
permission model.

### K8s Manifests
- Canonical source: `k8s/` in this repo
- Also mirrored in: `universe/infra/k8s/tasks/`
- Apply with: `kubectl apply -k k8s/`
- DB init (one-time): `kubectl apply -f k8s/init-db.yaml`

### CI/CD

One way, and it runs on our own stack:

    push  ->  github.com/hanzoai/tasks        (a mirror)
              .github/workflows/sync.yml       carries refs onward
      ->  git.hanzo.ai/hanzoai/tasks           CANONICAL
              .hanzo/workflows/cicd.yml        tests + builds ghcr.io/hanzoai/tasks
      ->  hanzoai/cloud go.mod                 pins the tag this repo cuts

**git.hanzo.ai is canonical; GitHub is a mirror.** `.github/workflows/` holds
exactly one file, `sync.yml`, and its only job is getting refs to the forge. Every
build, check and deploy is a workflow under `.hanzo/workflows/`, which the forge
reads. `.hanzo/workflows` uses GitHub Actions syntax, so a workflow moves between
the two by changing directory and nothing else.

`.hanzo/workflows/cicd.yml` is a thin trigger; all config lives in the repo-root
`hanzo.yml` — `go test -race ./pkg/... ./cmd/...` plus `go vet`, and the image
`ghcr.io/hanzoai/tasks` from the root `Dockerfile`. `hanzo.yml` declares no
`deploy:` key, so **cicd.yml never deploys**: it publishes an image and stops.

What this repo is actually load-bearing for is the **Go module tag**, which
`hanzoai/cloud` `go.mod` pins. Tags are cut by hand; CI only stamps image tags off
a tag that already exists.

The `tasks` image has no live consumer today. `tasks.hanzo.ai` is served by
cloud's embedded `clients/tasks/ui`, not by this image, and there is no App CR for
it in `hanzoai/universe`.

### Observability
- OTEL traces: `otel-collector.hanzo.svc:4318`
- Insights analytics: `insights-capture.hanzo.svc:3000`
- Dynamic config: `/etc/tasks/dynamic-config/dynamic-config.yaml` (ConfigMap)

## Native ZAP-only (2026-04-26)

The temporal fork is GONE. Zero `go.temporal.io/*`. Zero
`google.golang.org/grpc`. Zero protobuf on the wire. The whole binary is:

```
cmd/tasksd       # 100 lines: signal handling, ZAP node, HTTP server
pkg/tasks/       # in-process server (zap.Node + opcode dispatch)
pkg/sdk/         # client + worker + workflow + activity + converter
ui/              # embedded React SPA (Vite bundle)
schema/tasks.zap # canonical wire schema
```

Build proof: `go build ./cmd/tasksd` → 10 MB native binary, 208 deps.
Boot proof: `tasksd --zap :9999 --http :7243` listens on both,
serves `/healthz`, `/v1/tasks/health`, `/_/tasks/*` (UI), responds to
ZAP opcodes 0x0050–0x00A5 from `pkg/sdk/client`.

The native engine is built and GREEN (see "## Durable Engine" below for
the true current state and the Phase-2 event-sourced design). Only one
opcode still returns 501: `OpcodeStartChildWorkflow` (0x006D). Everything
else — ExecuteWorkflow, activity dispatch, signal, cancel, terminate,
query, reset, schedules, namespaces, deployments — is real and tested.

### What was deleted (2026-04-26)
- `tasks/` (renamed temporal/, the fork's runtime)
- `tasktests/` (renamed temporaltest/)
- `service/` (frontend / history / matching / worker — 817 files)
- `chasm/` (component state machine framework — 169 files)
- `client/` (legacy gRPC clients — 40 files)
- `api/` (local mirror of temporal protos — 114 files)
- `proto/`, `tools/` (codegen for temporal protos)
- `cmd/tools/` (genrpcwrappers, genrpcserverinterceptors, getproto, etc.)
- `components/` (nexusoperations, callbacks, dummy state machines)
- `docker/` (pre-native server containerization)
- `schema/{cassandra,elasticsearch,mysql,postgresql,sqlite}/` (DB driver
  schemas — embedded SQLite returns via pkg/tasks when persistence lands)
- 40 tainted `common/` subdirs + 7 top-level common files
  (`util.go`, `rpc.go`, `rpc_mock.go`, `client_cache.go`, `daemon.go`,
  `constants.go`)
- `tests/` (upstream-compat suite, 3.7 MB)

### What remains in `common/`
Pure stdlib utilities that survive the rip: `aggregate`, `auth`, `build`,
`channel`, `circuitbreaker`, `clock`, `collection`, `contextutil`,
`convert`, `debug`, `definition`, `effect`, `finalizer`, `future`, `goro`,
`health`, `masker`, `number`, `pingable`, `pprof`, `predicates`, `quotas`,
`resolver`, `routing`, `schedules`, `shuffle`, `stream_batcher`, `tasks`,
`tasktoken`, `timer`, `util`, `versioninfo`. These are candidates for
further pruning once the native engine settles.

### Auth: IAM only (v3.5.0+, 2026-04-29)

tasksd validates `Authorization: Bearer <jwt>` directly against IAM
JWKS — no gateway dependency required. The strip+mint pattern is the
trust boundary:

- Inbound `X-Org-Id` / `X-User-Id` / `X-User-Email` are unconditionally
  deleted on every request (`auth.stripIdentityHeaders`).
- If a Bearer JWT is present, the token is parsed (RS256/ES256/...) and
  verified against keys fetched from `TASKSD_JWKS_URL`. Issuer and
  audience are checked against `TASKSD_JWT_ISSUER` / `TASKSD_JWT_AUDIENCE`.
- On success, `X-Org-Id` is minted from the `owner` claim, `X-User-Id`
  from `sub`, `X-User-Email` from `email`. Per-org store scoping uses
  `engine.As(Org(auth.OrgID(ctx)))`. The frontend scopes to the ORG; a
  host that wants project- or user-level isolation asks for it explicitly
  via `Embedded.View(Principal{Org, Project, User})`.
- `TASKSD_REQUIRE_IDENTITY=true` (production) rejects requests without
  a validated JWT. Default false keeps embedded/dev path functional.

Production env (do-sfo3-hanzo-k8s/hanzo):
```
TASKSD_JWKS_URL=http://iam.hanzo.svc/v1/iam/.well-known/jwks
TASKSD_JWT_ISSUER=https://hanzo.id
TASKSD_REQUIRE_IDENTITY=true
```

History — pre-v3.5.0, the middleware trusted client-supplied X-* headers
under the assumption that hanzoai/gateway sat between ingress and tasksd.
The actual ingress topology routed direct to the tasks Service, leaving
`X-Org-Id` spoofable on `tasks-api.hanzo.ai`. v3.5.0 makes tasksd
self-sufficient: gateway can still front it for rate limiting / billing,
but it is no longer the sole trust boundary.

## Durable Engine — true state + Phase-2 event-sourcing design

> Authoritative architecture note (CTO). Supersedes the stale "handlers
> return a 501" text above. Read this before touching `pkg/tasks` or the
> workflow runtime in `pkg/sdk`.

### Where the engine actually is (corrected)

The native engine is NOT a shell of 501 stubs. It is **Phase-1-complete
and green** (`go test -race ./pkg/tasks/... ./pkg/sdk/...` passes). Built
and tested today:

- `ExecuteWorkflow` — `engine.startWorkflowWithRequestID`: persists a
  `WorkflowExecution` (SQLite), writes `WORKFLOW_EXECUTION_STARTED` to an
  append-only history, mints a run id, enqueues a workflow task to a
  subscribed worker. Idempotent on `(ns, workflowId, requestID)` via the
  `idem/` keyspace.
- Append-only history — `engine.appendHistory` + `GetWorkflowHistory`
  (paginated, reversible). `HistoryEvent{EventId, EventTime, EventType,
  Attributes}`, `EventId` monotonic per `(ns, wf, run)`.
- Signal / Cancel (two-phase CANCEL_REQUESTED→CANCELED handshake with a
  sweep) / Terminate / Query (server pushes the query to a worker and
  awaits its response) / Reset (truncate history at an eventId).
- Standalone activities (`activities.go`): first-class activity records
  with `ACTIVITY_TASK_SCHEDULED/STARTED/COMPLETED/FAILED` history.
- Worker dispatch (`dispatch.go`): workers `Subscribe` per (ns, queue,
  kind); server pushes tasks (no polling); HMAC task tokens; round-robin;
  pending queues drain on subscribe; activity results pushed back to the
  workflow's peer.
- Schedules (cron + interval), namespaces, deployments (worker
  versioning), nexus endpoints, identities, visibility query parser,
  per-tenant store scoping, SSE event stream, replication scaffolding
  (`replication/` quasar + local), migration tool.

Persistence = per-`(principal, namespace)` SQLite shard
(`pkg/tasks/store`), WAL, single-writer, `kv` + `history` + `idem` +
`meta` tables. Survives restart (`TestStore_PersistsAcrossOpen`).

A `Principal` is the tenant that owns a shard — an org, optionally
narrowed to a project and a user. It is one value used in the two places
tenancy is decided, so they cannot disagree: the shard's directory
(`<data>/<org>/<project>/<user>/<ns>.db`, each unset leg written `_`) and
the key its DEK is wrapped under. Shards and namespaces are created on
first use; nothing is declared up front.

`EmbedConfig.MasterKey` (32 bytes, `KMS_MASTER_KEY` base64 for `tasksd`)
opens every shard encrypted at rest. Each file carries its own DEK,
wrapped under a KEK derived master -> org -> project -> user and stored
beside it as `<ns>.db.dek`, so rotating the master rewraps sidecars and
never touches ciphertext. A keyed shard is ciphertext on every build:
`hanzoai/sqlite` uses the live libsqlcipher codec when one is linked and
its pure-Go SQLCipher envelope when one is not. On the envelope the file
is sealed at checkpoint/close rather than per commit, which is what the
manager's sweep bounds. Unset leaves shards plaintext (dev).

### What is genuinely missing (the real gap)

The whole system is explicitly **Phase 1: coroutine-push, not replay**
(see `pkg/sdk/worker/worker.go:19`, `workflow/version.go:31`,
`workflow/local_activity.go:62`). A workflow runs as ONE live worker
goroutine that blocks on activity results pushed back over the wire.
Consequences:

1. **No deterministic replay.** Workflow-driven activities (worker
   `OpcodeScheduleActivity` 0x006B) write NOTHING to the *workflow's*
   history — only START/terminal/signal/cancel land. The history cannot
   reconstruct a run, so it cannot be replayed.
2. **No crash-recovery of workflows.** If the worker (or tasksd) dies
   mid-run, the live goroutine is lost; nothing replays history to
   resume. The dispatcher's pending/inflight state is in-memory only.
3. **No timers** (workflow `Sleep`/`NewTimer` has no durable server
   side), **no activity retry/backoff** (RetryPolicy is stored, never
   enforced), **no child workflows** (`OpcodeStartChildWorkflow` → 501).

### Target architecture — event-sourced durable execution

One model, the Temporal core done minimally. **History is the only source
of truth. A workflow task is one decision episode. The worker is
stateless between episodes — it rebuilds state by replaying history.**

The loop:

1. `ExecuteWorkflow` persists the run + `WORKFLOW_EXECUTION_STARTED`,
   marks a **workflow task pending**, enqueues it.
2. Server delivers the workflow task **carrying the full history**
   (`WorkflowTask.History` — the wire slot already exists; today it
   smuggles raw input, `dispatch.go:431` "Phase 2 will parse an
   event-sourced history").
3. Worker **replays** from event 0, re-running the registered function.
   Each workflow primitive consults history by a **deterministic command
   sequence number** (`seq`, incremented per command in program order):
   - `ExecuteActivity` at `seq=k`: if `ActivityTaskCompleted{seq=k}` is
     in history → return its recorded result (NO re-dispatch); if
     `ActivityTaskFailed{seq=k}` (retries exhausted) → return the failure;
     else emit `ScheduleActivity{seq=k}` and the future stays unresolved.
   - `NewTimer`/`Sleep` at `seq=k`: `TimerFired{seq=k}` → resolved; else
     emit `StartTimer{seq=k, fireAt}`.
   - Signals: `WorkflowExecutionSignaled` events replay into the signal
     channels in history order.
4. When the function can make no more progress (all pending futures
   unresolved) the episode ends; the worker returns the batch of NEW
   commands via `RespondWorkflowTaskCompleted` (the `commandsEnvelope`
   already models `ScheduleActivity`; add `StartTimer`, `StartChild`,
   plus the existing `CompleteWorkflow`/`FailWorkflow`).
5. Server **applies commands** — for each: append the corresponding
   `*_SCHEDULED`/`*_STARTED` history event and act (dispatch activity,
   arm timer, start child, terminal-transition). Command application is
   idempotent on `(run, seq)`.
6. When an activity completes/fails, a timer fires, a signal or cancel
   arrives, or a child completes → append the result event
   (`ActivityTaskCompleted{seq}`, `TimerFired{seq}`, …) and **schedule a
   new workflow task**. Back to step 2.

**Determinism invariant**: same history ⇒ same command sequence. Enforced
by (a) the workflow function being pure w.r.t. inputs + history,
(b) every non-deterministic input (activity result, timer fire, time,
version marker, side-effect) sourced from a history event keyed by `seq`,
(c) the decider dropping commands whose `seq` already has a
`*_SCHEDULED`/`*_STARTED` event (no double-schedule on replay).

**Exactly-once activity dispatch**: an activity is identified by
`(ns, run, seq)`. Scheduling is idempotent on that key — replay or a
recovery re-dispatch of the same `seq` returns the same activity record
and never runs the user activity twice once it is terminal. Server-side
retry: on `ActivityTaskFailed`, if `attempt < RetryPolicy.MaximumAttempts`
and the error is retryable, re-dispatch after `min(MaximumInterval,
InitialInterval · BackoffCoefficient^(attempt-1))`, recording each attempt;
on exhaustion write the terminal `ActivityTaskFailed{seq}` and schedule a
workflow task.

**Crash-recovery (single replica)**: everything above is in SQLite. On
tasksd boot, a recovery pass scans RUNNING workflows and rebuilds the
in-memory dispatcher from history: any workflow whose latest event needs
a decision (started, or a result landed with no following
`WorkflowTaskCompleted`) → re-enqueue a workflow task; any activity
`Scheduled`/`Started` with no terminal event → re-dispatch (idempotent per
`(run, seq)`); any `TimerStarted` with no `TimerFired` → re-arm to its
`fireAt`. Because dispatch is `seq`-keyed and the decider dedups on
replay, recovery causes no lost and no double execution.

**Multi-replica (later)**: the SQLite store already replicates via
`replication/` (quasar). Add a **per-(ns, run) dispatch lease** in the
replicated `meta` table so exactly one replica owns advancing a given
run at a time (mirror the exactly-once lease pattern used elsewhere in
the fleet). Until then tasksd runs single-writer (replicas=1).

### History event vocabulary (add to the existing family)

`WORKFLOW_TASK_SCHEDULED/STARTED/COMPLETED/FAILED`,
`ACTIVITY_TASK_SCHEDULED/STARTED/COMPLETED/FAILED/TIMED_OUT`,
`TIMER_STARTED/FIRED/CANCELED`,
`CHILD_WORKFLOW_EXECUTION_STARTED/COMPLETED/FAILED`. Each carries its
`seq` in `Attributes`. These are JSON-native, owned by Hanzo, and
persisted in the `history` table alongside the existing `kv` records.

### Phasing (what ships when)

- **Phase 2a (first coherent slice, this lane):** durable activity core.
  Workflow-driven activity lifecycle written to the workflow's history;
  exactly-once dispatch keyed by `(run, seq)`; server-side activity
  retry+backoff per RetryPolicy; crash-recovery re-dispatch of in-flight
  activities; history-backed `ExecuteActivity` replay (recorded result
  returned on re-run) + workflow-task-driven advance. Tests (`-race`):
  determinism/replay, retry+backoff timing, exactly-once dispatch,
  crash-recovery (kill mid-activity → resume, no double-run).
- **Phase 2b:** durable timers (`StartTimer`/`TimerFired` + timer wheel),
  signals/cancel folded into replay, child workflows (retire the 0x006D
  501), query consistency against replayed state.
- **Phase 2c:** multi-replica dispatch lease over the replicated store.

### Scope discipline (agent orchestration, not Temporal parity)

Workflows = agent-session / subagent trees. Activities = agent runs +
tool calls. Signals = control commands (pause/resume/message). Timers =
schedules/retries. Child workflows = subagents. Skip search attributes v2,
advanced visibility, multi-namespace federation, Nexus cross-cluster —
unless trivial. Once `ExecuteWorkflow + child + signal + cancel` are real,
task #31 (agent sessions) wires the live TaskController in one line and
#32 (converge agent-scheduler + vm-metering tickers onto durable cron)
unblocks.

### KNOWN HAZARD — origin/main regression (coordination)

`origin/main` was force-pushed on 2026-07-02 to re-merge ~494 upstream
Temporal commits (`merge: upstream/main`, `pin go.temporal.io/server
v1.32.0-157.0`), re-adding `proto/`, `config/`, `develop/`, `docker/` and
the `go.temporal.io/*` deps — while the LLM.md still claims "runtime fully
ripped out." The native engine (`pkg/tasks`) is temporal-free and
identical across the divergence; the re-add is dependency/tooling baggage
the engine does not use. This contradicts the "temporal fork is GONE"
doctrine and must be resolved by a deliberate decision (re-rip vs. keep as
fork base) — do not let it silently become the base. `feat/tasks-durable-
engine` branches off this commit and keeps all new work inside the
temporal-free `pkg/tasks` + `pkg/sdk/workflow` zones so it rebases clean
whichever way that decision lands.
