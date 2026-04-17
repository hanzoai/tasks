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
EXPOSE 7233 7243
ENTRYPOINT ["tasksd"]
CMD ["start", "--env", "embedded-sqlite", "--config", "/etc/tasks/config"]
