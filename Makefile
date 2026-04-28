# Hanzo Tasks — native ZAP daemon. One Go binary, one embedded SPA.

.PHONY: all build ui run clean test vet tidy release

all: build

# Sync the admin-tasks (@hanzo/gui) bundle into ui/dist for go:embed.
# Source lives in ~/work/hanzo/gui/code/admin-tasks. Set
# ADMIN_TASKS_DIR=/abs/path to override.
ui:
	scripts/sync-admin-ui.sh

# Build the tasksd binary. Pure Go, CGO disabled. ui/dist must exist
# (run `make ui` first, or rely on a previous sync).
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o tasksd ./cmd/tasksd

# Sync UI then build daemon — full local image-equivalent.
release: ui build

# Run the daemon locally on default ports (ZAP :9999, HTTP :7243).
run: build
	./tasksd

vet:
	go vet ./...

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -f tasksd
	rm -rf ui/dist tasks-data
