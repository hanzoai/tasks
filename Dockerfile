# syntax=docker/dockerfile:1
#
# hanzoai/tasks — single-binary image (Go + embedded Vite UI).
#
# Stage 1 (ui-build): pnpm + Vite build the React SPA from ui/ into
#                     ui/dist. Pure Node, no native deps.
# Stage 2 (go-build): compile tasksd with go:embed picking up ui/dist
#                     from stage 1. CGO for the sqlite driver.
# Stage 3 (runtime):  minimal alpine, just the tasksd binary + config.
#
# Final image has no Node, no pnpm, no source. One binary, one config
# dir, one data dir.

FROM node:22-alpine AS ui-build
RUN corepack enable pnpm
WORKDIR /ui
# Copy manifest first so `pnpm install` layer caches across code edits.
COPY ui/package.json ui/tsconfig.json ui/vite.config.ts ui/index.html ui/postcss.config.js ui/tailwind.config.js ./
# Lockfile is optional — if absent pnpm resolves fresh; CI should
# commit the lockfile once the scaffold stabilises.
COPY ui/pnpm-lock.yaml* ./
RUN pnpm install --prefer-offline
COPY ui/src ./src
COPY ui/public ./public
RUN pnpm build

FROM golang:1.26-alpine AS go-build
ARG GITHUB_TOKEN
WORKDIR /go/src/hanzo-tasks
RUN apk add --no-cache gcc musl-dev sqlite-dev git
ENV GOPRIVATE=github.com/luxfi/*,github.com/hanzoai/*
RUN git config --global url."https://${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Overlay the built UI bundle on top of the ui/dist placeholder so
# go:embed sees the real assets. The placeholder exists to keep
# `go build` working outside this Dockerfile (developer laptop,
# local test runs).
COPY --from=ui-build /ui/dist ./ui/dist
RUN CGO_ENABLED=1 go build -tags sqlite -o /tasksd ./cmd/tasksd/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates sqlite-libs
COPY --from=go-build /tasksd /usr/local/bin/tasksd
COPY config/ /etc/tasks/config/
RUN mkdir -p /data
EXPOSE 7233 7234 7243
ENTRYPOINT ["tasksd"]
# Default to embedded-sqlite command which auto-runs schema migrations on
# first boot. 7233 = gRPC frontend, 7234 = HTTP API + embedded UI.
# For Postgres/MySQL, override CMD to invoke `start`.
CMD ["embedded-sqlite", "--db-path", "/data/tasks.db", "--bind-ip", "0.0.0.0", "--port", "7233", "--http-port", "7234"]
