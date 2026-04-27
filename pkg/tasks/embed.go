// Copyright © 2026 Hanzo AI. MIT License.

// Embed runs the in-process Hanzo Tasks server. One backend, two
// transports: ZAP on :9999 (canonical, native, binary) and HTTP/JSON
// (browser-only, for the embedded UI). Both go through the engine →
// store layer so they cannot drift. No gRPC. No go.temporal.io.

package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/luxfi/zap"
)

// EmbedConfig configures the in-process Tasks server.
type EmbedConfig struct {
	DataDir   string       // "" → "./tasks-data" (reserved; memdb today)
	ZAPPort   int          // 0 → 9999
	Namespace string       // "" → "default"
	Logger    *slog.Logger // nil → slog.Default()
}

// Embedded is the handle to a running in-process Tasks server.
type Embedded struct {
	cfg    EmbedConfig
	node   *zap.Node
	engine *engine
	stop   chan struct{}
}

// Embed starts the Tasks server. Stop before exit.
func Embed(ctx context.Context, cfg EmbedConfig) (*Embedded, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "./tasks-data"
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	st := newStore()
	en := newEngine(st)

	// Bootstrap default namespace so the UI has something to render
	// on first boot. Idempotent.
	_ = en.RegisterNamespace(Namespace{
		NamespaceInfo: NamespaceInfo{
			Name:        cfg.Namespace,
			State:       "NAMESPACE_STATE_REGISTERED",
			Description: "Default namespace (bootstrapped)",
			Region:      "embedded",
		},
		Config: NamespaceCfg{WorkflowExecutionRetentionTtl: "720h", APSLimit: 400},
	})

	node := zap.NewNode(zap.NodeConfig{
		NodeID:      "tasks-embed",
		ServiceType: "_tasks._tcp",
		Port:        cfg.ZAPPort,
		Logger:      cfg.Logger,
		NoDiscovery: true,
	})

	for op, h := range zapHandlers(en, cfg.Namespace) {
		node.Handle(op, h)
	}

	if err := node.Start(); err != nil {
		return nil, fmt.Errorf("tasks.Embed: zap start: %w", err)
	}

	stop := make(chan struct{})
	go en.runScheduler(stop)

	return &Embedded{cfg: cfg, node: node, engine: en, stop: stop}, nil
}

// ZAPPort returns the bound ZAP port.
func (e *Embedded) ZAPPort() int {
	if e == nil {
		return 0
	}
	return e.cfg.ZAPPort
}

// Stop shuts the server down. Idempotent.
func (e *Embedded) Stop(ctx context.Context) error {
	if e == nil {
		return nil
	}
	if e.stop != nil {
		close(e.stop)
		e.stop = nil
	}
	if e.node != nil {
		e.node.Stop()
		e.node = nil
	}
	if e.engine != nil && e.engine.store != nil {
		_ = e.engine.store.close()
	}
	_ = ctx
	return nil
}

// MCPHandler returns the JSON-RPC 2.0 MCP endpoint.
func (e *Embedded) MCPHandler() http.Handler { return e.mcpHandler() }

// EventsHandler returns the SSE realtime stream of engine events.
func (e *Embedded) EventsHandler() http.Handler { return e.sseHandler() }

// HTTPHandler returns the browser-only JSON shim. Mirrors zapHandlers.
func (e *Embedded) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	en := e.engine

	// /v1/tasks/namespaces
	mux.HandleFunc("/v1/tasks/namespaces", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			rows, err := en.ListNamespaces()
			writeOK(w, err, map[string]any{"namespaces": rows})
		case http.MethodPost:
			var req struct {
				Namespace
			}
			if err := decode(r, &req); err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			err := en.RegisterNamespace(req.Namespace)
			writeOK(w, err, req.Namespace)
		default:
			http.NotFound(w, r)
		}
	})

	// /v1/tasks/namespaces/{ns}[/...]
	mux.HandleFunc("/v1/tasks/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/tasks/namespaces/")
		parts := strings.Split(rest, "/")
		ns := parts[0]
		if ns == "" {
			http.NotFound(w, r)
			return
		}
		if len(parts) == 1 {
			n, ok, err := en.DescribeNamespace(ns)
			if err != nil {
				writeErr(w, 500, err.Error())
				return
			}
			if !ok {
				writeErr(w, 404, "namespace not found")
				return
			}
			writeOK(w, nil, n)
			return
		}
		switch parts[1] {
		case "workflows":
			handleWorkflows(w, r, en, ns, parts[2:])
		case "schedules":
			handleSchedules(w, r, en, ns, parts[2:])
		case "batches":
			handleBatches(w, r, en, ns, parts[2:])
		case "deployments":
			handleDeployments(w, r, en, ns, parts[2:])
		case "nexus":
			handleNexus(w, r, en, ns, parts[2:])
		case "identities":
			handleIdentities(w, r, en, ns, parts[2:])
		case "task-queues":
			handleTaskQueues(w, r, en, ns, parts[2:])
		case "workers":
			handleWorkers(w, r, ns, parts[2:])
		default:
			http.NotFound(w, r)
		}
	})

	return mux
}

// ── per-resource HTTP routers ──────────────────────────────────────

func handleWorkflows(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		rows, err := en.ListWorkflows(ns)
		writeOK(w, err, map[string]any{"executions": rows})
	case len(sub) == 0 && r.Method == http.MethodPost:
		var req struct {
			WorkflowId   string  `json:"workflowId"`
			RunId        string  `json:"runId"`
			WorkflowType TypeRef `json:"workflowType"`
			TaskQueue    struct {
				Name string `json:"name"`
			} `json:"taskQueue"`
			Input any `json:"input"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		wf, err := en.StartWorkflow(ns, req.WorkflowId, req.RunId, req.WorkflowType, req.TaskQueue.Name, req.Input)
		writeOK(w, err, wf)
	case len(sub) == 1 && r.Method == http.MethodGet:
		runId := r.URL.Query().Get("execution.runId")
		wf, ok, err := en.DescribeWorkflow(ns, sub[0], runId)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "workflow not found")
			return
		}
		writeOK(w, nil, map[string]any{
			"workflowExecutionInfo": wf,
			"executionConfig": map[string]any{
				"taskQueue": map[string]string{"name": wf.TaskQueue},
			},
		})
	case len(sub) == 2 && sub[1] == "cancel" && r.Method == http.MethodPost:
		wf, err := en.CancelWorkflow(ns, sub[0], r.URL.Query().Get("runId"))
		writeOK(w, err, wf)
	case len(sub) == 2 && sub[1] == "terminate" && r.Method == http.MethodPost:
		wf, err := en.TerminateWorkflow(ns, sub[0], r.URL.Query().Get("runId"))
		writeOK(w, err, wf)
	case len(sub) == 2 && sub[1] == "signal" && r.Method == http.MethodPost:
		var req struct {
			Name    string `json:"name"`
			Payload any    `json:"payload"`
		}
		_ = decode(r, &req)
		err := en.SignalWorkflow(ns, sub[0], r.URL.Query().Get("runId"), req.Name, req.Payload)
		writeOK(w, err, map[string]string{"status": "signaled"})
	case len(sub) == 2 && sub[1] == "history" && r.Method == http.MethodGet:
		// Synthetic history derived from the workflow record. The native
		// engine doesn't yet emit per-event durable history, so we render
		// the start event, the latest signal counter, and (if closed) the
		// terminal transition. The UI surfaces a "coming in v1.42" hint
		// alongside this list — no fake events are fabricated.
		wf, ok, err := en.DescribeWorkflow(ns, sub[0], r.URL.Query().Get("runId"))
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "workflow not found")
			return
		}
		writeOK(w, nil, map[string]any{"events": synthHistory(wf), "synthetic": true})
	case len(sub) == 2 && sub[1] == "query" && r.Method == http.MethodPost:
		// Queries (including __stack_trace) require the worker SDK
		// runtime to be running. Until that lands, return a typed 501
		// the UI can render gracefully.
		writeErr(w, 501, "QueryWorkflow lands when the worker SDK runtime ships")
	default:
		http.NotFound(w, r)
	}
}

// synthHistory returns a small, honest event list derived from the
// workflow record. The engine doesn't yet store full history; this is
// what we know.
func synthHistory(wf *WorkflowExecution) []map[string]any {
	out := []map[string]any{
		{
			"eventId":   "1",
			"eventType": "WORKFLOW_EXECUTION_STARTED",
			"eventTime": wf.StartTime,
			"attributes": map[string]any{
				"workflowType": wf.Type.Name,
				"taskQueue":    wf.TaskQueue,
				"input":        wf.Input,
			},
		},
	}
	if wf.HistoryLen > 1 {
		out = append(out, map[string]any{
			"eventId":   "2",
			"eventType": "WORKFLOW_TASK_SIGNALED",
			"eventTime": wf.StartTime,
			"attributes": map[string]any{
				"signalCount": wf.HistoryLen - 1,
			},
		})
	}
	if wf.CloseTime != "" {
		out = append(out, map[string]any{
			"eventId":   fmt.Sprintf("%d", len(out)+1),
			"eventType": closeEventName(wf.Status),
			"eventTime": wf.CloseTime,
			"attributes": map[string]any{
				"status": wf.Status,
				"result": wf.Result,
			},
		})
	}
	return out
}

func closeEventName(status string) string {
	switch status {
	case "WORKFLOW_EXECUTION_STATUS_COMPLETED":
		return "WORKFLOW_EXECUTION_COMPLETED"
	case "WORKFLOW_EXECUTION_STATUS_FAILED":
		return "WORKFLOW_EXECUTION_FAILED"
	case "WORKFLOW_EXECUTION_STATUS_CANCELED":
		return "WORKFLOW_EXECUTION_CANCELED"
	case "WORKFLOW_EXECUTION_STATUS_TERMINATED":
		return "WORKFLOW_EXECUTION_TERMINATED"
	case "WORKFLOW_EXECUTION_STATUS_TIMED_OUT":
		return "WORKFLOW_EXECUTION_TIMED_OUT"
	default:
		return "WORKFLOW_EXECUTION_CLOSED"
	}
}

// handleTaskQueues derives queues from listed workflows. Honest: there
// is no separate "task queue" object in storage yet; queues live as
// strings on workflows. Aggregating them here is cheap and matches what
// the upstream UI shows on first paint.
func handleTaskQueues(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		rows, err := en.ListWorkflows(ns)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeOK(w, nil, map[string]any{"taskQueues": aggregateTaskQueues(rows)})
	case len(sub) == 1 && r.Method == http.MethodGet:
		rows, err := en.ListWorkflows(ns)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeOK(w, nil, taskQueueDetail(rows, sub[0]))
	case len(sub) == 2 && sub[1] == "workers" && r.Method == http.MethodGet:
		// Worker registration is part of the worker SDK runtime that
		// hasn't shipped. Always return an empty list — no fake data.
		writeOK(w, nil, map[string]any{"workers": []any{}})
	default:
		http.NotFound(w, r)
	}
}

// handleWorkers — namespace-wide worker listing. Same honesty story.
func handleWorkers(w http.ResponseWriter, r *http.Request, ns string, sub []string) {
	_ = ns
	if len(sub) == 0 && r.Method == http.MethodGet {
		writeOK(w, nil, map[string]any{"workers": []any{}})
		return
	}
	http.NotFound(w, r)
}

type taskQueueSummary struct {
	Name        string `json:"name"`
	Workflows   int    `json:"workflows"`
	Running     int    `json:"running"`
	LatestStart string `json:"latestStart,omitempty"`
}

func aggregateTaskQueues(rows []WorkflowExecution) []taskQueueSummary {
	by := map[string]*taskQueueSummary{}
	for i := range rows {
		wf := &rows[i]
		q := wf.TaskQueue
		if q == "" {
			q = "default"
		}
		s, ok := by[q]
		if !ok {
			s = &taskQueueSummary{Name: q}
			by[q] = s
		}
		s.Workflows++
		if wf.Status == "WORKFLOW_EXECUTION_STATUS_RUNNING" {
			s.Running++
		}
		if wf.StartTime > s.LatestStart {
			s.LatestStart = wf.StartTime
		}
	}
	out := make([]taskQueueSummary, 0, len(by))
	for _, s := range by {
		out = append(out, *s)
	}
	return out
}

func taskQueueDetail(rows []WorkflowExecution, queue string) map[string]any {
	matches := make([]WorkflowExecution, 0, len(rows))
	for i := range rows {
		q := rows[i].TaskQueue
		if q == "" {
			q = "default"
		}
		if q == queue {
			matches = append(matches, rows[i])
		}
	}
	running := 0
	for i := range matches {
		if matches[i].Status == "WORKFLOW_EXECUTION_STATUS_RUNNING" {
			running++
		}
	}
	return map[string]any{
		"name":       queue,
		"workflows":  matches,
		"running":    running,
		"total":      len(matches),
	}
}

func handleSchedules(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		rows, err := en.ListSchedules(ns)
		writeOK(w, err, map[string]any{"schedules": rows})
	case len(sub) == 0 && r.Method == http.MethodPost:
		var req Schedule
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		req.Namespace = ns
		err := en.CreateSchedule(req)
		writeOK(w, err, req)
	case len(sub) == 1 && r.Method == http.MethodGet:
		s, ok, err := en.DescribeSchedule(ns, sub[0])
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "schedule not found")
			return
		}
		writeOK(w, nil, s)
	case len(sub) == 1 && r.Method == http.MethodDelete:
		err := en.DeleteSchedule(ns, sub[0])
		writeOK(w, err, map[string]string{"status": "deleted"})
	case len(sub) == 2 && sub[1] == "pause" && r.Method == http.MethodPost:
		var req struct {
			Note string `json:"note"`
		}
		_ = decode(r, &req)
		err := en.PauseSchedule(ns, sub[0], true, req.Note)
		writeOK(w, err, map[string]string{"status": "paused"})
	case len(sub) == 2 && sub[1] == "unpause" && r.Method == http.MethodPost:
		err := en.PauseSchedule(ns, sub[0], false, "")
		writeOK(w, err, map[string]string{"status": "running"})
	default:
		http.NotFound(w, r)
	}
}

func handleBatches(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		rows, err := en.ListBatches(ns)
		writeOK(w, err, map[string]any{"batches": rows})
	case len(sub) == 0 && r.Method == http.MethodPost:
		var req BatchOperation
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		req.Namespace = ns
		b, err := en.StartBatch(req)
		writeOK(w, err, b)
	case len(sub) == 1 && r.Method == http.MethodGet:
		b, ok, err := en.DescribeBatch(ns, sub[0])
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "batch not found")
			return
		}
		writeOK(w, nil, b)
	default:
		http.NotFound(w, r)
	}
}

func handleDeployments(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		rows, err := en.ListDeployments(ns)
		writeOK(w, err, map[string]any{"deployments": rows})
	case len(sub) == 0 && r.Method == http.MethodPost:
		var req Deployment
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		req.Namespace = ns
		err := en.CreateDeployment(req)
		writeOK(w, err, req)
	default:
		http.NotFound(w, r)
	}
}

func handleNexus(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		rows, err := en.ListNexusEndpoints(ns)
		writeOK(w, err, map[string]any{"endpoints": rows})
	case len(sub) == 0 && r.Method == http.MethodPost:
		var req NexusEndpoint
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		req.Namespace = ns
		err := en.CreateNexusEndpoint(req)
		writeOK(w, err, req)
	default:
		http.NotFound(w, r)
	}
}

func handleIdentities(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	switch {
	case len(sub) == 0 && r.Method == http.MethodGet:
		rows, err := en.ListIdentities(ns)
		writeOK(w, err, map[string]any{"identities": rows})
	case len(sub) == 0 && r.Method == http.MethodPost:
		var req Identity
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		req.Namespace = ns
		err := en.GrantIdentity(req)
		writeOK(w, err, req)
	default:
		http.NotFound(w, r)
	}
}

// ── ZAP handler dispatch ────────────────────────────────────────────

const (
	opStartWorkflow           uint16 = 0x0060
	opSignalWorkflow          uint16 = 0x0061
	opCancelWorkflow          uint16 = 0x0062
	opTerminateWorkflow       uint16 = 0x0063
	opDescribeWorkflow        uint16 = 0x0064
	opListWorkflows           uint16 = 0x0065
	opCreateSchedule          uint16 = 0x0070
	opDeleteSchedule          uint16 = 0x0071
	opListSchedules           uint16 = 0x0072
	opPauseSchedule           uint16 = 0x0073
	opUnpauseSchedule         uint16 = 0x0074
	opDescribeSchedule        uint16 = 0x0076
	opRegisterNamespace       uint16 = 0x0080
	opDescribeNamespace       uint16 = 0x0081
	opListNamespaces          uint16 = 0x0082
	opHealth                  uint16 = 0x0090
)

func zapHandlers(en *engine, defaultNS string) map[uint16]zap.Handler {
	envBody := func(v any) ([]byte, error) { return json.Marshal(v) }
	wrap := func(fn func(req map[string]any) (any, uint32, string)) zap.Handler {
		return func(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
			req := map[string]any{}
			if msg != nil {
				root := msg.Root()
				if !root.IsNull() {
					if rb := root.Bytes(envelopeBody); len(rb) > 0 {
						_ = json.Unmarshal(rb, &req)
					}
				}
			}
			out, status, errMsg := fn(req)
			body, _ := envBody(out)
			return envelope(body, status, errMsg)
		}
	}
	str := func(req map[string]any, k string) string {
		if v, ok := req[k].(string); ok {
			return v
		}
		return ""
	}
	strOr := func(req map[string]any, k, def string) string {
		if v, ok := req[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	// scheduleSDK marshals an engine Schedule into the SDK shape.
	scheduleSDK := func(s *Schedule) map[string]any {
		return map[string]any{
			"id": s.ScheduleId,
			"spec": map[string]any{
				"cron": s.Spec.CronString,
			},
			"action": map[string]any{
				"workflow_type": s.Action.WorkflowType.Name,
				"task_queue":    s.Action.TaskQueue,
			},
			"paused": s.State.Paused,
		}
	}
	_ = scheduleSDK
	// wfInfoSDK marshals an engine WorkflowExecution into the
	// snake_case SDK shape (WorkflowExecutionInfo) expected over ZAP.
	wfInfoSDK := func(wf *WorkflowExecution) map[string]any {
		statusInt := 0
		switch wf.Status {
		case "WORKFLOW_EXECUTION_STATUS_RUNNING":
			statusInt = 1
		case "WORKFLOW_EXECUTION_STATUS_COMPLETED":
			statusInt = 2
		case "WORKFLOW_EXECUTION_STATUS_FAILED":
			statusInt = 3
		case "WORKFLOW_EXECUTION_STATUS_CANCELED":
			statusInt = 4
		case "WORKFLOW_EXECUTION_STATUS_TERMINATED":
			statusInt = 5
		case "WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW":
			statusInt = 6
		}
		out := map[string]any{
			"workflow_id":    wf.Execution.WorkflowId,
			"run_id":         wf.Execution.RunId,
			"workflow_type":  wf.Type.Name,
			"status":         statusInt,
			"history_length": wf.HistoryLen,
			"task_queue":     wf.TaskQueue,
		}
		// Empty time strings won't parse with omitempty time.Time on the
		// SDK side; only include when present.
		if wf.StartTime != "" {
			out["start_time"] = wf.StartTime
		}
		if wf.CloseTime != "" {
			out["close_time"] = wf.CloseTime
		}
		return out
	}
	return map[uint16]zap.Handler{
		opHealth: wrap(func(_ map[string]any) (any, uint32, string) {
			return map[string]any{"service": "tasks", "status": "ok", "namespace": defaultNS}, 200, ""
		}),
		opListNamespaces: wrap(func(_ map[string]any) (any, uint32, string) {
			rows, err := en.ListNamespaces()
			if err != nil {
				return nil, 500, err.Error()
			}
			return map[string]any{"namespaces": rows}, 200, ""
		}),
		opDescribeNamespace: wrap(func(req map[string]any) (any, uint32, string) {
			n, ok, err := en.DescribeNamespace(str(req, "namespace"))
			if err != nil {
				return nil, 500, err.Error()
			}
			if !ok {
				return nil, 404, "namespace not found"
			}
			return n, 200, ""
		}),
		opRegisterNamespace: wrap(func(req map[string]any) (any, uint32, string) {
			var ns Namespace
			if b, _ := json.Marshal(req); b != nil {
				_ = json.Unmarshal(b, &ns)
			}
			if err := en.RegisterNamespace(ns); err != nil {
				return nil, 400, err.Error()
			}
			return ns, 200, ""
		}),
		opStartWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			typeName := strOr(req, "workflow_type", "")
			if typeName == "" {
				if t, ok := req["workflowType"].(map[string]any); ok {
					typeName, _ = t["name"].(string)
				}
			}
			tq := strOr(req, "task_queue", "")
			if tq == "" {
				if q, ok := req["taskQueue"].(map[string]any); ok {
					tq, _ = q["name"].(string)
				}
			}
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			wf, err := en.StartWorkflow(ns, wfID, runID, TypeRef{Name: typeName}, tq, req["input"])
			if err != nil {
				return nil, 400, err.Error()
			}
			// SDK expects {run_id} response shape.
			return map[string]any{"run_id": wf.Execution.RunId, "workflow_id": wf.Execution.WorkflowId}, 200, ""
		}),
		opListWorkflows: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			rows, err := en.ListWorkflows(ns)
			if err != nil {
				return nil, 500, err.Error()
			}
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				out = append(out, wfInfoSDK(&rows[i]))
			}
			return map[string]any{"executions": out}, 200, ""
		}),
		opDescribeWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			wf, ok, err := en.DescribeWorkflow(ns, wfID, runID)
			if err != nil {
				return nil, 500, err.Error()
			}
			if !ok {
				return nil, 404, "workflow not found"
			}
			return map[string]any{"info": wfInfoSDK(wf)}, 200, ""
		}),
		opSignalWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			sigName := strOr(req, "signal_name", str(req, "signalName"))
			err := en.SignalWorkflow(ns, wfID, runID, sigName, req["input"])
			if err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"status": "signaled"}, 200, ""
		}),
		opCancelWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			wf, err := en.CancelWorkflow(ns, wfID, runID)
			if err != nil {
				return nil, 400, err.Error()
			}
			return wfInfoSDK(wf), 200, ""
		}),
		opTerminateWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			wf, err := en.TerminateWorkflow(ns, wfID, runID)
			if err != nil {
				return nil, 400, err.Error()
			}
			return wfInfoSDK(wf), 200, ""
		}),
		opListSchedules: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			rows, err := en.ListSchedules(ns)
			if err != nil {
				return nil, 500, err.Error()
			}
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				out = append(out, scheduleSDK(&rows[i]))
			}
			return map[string]any{"schedules": out}, 200, ""
		}),
		opCreateSchedule: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			s := Schedule{ScheduleId: id, Namespace: ns}
			// Re-marshal the "schedule" field and let the SDK shape's
			// custom UnmarshalJSON handle it via a side struct.
			if sched, ok := req["schedule"].(map[string]any); ok {
				if spec, ok := sched["spec"].(map[string]any); ok {
					if cron, ok := spec["cron"].([]any); ok {
						for _, c := range cron {
							if cs, ok := c.(string); ok {
								s.Spec.CronString = append(s.Spec.CronString, cs)
							}
						}
					}
				}
				if action, ok := sched["action"].(map[string]any); ok {
					s.Action.WorkflowType.Name, _ = action["workflow_type"].(string)
					s.Action.TaskQueue, _ = action["task_queue"].(string)
				}
				if p, ok := sched["paused"].(bool); ok {
					s.State.Paused = p
				}
			}
			if s.ScheduleId == "" {
				return nil, 400, "schedule_id required"
			}
			if err := en.CreateSchedule(s); err != nil {
				return nil, 400, err.Error()
			}
			return scheduleSDK(&s), 200, ""
		}),
		opDescribeSchedule: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			s, ok, err := en.DescribeSchedule(ns, id)
			if err != nil {
				return nil, 500, err.Error()
			}
			if !ok {
				return nil, 404, "schedule not found"
			}
			return scheduleSDK(s), 200, ""
		}),
		opDeleteSchedule: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			if err := en.DeleteSchedule(ns, id); err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"status": "deleted"}, 200, ""
		}),
		opPauseSchedule: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			paused := true
			if p, ok := req["paused"].(bool); ok {
				paused = p
			}
			if err := en.PauseSchedule(ns, id, paused, str(req, "note")); err != nil {
				return nil, 400, err.Error()
			}
			out := "paused"
			if !paused {
				out = "running"
			}
			return map[string]string{"status": out}, 200, ""
		}),
		opUnpauseSchedule: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			id := strOr(req, "schedule_id", str(req, "scheduleId"))
			if err := en.PauseSchedule(ns, id, false, ""); err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"status": "running"}, 200, ""
		}),
	}
}

// ── envelope (ZAP single-field JSON shape) ─────────────────────────

const (
	envelopeBody       = 0
	envelopeStatus     = 8
	envelopeError      = 12
	envelopeObjectSize = 24
)

func envelope(body []byte, status uint32, errMsg string) (*zap.Message, error) {
	b := zap.NewBuilder(envelopeObjectSize + len(body) + len(errMsg) + 64)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, status)
	obj.SetBytes(envelopeError, []byte(errMsg))
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

// ── HTTP helpers ────────────────────────────────────────────────────

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

func writeOK(w http.ResponseWriter, err error, body any) {
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": msg, "code": code})
}
