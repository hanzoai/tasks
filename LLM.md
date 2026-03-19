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
- Docker images: `hanzoai/tasks` (was `temporalio/server`)
- External deps (`go.temporal.io/sdk`, `go.temporal.io/api`) are NOT changed -- those are separate repos
- Wire-protocol client names (`temporal-server` in headers/version_checker.go, metadata keys in client/history/metadata.go) preserved to maintain backward compatibility with existing Temporal SDK clients
