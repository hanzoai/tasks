<div class="title-block" style="text-align: center;" align="center">

# Hanzo Tasks

Durable workflow execution engine for AI agent orchestration.

[![GitHub License](https://img.shields.io/github/license/hanzoai/tasks)](https://github.com/hanzoai/tasks/blob/main/LICENSE)

</div>

## Introduction

Hanzo Tasks is a durable execution engine that powers AI agent orchestration in the Hanzo ecosystem. It enables developers to build scalable, fault-tolerant workflows that automatically handle intermittent failures and retry failed operations.

Tasks provides the backbone for:
- **Playground spaces** -- each space maps to a Tasks namespace
- **Agent execution** -- each agent runs as a Tasks worker
- **Durable cron and batch jobs** -- reliable scheduled and bulk operations

MIT licensed. See [LICENSE](./LICENSE).

## Getting Started

### Build

```bash
make tasksd
```

### Run

```bash
./tasksd start
```

Or with a config file:

```bash
./tasksd --config-file config/development-sqlite.yaml --allow-no-auth start
```

### Build from source

```bash
go build ./cmd/tasksd/
./tasksd start
```

## Module

```
github.com/hanzoai/tasks
```

## Integration

Hanzo Tasks integrates with the broader Hanzo ecosystem:

- **Playground** connects to the Tasks server via the durable-execution SDK
- **Base** embeds Tasks for durable cron/batch execution
- Each playground **space** = a Tasks namespace
- Each **agent** = a Tasks worker

## Repository

This repository contains the source code of the Hanzo Tasks server. To implement Workflows, Activities, and Workers from Go, use the in-process client at [`pkg/tasks`](./pkg/tasks); a full durable-execution SDK is compatible with the server's gRPC surface.

## Architecture

See [docs/architecture/README.md](./docs/architecture/README.md) for server architecture details.

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup and guidelines.

## License

[MIT License](https://github.com/hanzoai/tasks/blob/main/LICENSE)
