// Copyright © 2026 Hanzo AI. MIT License.

package examples

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	tasks "github.com/hanzoai/tasks/pkg/tasks"
)

// EmbedResult captures the lifecycle of an in-process daemon dial.
type EmbedResult struct {
	Port           int
	HealthService  string
	HealthStatus   string
	StartedID      string
	StartedRunID   string
	ListedCount    int
}

// Embed boots tasks.Embed in-process on an ephemeral port, dials it via
// pkg/sdk/client, and round-trips Health → ExecuteWorkflow →
// ListWorkflows. This is the canonical "single binary" pattern Base
// apps use when TASKS_EMBED=true; the sample exercises the same code
// path so a regression in either the Embed boot or the SDK dial would
// trip the test suite immediately.
func Embed(ctx context.Context) (*EmbedResult, error) {
	// Pick a free port. Do it before Embed so we can pass it through.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	srv, err := tasks.Embed(ctx, tasks.EmbedConfig{
		ZAPPort:   port,
		Namespace: "default",
	})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = srv.Stop(context.Background()) }()

	c, err := client.Dial(client.Options{
		HostPort:    fmt.Sprintf("127.0.0.1:%d", port),
		Namespace:   "default",
		DialTimeout: 3 * time.Second,
		CallTimeout: 3 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer c.Close()

	res := &EmbedResult{Port: port}

	svc, status, err := c.Health(ctx)
	if err != nil {
		return nil, fmt.Errorf("health: %w", err)
	}
	res.HealthService, res.HealthStatus = svc, status

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        fmt.Sprintf("embed-%d", port),
		TaskQueue: "default",
	}, "EmbedDemo")
	if err != nil {
		return nil, fmt.Errorf("execute: %w", err)
	}
	res.StartedID, res.StartedRunID = run.GetID(), run.GetRunID()

	list, err := c.ListWorkflows(ctx, "", 10, nil)
	if err != nil {
		return nil, fmt.Errorf("list: %w", err)
	}
	for _, e := range list.Executions {
		if e.WorkflowID == res.StartedID {
			res.ListedCount++
		}
	}
	return res, nil
}
