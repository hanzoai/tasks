// Copyright © 2026 Hanzo AI. MIT License.
//
// Embed tasks into a Hanzo Base app (or any Go process) without
// running a separate tasksd binary. Same Go module, same ZAP
// opcodes, same SDK — one composable unit.
//
//	srv, err := tasks.Embed(ctx, tasks.EmbedConfig{
//	    DataDir: "/data/app",  // shared with base, or separate
//	    ZAPPort: 9999,
//	})
//	defer srv.Stop(ctx)
//
//	// Same client as external mode — transport auto-detected.
//	client := tasks.New("", fmt.Sprintf("localhost:%d", srv.ZAPPort()), nil)
//	client.Add("cleanup", "1h", cleanupFn)
//
// External vs embedded is a deployment choice. Same code, same wire.

package tasks

import (
	"context"
	"errors"
	"fmt"
	"net"
)

// EmbedConfig is the one place to configure an in-process tasks
// server. Every knob has a production default; callers only set
// what they want to change.
type EmbedConfig struct {
	// DataDir holds the task+workflow persistence. Defaults to
	// "./tasks-data". Share with Base by passing Base's data dir.
	DataDir string

	// ZAPPort is the _tasks._tcp listener. Default 9999. Set to 0
	// to pick an ephemeral port; the chosen port is available via
	// Embedded.ZAPPort() after Start.
	ZAPPort int

	// Namespace is the default namespace created on first boot.
	// Defaults to "default".
	Namespace string
}

// Embedded is the handle to a running in-process tasks server.
type Embedded struct {
	cfg      EmbedConfig
	listener net.Listener
	zapPort  int
}

// Embed starts an in-process tasks server with the given config.
// Returns an Embedded handle; caller must call Stop before
// process exit to flush the data dir cleanly.
//
// The server speaks ZAP on EmbedConfig.ZAPPort (default :9999,
// service type `_tasks._tcp`). No HTTP gateway is started —
// browsers don't talk to embedded servers. If you need the UI
// embedded too, mount Handler() at /_/tasks in your app's own
// HTTP router (gofiber or net/http both work via the fiber
// adaptor).
//
// NB: this is the v0 embed contract. The storage layer currently
// still uses the Temporal persistence path internally; task #41
// migrates it to a ZAP-native store so Embed becomes a pure
// stdlib+luxfi/zap binary with no upstream imports at all.
func Embed(ctx context.Context, cfg EmbedConfig) (*Embedded, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "./tasks-data"
	}
	if cfg.ZAPPort == 0 {
		cfg.ZAPPort = 9999
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}

	// TODO(#41): wire the in-process frontend + persistence
	// layer here. For now we expose the contract so callers can
	// depend on the API while the internals move off gRPC.
	return nil, errors.New("tasks.Embed: not yet implemented — tracked as task #41")
}

// ZAPPort returns the actual bound ZAP port (useful when the
// caller requested an ephemeral port via ZAPPort=0).
func (e *Embedded) ZAPPort() int {
	if e == nil {
		return 0
	}
	return e.zapPort
}

// Stop shuts the server down and releases the listener + data
// dir locks. Safe to call multiple times.
func (e *Embedded) Stop(ctx context.Context) error {
	if e == nil || e.listener == nil {
		return nil
	}
	if err := e.listener.Close(); err != nil {
		return fmt.Errorf("tasks.Embed: close listener: %w", err)
	}
	e.listener = nil
	_ = ctx
	return nil
}
