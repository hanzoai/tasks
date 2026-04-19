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

## Rebrand Notes (2026-03-19)
- Upstream server packages replaced with `github.com/hanzoai/tasks` in all Go files, go.mod, go.sum
- `cmd/server` renamed to `cmd/tasksd`, binary name is `tasksd`
- Docker images: `ghcr.io/hanzoai/tasks` (was the upstream image name)
- External SDK/API deps are NOT changed — those are separate repos
- Wire-protocol client names preserved to maintain backward compatibility with existing SDK clients

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
