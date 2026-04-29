// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCancelHandshake_NoSubscribers asserts that cancelling with no
// worker subscribed converges synchronously to CANCELED — preserving
// the v1 API surface while keeping the CANCELING state machinery.
func TestCancelHandshake_NoSubscribers(t *testing.T) {
	en, ns := engineFixture(t)
	wf, _ := en.StartWorkflow(ns, "wf-c", "", TypeRef{Name: "Demo"}, "default", nil)
	got, err := en.CancelWorkflowWithReason(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "x", "z")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Status != "WORKFLOW_EXECUTION_STATUS_CANCELED" {
		t.Fatalf("status=%s, want CANCELED (no subscriber → auto-ack)", got.Status)
	}
	if got.HistoryLen != 3 {
		t.Fatalf("history=%d, want 3", got.HistoryLen)
	}
}

// TestCancelHandshake_WithWorker asserts the CANCELING state is
// observable while a worker is subscribed, and that the worker's ack
// (kind=3 command) drives the transition to CANCELED.
func TestCancelHandshake_WithWorker(t *testing.T) {
	en, ns := engineFixture(t)
	cap := &captureSend{}
	en.disp.send = cap.fn

	wf, _ := en.StartWorkflow(ns, "wf-c", "", TypeRef{Name: "Demo"}, "default", nil)
	if _, err := en.disp.Subscribe("peerA", ns, "default", kindWorkflow); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	got, err := en.CancelWorkflowWithReason(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "user requested", "z@hanzo.ai")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Status != "WORKFLOW_EXECUTION_STATUS_CANCELING" {
		t.Fatalf("status=%s, want CANCELING", got.Status)
	}

	// Cancel-request push should be observable.
	calls := cap.snapshot()
	var sawCancel bool
	for _, c := range calls {
		if c.opcode == OpcodeDeliverCancelRequest {
			sawCancel = true
			var body cancelRequestDeliveryJSON
			if err := json.Unmarshal(c.body, &body); err != nil {
				t.Fatalf("decode cancel request: %v", err)
			}
			if body.WorkflowID != wf.Execution.WorkflowId || body.Reason != "user requested" {
				t.Fatalf("unexpected cancel body: %+v", body)
			}
		}
	}
	if !sawCancel {
		t.Fatalf("no DeliverCancelRequest observed; calls=%+v", calls)
	}

	// Simulate worker ack: AckCanceled (this is what the embed handler
	// invokes when it sees command kind=3).
	acked, err := en.AckCanceled(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "ack", "peerA")
	if err != nil {
		t.Fatalf("ack: %v", err)
	}
	if acked.Status != "WORKFLOW_EXECUTION_STATUS_CANCELED" {
		t.Fatalf("after ack status=%s, want CANCELED", acked.Status)
	}
}

// TestCancelHandshake_SweepAutoCompletes asserts a CANCELING workflow
// expires on the sweeper after cancelTimeout.
func TestCancelHandshake_SweepAutoCompletes(t *testing.T) {
	old := cancelTimeout
	cancelTimeout = 10 * time.Millisecond
	defer func() { cancelTimeout = old }()

	en, ns := engineFixture(t)
	cap := &captureSend{}
	en.disp.send = cap.fn

	wf, _ := en.StartWorkflow(ns, "wf-c", "", TypeRef{Name: "Demo"}, "default", nil)
	if _, err := en.disp.Subscribe("peerA", ns, "default", kindWorkflow); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	got, err := en.CancelWorkflowWithReason(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "x", "z")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Status != "WORKFLOW_EXECUTION_STATUS_CANCELING" {
		t.Fatalf("status=%s, want CANCELING", got.Status)
	}
	time.Sleep(30 * time.Millisecond)
	en.sweepCanceling()
	final, _, _ := en.DescribeWorkflow(ns, wf.Execution.WorkflowId, wf.Execution.RunId)
	if final.Status != "WORKFLOW_EXECUTION_STATUS_CANCELED" {
		t.Fatalf("after sweep status=%s, want CANCELED", final.Status)
	}
}

// TestCancelHandshake_NoDoubleTransition asserts re-cancel while
// CANCELING is idempotent (no extra CANCEL_REQUESTED event).
func TestCancelHandshake_NoDoubleTransition(t *testing.T) {
	en, ns := engineFixture(t)
	en.disp.send = (&captureSend{}).fn

	wf, _ := en.StartWorkflow(ns, "wf-c", "", TypeRef{Name: "Demo"}, "default", nil)
	if _, err := en.disp.Subscribe("peerA", ns, "default", kindWorkflow); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	first, _ := en.CancelWorkflowWithReason(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "x", "z")
	again, _ := en.CancelWorkflowWithReason(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "x", "z")
	if first.HistoryLen != again.HistoryLen {
		t.Fatalf("history len changed on re-cancel: %d vs %d", first.HistoryLen, again.HistoryLen)
	}
}

// TestQueryPush_NoWorkers asserts ErrNoWorkersSubscribed is returned
// when the workflow's task queue has no subscriber.
func TestQueryPush_NoWorkers(t *testing.T) {
	en, ns := engineFixture(t)
	wf, _ := en.StartWorkflow(ns, "wf-q", "", TypeRef{Name: "Demo"}, "qx", nil)
	_, err := en.QueryWorkflow(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "userQuery", nil)
	if err != ErrNoWorkersSubscribed {
		t.Fatalf("err = %v, want ErrNoWorkersSubscribed", err)
	}
}

// TestQueryPush_WorkerResponds simulates a worker that intercepts the
// DeliverQuery push and answers via CompleteQuery.
func TestQueryPush_WorkerResponds(t *testing.T) {
	en, ns := engineFixture(t)
	wf, _ := en.StartWorkflow(ns, "wf-q", "", TypeRef{Name: "Demo"}, "default", nil)

	// Wire send to capture the query token and respond synchronously
	// from a goroutine simulating the worker.
	var sentToken string
	var mu sync.Mutex
	en.disp.send = func(peerID string, opcode uint16, body []byte) error {
		if opcode != OpcodeDeliverQuery {
			return nil
		}
		var q queryDeliveryJSON
		if err := json.Unmarshal(body, &q); err != nil {
			return err
		}
		mu.Lock()
		sentToken = q.Token
		mu.Unlock()
		go func() {
			time.Sleep(5 * time.Millisecond)
			en.disp.CompleteQuery(q.Token, []byte(`{"answer":42}`), "")
		}()
		return nil
	}

	if _, err := en.disp.Subscribe("peerA", ns, "default", kindWorkflow); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	out, err := en.QueryWorkflow(ns, wf.Execution.WorkflowId, wf.Execution.RunId, "userQuery", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	mu.Lock()
	if sentToken == "" {
		t.Fatalf("no token captured")
	}
	mu.Unlock()
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result type: %T", out)
	}
	if m["answer"].(float64) != 42 {
		t.Fatalf("unexpected answer: %v", m["answer"])
	}
}

// TestQueryPush_WorkerTimeout asserts the query times out when the
// worker never responds.
func TestQueryPush_WorkerTimeout(t *testing.T) {
	en, ns := engineFixture(t)
	wf, _ := en.StartWorkflow(ns, "wf-q", "", TypeRef{Name: "Demo"}, "default", nil)
	en.disp.send = func(string, uint16, []byte) error { return nil }
	if _, err := en.disp.Subscribe("peerA", ns, "default", kindWorkflow); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := en.QueryWorkflowCtx(ctx, ns, wf.Execution.WorkflowId, wf.Execution.RunId, "userQuery", nil)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}

// ── admin endpoints ─────────────────────────────────────────────────

func adminFixture(t *testing.T) (*Embedded, *httptest.Server) {
	t.Helper()
	emb, err := Embed(context.Background(), EmbedConfig{ZAPPort: 0})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	srv := httptest.NewServer(emb.HTTPHandler())
	t.Cleanup(func() {
		srv.Close()
		_ = emb.Stop(context.Background())
	})
	return emb, srv
}

func httpPost(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	out := make([]byte, 0, 1024)
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			out = append(out, buf[:n]...)
		}
		if err != nil {
			break
		}
	}
	return resp.StatusCode, out
}

func TestAdmin_SearchAttributes(t *testing.T) {
	_, srv := adminFixture(t)
	cases := []struct {
		name   string
		body   string
		code   int
	}{
		{"ok", `{"name":"OrderId","type":"Keyword"}`, 200},
		{"duplicate", `{"name":"OrderId","type":"Keyword"}`, 409},
		{"bad type", `{"name":"X","type":"BadType"}`, 400},
		{"missing name", `{"type":"Keyword"}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, body := httpPost(t, srv.URL+"/v1/tasks/namespaces/default/search-attributes", c.body)
			if code != c.code {
				t.Fatalf("code=%d want %d body=%s", code, c.code, body)
			}
		})
	}
	t.Run("not found", func(t *testing.T) {
		code, _ := httpPost(t, srv.URL+"/v1/tasks/namespaces/ghost/search-attributes", `{"name":"X","type":"Keyword"}`)
		if code != 404 {
			t.Fatalf("code=%d want 404", code)
		}
	})
}

func TestAdmin_NamespaceMetadata(t *testing.T) {
	_, srv := adminFixture(t)
	t.Run("ok", func(t *testing.T) {
		desc := "updated"
		body, _ := json.Marshal(NamespaceMetadataPatch{Description: &desc})
		code, _ := httpPost(t, srv.URL+"/v1/tasks/namespaces/default/metadata", string(body))
		if code != 200 {
			t.Fatalf("code=%d", code)
		}
	})
	t.Run("not found", func(t *testing.T) {
		desc := "x"
		body, _ := json.Marshal(NamespaceMetadataPatch{Description: &desc})
		code, _ := httpPost(t, srv.URL+"/v1/tasks/namespaces/ghost/metadata", string(body))
		if code != 404 {
			t.Fatalf("code=%d want 404", code)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		owner := "z@hanzo.ai"
		body, _ := json.Marshal(NamespaceMetadataPatch{OwnerEmail: &owner})
		c1, _ := httpPost(t, srv.URL+"/v1/tasks/namespaces/default/metadata", string(body))
		c2, _ := httpPost(t, srv.URL+"/v1/tasks/namespaces/default/metadata", string(body))
		if c1 != 200 || c2 != 200 {
			t.Fatalf("codes %d %d", c1, c2)
		}
	})
}

func TestAdmin_BatchTerminate(t *testing.T) {
	emb, srv := adminFixture(t)
	b, err := emb.engine.StartBatch(BatchOperation{Namespace: "default", Operation: "BATCH_OPERATION_TYPE_TERMINATE"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	url := srv.URL + "/v1/tasks/namespaces/default/batches/" + b.BatchId + "/terminate"
	t.Run("ok", func(t *testing.T) {
		code, _ := httpPost(t, url, `{"reason":"x","identity":"y"}`)
		if code != 200 {
			t.Fatalf("code=%d", code)
		}
	})
	t.Run("idempotent", func(t *testing.T) {
		code, _ := httpPost(t, url, `{}`)
		if code != 200 {
			t.Fatalf("code=%d", code)
		}
	})
	t.Run("not found", func(t *testing.T) {
		code, _ := httpPost(t, srv.URL+"/v1/tasks/namespaces/default/batches/nonex/terminate", `{}`)
		if code != 404 {
			t.Fatalf("code=%d want 404", code)
		}
	})
}

func TestAdmin_WorkflowMetadata(t *testing.T) {
	emb, srv := adminFixture(t)
	wf, err := emb.engine.StartWorkflow("default", "wf-md", "", TypeRef{Name: "Demo"}, "q", nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	url := srv.URL + "/v1/tasks/namespaces/default/workflows/" + wf.Execution.WorkflowId + "/metadata?runId=" + wf.Execution.RunId
	t.Run("ok", func(t *testing.T) {
		code, body := httpPost(t, url, `{"summary":"hello","details":"world","updatedBy":"z@hanzo.ai"}`)
		if code != 200 {
			t.Fatalf("code=%d body=%s", code, body)
		}
		var got WorkflowExecution
		_ = json.Unmarshal(body, &got)
		if got.UserMetadata == nil || got.UserMetadata.Summary != "hello" || got.UserMetadata.UpdatedAt == "" {
			t.Fatalf("unexpected metadata: %+v", got.UserMetadata)
		}
	})
	t.Run("not found", func(t *testing.T) {
		code, _ := httpPost(t, srv.URL+"/v1/tasks/namespaces/default/workflows/ghost/metadata", `{}`)
		if code != 404 {
			t.Fatalf("code=%d want 404", code)
		}
	})
}
