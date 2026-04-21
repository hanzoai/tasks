package embedded_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/hanzoai/tasks/temporaltest/embedded"
)

// startEcho registers an echo workflow + activity and exercises the full
// round-trip against an embedded SQLite server. This is the canonical
// smoke test external consumers can reference.
func TestEmbedded_SQLite_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "durable.db")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv, err := embedded.Start(ctx, embedded.Config{
		Namespace: "embedded-test",
		Backend:   embedded.BackendSQLite,
		Path:      dbPath,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	c, err := srv.NewClient(ctx)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	w := worker.New(c, "embedded-test", worker.Options{})
	w.RegisterWorkflowWithOptions(echoWorkflow, workflow.RegisterOptions{Name: "EchoWorkflow"})
	w.RegisterActivityWithOptions(echoActivity, activity.RegisterOptions{Name: "echo"})
	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	t.Cleanup(w.Stop)

	run, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "EchoWorkflow-1",
		TaskQueue: "embedded-test",
	}, "EchoWorkflow", "hello")
	if err != nil {
		t.Fatalf("ExecuteWorkflow: %v", err)
	}
	var got string
	if err := run.Get(ctx, &got); err != nil {
		t.Fatalf("run.Get: %v", err)
	}
	if got != "hello" {
		t.Fatalf("expected hello got %q", got)
	}
}

func TestEmbedded_Validation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  embedded.Config
		want string
	}{
		{"missing namespace", embedded.Config{Backend: embedded.BackendSQLite, Path: "/tmp/x.db"}, "Namespace is required"},
		{"missing backend", embedded.Config{Namespace: "x", Path: "/tmp/x.db"}, "Backend is required"},
		{"unsupported postgres", embedded.Config{Namespace: "x", Backend: embedded.BackendPostgres}, "not yet implemented"},
		{"missing path", embedded.Config{Namespace: "x", Backend: embedded.BackendSQLite}, "Path is required"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := embedded.Start(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !contains(err.Error(), tc.want) {
				t.Fatalf("err %q missing substring %q", err, tc.want)
			}
		})
	}
}

func echoWorkflow(ctx workflow.Context, in string) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second,
	})
	var out string
	if err := workflow.ExecuteActivity(ctx, "echo", in).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

func echoActivity(_ context.Context, in string) (string, error) {
	return in, nil
}

func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
