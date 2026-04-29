// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTaskQueuesAndWorkers(t *testing.T) {
	emb, err := Embed(context.Background(), EmbedConfig{ZAPPort: 0})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(context.Background())

	srv := httptest.NewServer(emb.HTTPHandler())
	defer srv.Close()

	// Seed a workflow so task-queue aggregation has something to find.
	resp, err := http.Post(srv.URL+"/v1/tasks/namespaces/default/workflows",
		"application/json",
		strings.NewReader(`{"workflowType":{"name":"Demo"},"taskQueue":{"name":"alpha"}}`))
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start workflow status: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// /task-queues — should list the alpha queue with 1 workflow.
	resp, err = http.Get(srv.URL + "/v1/tasks/namespaces/default/task-queues")
	if err != nil {
		t.Fatalf("list queues: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list queues status %d: %s", resp.StatusCode, body)
	}
	var listResp struct {
		TaskQueues []struct {
			Name      string `json:"name"`
			Workflows int    `json:"workflows"`
			Running   int    `json:"running"`
		} `json:"taskQueues"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.TaskQueues) != 1 || listResp.TaskQueues[0].Name != "alpha" {
		t.Fatalf("unexpected queues: %+v", listResp.TaskQueues)
	}
	if listResp.TaskQueues[0].Workflows != 1 || listResp.TaskQueues[0].Running != 1 {
		t.Fatalf("unexpected counts: %+v", listResp.TaskQueues[0])
	}

	// /task-queues/{queue} detail.
	resp, err = http.Get(srv.URL + "/v1/tasks/namespaces/default/task-queues/alpha")
	if err != nil {
		t.Fatalf("queue detail: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue detail status %d: %s", resp.StatusCode, body)
	}
	var detail struct {
		Name      string `json:"name"`
		Total     int    `json:"total"`
		Running   int    `json:"running"`
		Workflows []any  `json:"workflows"`
	}
	if err := json.Unmarshal(body, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Name != "alpha" || detail.Total != 1 || detail.Running != 1 {
		t.Fatalf("unexpected detail: %+v", detail)
	}

	// /task-queues/{queue}/workers — stub, empty list.
	resp, err = http.Get(srv.URL + "/v1/tasks/namespaces/default/task-queues/alpha/workers")
	if err != nil {
		t.Fatalf("queue workers: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue workers status %d: %s", resp.StatusCode, body)
	}
	var workersResp struct {
		Workers []any `json:"workers"`
	}
	if err := json.Unmarshal(body, &workersResp); err != nil {
		t.Fatalf("decode workers: %v", err)
	}
	if len(workersResp.Workers) != 0 {
		t.Fatalf("expected empty workers, got %d", len(workersResp.Workers))
	}

	// /workers — namespace-wide stub.
	resp, err = http.Get(srv.URL + "/v1/tasks/namespaces/default/workers")
	if err != nil {
		t.Fatalf("ns workers: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ns workers status %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &workersResp); err != nil {
		t.Fatalf("decode ns workers: %v", err)
	}
	if len(workersResp.Workers) != 0 {
		t.Fatalf("expected empty ns workers, got %d", len(workersResp.Workers))
	}
}

func TestWorkflowHistoryAndQuery(t *testing.T) {
	emb, err := Embed(context.Background(), EmbedConfig{ZAPPort: 0})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	defer emb.Stop(context.Background())

	srv := httptest.NewServer(emb.HTTPHandler())
	defer srv.Close()

	// Start a workflow.
	resp, err := http.Post(srv.URL+"/v1/tasks/namespaces/default/workflows",
		"application/json",
		strings.NewReader(`{"workflowId":"wf-1","workflowType":{"name":"Demo"},"taskQueue":{"name":"q"}}`))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("start status %d: %s", resp.StatusCode, body)
	}
	var started struct {
		Execution struct {
			RunId string `json:"runId"`
		} `json:"execution"`
	}
	if err := json.Unmarshal(body, &started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	runId := started.Execution.RunId

	// /history — durable history; the start should have appended a
	// WORKFLOW_EXECUTION_STARTED event.
	resp, err = http.Get(srv.URL + "/v1/tasks/namespaces/default/workflows/wf-1/history?runId=" + runId)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("history status %d: %s", resp.StatusCode, body)
	}
	var hist struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(body, &hist); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(hist.Events) != 1 || hist.Events[0]["eventType"] != "WORKFLOW_EXECUTION_STARTED" {
		t.Fatalf("unexpected history events: %+v", hist.Events)
	}

	// /query — synchronous reply (worker poller dispatch is a follow-up;
	// the engine returns workflow metadata for known synthetic queries).
	resp, err = http.Post(srv.URL+"/v1/tasks/namespaces/default/workflows/wf-1/query?runId="+runId,
		"application/json",
		strings.NewReader(`{"queryType":"__workflow_metadata"}`))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query status %d: %s", resp.StatusCode, body)
	}
}
