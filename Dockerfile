# syntax=docker/dockerfile:1
#
# hanzoai/tasks — native ZAP daemon. One Go binary with embedded SPA.
# No CGO. No SQLite driver. No protobuf. No gRPC.
#
# Precondition: ui/dist/ must be populated before `docker build`.
# Run scripts/sync-admin-ui.sh locally, or the CI pipeline builds
# admin-tasks (~/work/hanzo/gui/code/admin-tasks) and rsyncs the
# resulting dist/ into this build context. ui/embed.go imports the
# bundle via //go:embed all:dist at compile time.

FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
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
CMD ["--zap-port", "9999", "--http", ":7243", "--data", "/data"]
