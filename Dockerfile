# syntax=docker/dockerfile:1
#
# hanzoai/tasks — native ZAP daemon. One Go binary with embedded Vite UI.
# No CGO. No SQLite driver. No protobuf. No gRPC.

FROM node:22-alpine AS ui-build
RUN corepack enable pnpm
WORKDIR /ui
COPY ui/package.json ui/pnpm-lock.yaml* ui/tsconfig.json ui/vite.config.ts ui/index.html ui/postcss.config.js ui/tailwind.config.js ./
RUN pnpm install --frozen-lockfile --prefer-offline
COPY ui/src ./src
COPY ui/public ./public
RUN pnpm build

FROM golang:1.26-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui-build /ui/dist ./ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tasksd ./cmd/tasksd

FROM alpine:3.21
RUN apk add --no-cache ca-certificates && mkdir -p /data
COPY --from=go-build /tasksd /usr/local/bin/tasksd
EXPOSE 9999 7243
ENTRYPOINT ["tasksd"]
CMD ["--zap-port", "9999", "--http", ":7243", "--data", "/data"]
