# Hanzo Tasks

Durable workflow execution engine for AI agent orchestration. Fork of [Temporal.io](https://github.com/temporalio/temporal) (MIT license).

## Module
`github.com/hanzoai/tasks`

## Quick Start
```bash
go build ./cmd/tasksd/
./tasksd start
```

## Integration
- Playground imports `go.temporal.io/sdk` and connects to Hanzo Tasks server
- Base embeds Tasks for durable cron/batch execution
- Each playground space = a Tasks namespace
- Each agent = a Tasks worker

## Rebrand Notes (2026-03-19)
- `go.temporal.io/server` replaced with `github.com/hanzoai/tasks` in all Go files, go.mod, go.sum
- `cmd/server` renamed to `cmd/tasksd`, binary name is `tasksd`
- Docker images: `ghcr.io/hanzoai/tasks` (was `temporalio/server`)
- External deps (`go.temporal.io/sdk`, `go.temporal.io/api`) are NOT changed -- those are separate repos
- Wire-protocol client names (`temporal-server` in headers/version_checker.go, metadata keys in client/history/metadata.go) preserved to maintain backward compatibility with existing Temporal SDK clients

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
- Both server and UI authenticate via hanzo.id
- Server uses IAM_INTERNAL_URL (`http://iam.hanzo.svc`) for in-cluster token validation

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
