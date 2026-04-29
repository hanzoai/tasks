// Copyright © 2026 Hanzo AI. MIT License.

package tasks_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/hanzoai/tasks/pkg/sdk/worker"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
	"github.com/hanzoai/tasks/pkg/tasks"
	luxlog "github.com/luxfi/log"
)

// freePort picks an OS-assigned port and closes the listener, returning
// the port number. There is a race between close and ZAP bind; for a
// test that's acceptable.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

var e2eActivityCalls int32

func E2EGreet(ctx context.Context, who string) (string, error) {
	atomic.AddInt32(&e2eActivityCalls, 1)
	return "hello, " + who, nil
}

func E2EHello(ctx workflow.Context, name string) (string, error) {
	ao := workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Second}
	ctx = workflow.WithActivityOptions(ctx, ao)
	var out string
	if err := workflow.ExecuteActivity(ctx, E2EGreet, name).Get(ctx, &out); err != nil {
		return "", err
	}
	return out, nil
}

// TestE2E_WorkflowExecutesActivityAndCompletes proves the native ZAP
// engine actually runs workflows: client starts a workflow, server
// pushes the task to a registered worker, the workflow body runs and
// schedules an activity, server pushes the activity, the activity
// returns a result, the result is pushed back to the workflow, the
// workflow returns, and DescribeWorkflow reports COMPLETED.
//
// Anything weaker than this (control-plane round-trips, fake
// workflows) is not what shipped — this is the end-to-end path that
// drives the wind-down workflow tomorrow.
func TestE2E_WorkflowExecutesActivityAndCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	port := freePort(t)
	emb, err := tasks.Embed(ctx, tasks.EmbedConfig{ZAPPort: port})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(ctx)

	cli, err := client.Dial(client.Options{
		HostPort:    fmt.Sprintf("127.0.0.1:%d", port),
		Namespace:   "default",
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	atomic.StoreInt32(&e2eActivityCalls, 0)

	w := worker.New(cli, "default", worker.Options{
		MaxConcurrentWorkflowTaskPollers:       1,
		MaxConcurrentActivityExecutionSize:     1,
		MaxConcurrentWorkflowTaskExecutionSize: 1,
		Logger:                                 luxlog.New(),
	})
	w.RegisterWorkflowWithOptions(E2EHello, worker.RegisterWorkflowOptions{Name: "Hello"})
	w.RegisterActivity(E2EGreet)

	if err := w.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer w.Stop()

	// Give Subscribe round-trips a moment to land. Without this the
	// EnqueueWorkflowTask call below races the Subscribe; the
	// dispatcher correctly queues, but the test wants delivery to be
	// observed on the wire path.
	time.Sleep(100 * time.Millisecond)

	run, err := cli.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "e2e-hello-" + time.Now().UTC().Format("150405.000000"),
		TaskQueue: "default",
	}, "Hello", "world")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}

	// Poll until terminal — the wire is async; the worker dispatches
	// the workflow task on a goroutine and the activity completion
	// flows back through the dispatcher push path.
	deadline := time.Now().Add(15 * time.Second)
	var lastStatus client.WorkflowStatus
	for time.Now().Before(deadline) {
		info, err := cli.DescribeWorkflow(ctx, run.GetID(), run.GetRunID())
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		lastStatus = info.Status
		if info.Status == client.WorkflowStatusCompleted {
			break
		}
		if info.Status == client.WorkflowStatusFailed ||
			info.Status == client.WorkflowStatusTimedOut ||
			info.Status == client.WorkflowStatusTerminated {
			t.Fatalf("workflow ended in non-success terminal state: %d (info=%+v)", info.Status, info)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if lastStatus != client.WorkflowStatusCompleted {
		t.Fatalf("workflow did not complete; last status=%d", lastStatus)
	}
	if got := atomic.LoadInt32(&e2eActivityCalls); got != 1 {
		t.Fatalf("expected exactly 1 activity invocation, got %d", got)
	}
}

// TestE2E_HTTPLifecycle exercises the /v1/tasks HTTP surface end-to-end:
// Start → Signal → History → Query → Terminate. Drives the same path
// the React UI does, with no worker registered (so the workflow stays
// in RUNNING until terminated).
func TestE2E_HTTPLifecycle(t *testing.T) {
	emb, err := tasks.Embed(context.Background(), tasks.EmbedConfig{ZAPPort: 0})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(context.Background())
	srv := httptest.NewServer(emb.HTTPHandler())
	defer srv.Close()

	post := func(path, body string) (int, []byte) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	get := func(path string) (int, []byte) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}

	// Start.
	code, body := post("/v1/tasks/namespaces/default/workflows",
		`{"workflowId":"e2e-1","workflowType":{"name":"Demo"},"taskQueue":{"name":"default"}}`)
	if code != 200 {
		t.Fatalf("start: %d %s", code, body)
	}
	var started struct {
		Execution struct {
			RunId string `json:"runId"`
		} `json:"execution"`
	}
	_ = json.Unmarshal(body, &started)
	runID := started.Execution.RunId
	if runID == "" {
		t.Fatalf("no runID in start response")
	}

	// Signal.
	code, body = post("/v1/tasks/namespaces/default/workflows/e2e-1/signal?runId="+runID,
		`{"name":"ping","payload":{"x":1}}`)
	if code != 200 {
		t.Fatalf("signal: %d %s", code, body)
	}

	// History.
	code, body = get("/v1/tasks/namespaces/default/workflows/e2e-1/history?runId=" + runID)
	if code != 200 {
		t.Fatalf("history: %d %s", code, body)
	}
	var hist struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal(body, &hist)
	if len(hist.Events) != 2 {
		t.Fatalf("history len=%d want 2 (start + signal)", len(hist.Events))
	}
	if hist.Events[0]["eventType"] != "WORKFLOW_EXECUTION_STARTED" ||
		hist.Events[1]["eventType"] != "WORKFLOW_EXECUTION_SIGNALED" {
		t.Fatalf("history event types: %v %v",
			hist.Events[0]["eventType"], hist.Events[1]["eventType"])
	}

	// Query.
	code, body = post("/v1/tasks/namespaces/default/workflows/e2e-1/query?runId="+runID,
		`{"queryType":"__workflow_metadata"}`)
	if code != 200 {
		t.Fatalf("query: %d %s", code, body)
	}

	// List with visibility query.
	code, body = get(`/v1/tasks/namespaces/default/workflows?query=` + urlEscape(`WorkflowType = "Demo"`))
	if code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	var listed struct {
		Executions []map[string]any `json:"executions"`
	}
	_ = json.Unmarshal(body, &listed)
	if len(listed.Executions) != 1 {
		t.Fatalf("list len=%d", len(listed.Executions))
	}

	// Terminate.
	code, body = post("/v1/tasks/namespaces/default/workflows/e2e-1/terminate?runId="+runID,
		`{"reason":"e2e","identity":"test"}`)
	if code != 200 {
		t.Fatalf("terminate: %d %s", code, body)
	}

	// History now has 3 events: STARTED, SIGNALED, TERMINATED.
	code, body = get("/v1/tasks/namespaces/default/workflows/e2e-1/history?runId=" + runID)
	if code != 200 {
		t.Fatalf("history2: %d %s", code, body)
	}
	_ = json.Unmarshal(body, &hist)
	if len(hist.Events) != 3 {
		t.Fatalf("history2 len=%d want 3", len(hist.Events))
	}
}

// urlEscape is the smallest correct query escape — only ' ', '"',
// '=' show up in the test queries.
func urlEscape(s string) string {
	r := strings.NewReplacer(" ", "%20", `"`, "%22", "=", "%3D")
	return r.Replace(s)
}
