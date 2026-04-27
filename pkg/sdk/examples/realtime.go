// Copyright © 2026 Hanzo AI. MIT License.

package examples

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	tasks "github.com/hanzoai/tasks/pkg/tasks"
)

// RealtimeResult captures what arrived on the SSE stream during the
// demo window: every workflow.* event for the workflows we started.
type RealtimeResult struct {
	StartedCount  int
	EventsCaught  int
	StartEvents   int
	CancelEvents  int
}

// Realtime boots an embedded daemon, opens an SSE subscription via
// the native pkg/sdk/client.SubscribeEvents helper, fires a few
// state-changing operations, and tallies the events it observed
// before the context expires.
//
// This proves the realtime pipeline end-to-end: engine → broker →
// SSE → SDK channel → caller. No proxy, no gRPC, no upstream client.
func Realtime(parent context.Context) (*RealtimeResult, error) {
	// Free port for the embedded daemon's HTTP+ZAP listeners.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// pkg/tasks.Embed exposes a single ZAP listener; the HTTP shim is
	// owned by cmd/tasksd. For the smoke test we want both, so we run
	// a slim HTTP server with srv.HTTPHandler() + srv.EventsHandler().
	bootCtx, bootCancel := context.WithCancel(parent)
	defer bootCancel()
	srv, err := tasks.Embed(bootCtx, tasks.EmbedConfig{
		ZAPPort:   port,
		Namespace: "default",
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = srv.Stop(context.Background()) }()

	httpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	httpAddr := httpL.Addr().String()
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/v1/tasks/events", srv.EventsHandler())
		mux.Handle("/v1/tasks/", srv.HTTPHandler())
		_ = http.Serve(httpL, mux)
	}()

	// Subscribe BEFORE producing events so we see them all.
	subCtx, subCancel := context.WithTimeout(parent, 2*time.Second)
	defer subCancel()
	ch, err := client.SubscribeEvents(subCtx, "http://"+httpAddr)
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}

	// Dial via ZAP and fire a couple state changes.
	c, err := client.Dial(client.Options{
		HostPort:    fmt.Sprintf("127.0.0.1:%d", port),
		Namespace:   "default",
		DialTimeout: 1 * time.Second,
		CallTimeout: 1 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	defer c.Close()

	res := &RealtimeResult{}
	for i := 0; i < 3; i++ {
		run, err := c.ExecuteWorkflow(subCtx, client.StartWorkflowOptions{
			ID:        fmt.Sprintf("realtime-%d-%d", port, i),
			TaskQueue: "default",
		}, "Realtime")
		if err != nil {
			return res, fmt.Errorf("execute %d: %w", i, err)
		}
		res.StartedCount++
		// Cancel even-indexed ones so we exercise multiple event kinds.
		if i%2 == 0 {
			_ = c.CancelWorkflow(subCtx, run.GetID(), run.GetRunID())
		}
	}

	// Drain events up to the context deadline.
	for {
		select {
		case <-subCtx.Done():
			return res, nil
		case ev, ok := <-ch:
			if !ok {
				return res, nil
			}
			res.EventsCaught++
			switch ev.Kind {
			case "workflow.started":
				res.StartEvents++
			case "workflow.canceled":
				res.CancelEvents++
			}
		}
	}
}
