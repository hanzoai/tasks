# Hanzo Tasks — native ZAP daemon. One Go binary, one Vite UI.

.PHONY: all build ui run clean test vet tidy

all: build

# Build the embedded Vite UI bundle (ui/dist) used by the go:embed in ui/embed.go.
ui:
	pnpm --prefix ui install --frozen-lockfile
	pnpm --prefix ui build

# Build the tasksd binary. Pure Go, CGO disabled. UI must already be built.
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o tasksd ./cmd/tasksd

# Build UI then daemon — full local image-equivalent.
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
