# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
ARG GITHUB_TOKEN
WORKDIR /go/src/hanzo-tasks
RUN apk add --no-cache gcc musl-dev sqlite-dev git
ENV GOPRIVATE=github.com/luxfi/*,github.com/hanzoai/*
RUN git config --global url."https://${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -tags sqlite -o /tasksd ./cmd/tasksd/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates sqlite-libs
COPY --from=build /tasksd /usr/local/bin/tasksd
COPY config/ /etc/tasks/config/
RUN mkdir -p /data
EXPOSE 7233 7234 7243
ENTRYPOINT ["tasksd"]
# Default to embedded-sqlite command which auto-runs schema migrations on
# first boot. 7233 = gRPC frontend, 7234 = HTTP API. For Postgres/MySQL,
# override CMD to invoke `start`.
CMD ["embedded-sqlite", "--db-path", "/data/tasks.db", "--bind-ip", "0.0.0.0", "--port", "7233", "--http-port", "7234"]
