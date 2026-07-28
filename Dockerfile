# syntax=docker/dockerfile:1
#
# hanzoai/tasks — native ZAP daemon. One Go binary with embedded SPA.
# No CGO (pure-Go SQLite store via hanzoai/sqlite). No protobuf. No gRPC.
#
# Precondition: ui/dist/ must be populated before `docker build`.
# Run scripts/sync-admin-ui.sh locally, or the CI pipeline builds
# admin-tasks (~/work/hanzo/gui/code/admin-tasks) and rsyncs the
# resulting dist/ into this build context. ui/embed.go imports the
# bundle via //go:embed all:dist at compile time.

FROM golang:1.26.5-alpine AS go-build
# git: the private-module fetch below resolves `direct` via git.
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
# Private Go modules (github.com/hanzoai/sqlite) need git auth. CI passes a
# buildkit secret; git presents the token for github.com. The token rides a
# --mount secret, so it never lands in an image layer. No token → public-only
# build still works. Secret names: the canonical hanzoai/ci lane provides
# `gh_token`; the in-cluster Kaniko path uses `GIT_AUTH_TOKEN` — accept either
# (same dual-mount pattern as hanzoai/iam's Dockerfile). Public modules keep
# the default proxy+sumdb (immutable hashes; retag-proof).
RUN --mount=type=secret,id=gh_token --mount=type=secret,id=GIT_AUTH_TOKEN \
    sh -c 'set -e; \
      TOK=""; \
      if [ -s /run/secrets/gh_token ]; then TOK="$(cat /run/secrets/gh_token)"; \
      elif [ -s /run/secrets/GIT_AUTH_TOKEN ]; then TOK="$(cat /run/secrets/GIT_AUTH_TOKEN)"; fi; \
      if [ -n "$TOK" ]; then \
        export GIT_CONFIG_GLOBAL=/tmp/gitconfig; \
        git config --global url."https://x-access-token:${TOK}@github.com/".insteadOf "https://github.com/"; \
      fi; \
      GOPRIVATE=github.com/hanzoai/sqlite go mod download; \
      rm -f /tmp/gitconfig'
COPY . .
RUN test -f ui/dist/index.html || (echo "ui/dist missing — run scripts/sync-admin-ui.sh before docker build" >&2 && exit 1)

# Per SCALE_STANDARD.md §2 — every Go production Dockerfile that
# emits JSON to a client builds with GOEXPERIMENT=jsonv2. Verified
# -12% time / -23% allocs on the edge POST roundtrip vs encoding/json
# v1 (json_bench_test.go in hanzoai/zip).
ARG GO_EXPERIMENT=jsonv2
ENV GOEXPERIMENT=${GO_EXPERIMENT}

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tasksd ./cmd/tasksd

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && mkdir -p /data
COPY --from=go-build /tasksd /usr/local/bin/tasksd
EXPOSE 9999 7243
ENTRYPOINT ["tasksd"]
CMD ["--zap", ":9999", "--http", ":7243", "--data", "/data"]
