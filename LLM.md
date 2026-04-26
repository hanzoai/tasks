# Hanzo Tasks

Durable workflow execution engine for AI agent orchestration.

## Module
`github.com/hanzoai/tasks`

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
- **Server**: `ghcr.io/hanzoai/tasks:latest` -- gRPC on 7233, HTTP on 7234
- **UI**: `ghcr.io/hanzoai/tasks-ui:latest` -- port 8080
- **Database**: PostgreSQL at `sql.hanzo.svc:5432` (databases: `tasks`, `tasks_visibility`)
- **Secrets**: KMS-managed via `tasks-secrets` (POSTGRES_PASSWORD, TASKS_AUTH_SECRET, IAM_CLIENT_SECRET)

### Domains
| URL | Service | Protocol |
|-----|---------|----------|
| tasks.hanzo.ai | tasks-ui (Web UI) | HTTPS |
| tasks-api.hanzo.ai | tasks (gRPC) | gRPC+TLS |

### IAM Integration
- **Provider**: hanzo.id (OIDC)
- **Client ID**: `app-tasks`
- **Callback**: `https://tasks.hanzo.ai/auth/sso/callback`
- **Scopes**: openid, profile, email
- **JWKS URI**: `https://hanzo.id/.well-known/jwks` (in-cluster: `http://iam.hanzo.svc/.well-known/jwks`)
- **Discovery**: `https://hanzo.id/.well-known/openid-configuration`
- **Registration script**: `scripts/register-iam.sh`

#### Auth Flow (two layers)
1. **UI (OIDC login)**: Tasks UI (`tasks-ui` container) handles the browser OIDC flow.
   User visits tasks.hanzo.ai, gets redirected to hanzo.id for login, callback returns
   JWT tokens. Configured via `TASKS_AUTH_*` env vars on the tasks-ui container.
2. **Server (JWT validation)**: Tasks server validates the JWT bearer token on every gRPC/HTTP
   request using JWKS keys fetched from hanzo.id. Configured via the wire-protocol env vars
   below (legacy names preserved for upstream config compatibility). These env vars feed into
   the embedded config template at `common/config/config_template_embedded.yaml` ->
   `global.authorization`.

#### Embedded Config Template Auth Env Vars (server)

> Canonical `TASKS_*` env var names. The binary reads these directly via the embedded
> config template parser.

| Env Var | Purpose | Value in K8s |
|---------|---------|-------------|
| `TASKS_JWT_KEY_SOURCE1` | JWKS URI for key fetching | `http://iam.hanzo.svc/.well-known/jwks` |
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
- GitHub Actions: `.github/workflows/build-and-publish.yml`
- Pushes to `ghcr.io/hanzoai/tasks:{sha,branch,latest}` on main/release branches
- Uses docker-bake.hcl for multi-arch builds (linux/amd64, linux/arm64)

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
Boot proof: `tasksd --zap-port 9999 --http :7243` listens on both,
serves `/healthz`, `/v1/tasks/health`, `/_/tasks/*` (UI), responds to
ZAP opcodes 0x0050–0x00A5 from `pkg/sdk/client`.

Workflow execution semantics are NOT yet wired in the native server —
handlers return a 501 envelope (`opcode 0x00XX: not yet implemented in
native server`). The native engine is the next build phase. The shape
is in place so callers depend on the API while the engine lands behind
it.

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

### Auth: IAM only
JWT validation against hanzo.id (`https://hanzo.id/.well-known/jwks`).
Identity headers (`X-User-Id`, `X-Org-Id`, `X-User-Email`) are populated
by `hanzoai/gateway` after JWT verify; tasksd trusts them as the canonical
caller identity (vendor-free X-* convention per global CLAUDE.md).
