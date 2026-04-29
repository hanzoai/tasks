// Copyright © 2026 Hanzo AI. MIT License.

// Embed runs the in-process Hanzo Tasks server. One backend, two
// transports: ZAP on :9999 (canonical, native, binary) and HTTP/JSON
// (browser-only, for the embedded UI). Both go through the engine →
// store layer so they cannot drift. No gRPC. No go.temporal.io.

package tasks

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hanzoai/tasks/pkg/auth"
	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/luxfi/zap"
)

var base64Std = base64.StdEncoding

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

	// Wire dispatcher → node.Send for server-push delivery.
	en.disp.send = func(peerID string, opcode uint16, body []byte) error {
		msg, err := wireSend(opcode, body)
		if err != nil {
			return err
		}
		return node.Send(context.Background(), peerID, msg)
	}

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

// wireSend builds a ZAP message for server-initiated push: the body is
// wrapped in the same single-field envelope used by request/response
// (status=0, error=""), with the opcode stamped in the frame's flag
// high byte so the receiver dispatches on it.
func wireSend(opcode uint16, body []byte) (*zap.Message, error) {
	b := zap.NewBuilder(envelopeObjectSize + len(body) + 32)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, 200)
	obj.FinishAsRoot()
	flags := uint16(opcode) << 8
	frame := b.FinishWithFlags(flags)
	return zap.Parse(frame)
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
// Per-request engine is scoped to the X-Org-Id minted by pkg/auth from
// the validated IAM JWT (Authorization: Bearer). Client-supplied
// identity headers are stripped before the handler runs. Empty org →
// legacy unscoped store (embedded/dev path only).
func (e *Embedded) HTTPHandler() http.Handler {
	mux := http.NewServeMux()

	// /v1/tasks/namespaces
	mux.HandleFunc("/v1/tasks/namespaces", func(w http.ResponseWriter, r *http.Request) {
		en := e.engine.WithOrg(auth.OrgID(r.Context()))
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
		en := e.engine.WithOrg(auth.OrgID(r.Context()))
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
		case "search-attributes":
			handleSearchAttributes(w, r, en, ns, parts[2:])
		case "metadata":
			handleNamespaceMetadata(w, r, en, ns, parts[2:])
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
		query := r.URL.Query().Get("query")
		rows, err := en.ListWorkflowExecutions(ns, query)
		writeOK(w, err, map[string]any{"executions": rows})
	case len(sub) == 0 && r.Method == http.MethodPost:
		var req struct {
			WorkflowId   string  `json:"workflowId"`
			RunId        string  `json:"runId"`
			WorkflowType TypeRef `json:"workflowType"`
			TaskQueue    struct {
				Name string `json:"name"`
			} `json:"taskQueue"`
			Input     any    `json:"input"`
			RequestId string `json:"requestId"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		wf, err := en.StartWorkflowWithRequestID(ns, req.WorkflowId, req.RunId, req.WorkflowType, req.TaskQueue.Name, req.Input, req.RequestId)
		writeOK(w, err, wf)
	case len(sub) == 1 && sub[0] == "signal-with-start" && r.Method == http.MethodPost:
		var req struct {
			WorkflowId    string  `json:"workflowId"`
			RunId         string  `json:"runId"`
			WorkflowType  TypeRef `json:"workflowType"`
			TaskQueue     struct {
				Name string `json:"name"`
			} `json:"taskQueue"`
			Input         any    `json:"input"`
			SignalName    string `json:"signalName"`
			SignalPayload any    `json:"signalPayload"`
			RequestId     string `json:"requestId"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if req.SignalName == "" {
			writeErr(w, 400, "signalName required")
			return
		}
		wf, err := en.SignalWithStartWorkflow(ns, req.WorkflowId, req.RunId, req.WorkflowType, req.TaskQueue.Name, req.Input, req.SignalName, req.SignalPayload, req.RequestId)
		writeOK(w, err, wf)
	case len(sub) == 1 && r.Method == http.MethodGet:
		runId := r.URL.Query().Get("execution.runId")
		if runId == "" {
			runId = r.URL.Query().Get("runId")
		}
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
		var req struct {
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		_ = decode(r, &req)
		wf, err := en.CancelWorkflowWithReason(ns, sub[0], r.URL.Query().Get("runId"), req.Reason, req.Identity)
		writeOK(w, err, wf)
	case len(sub) == 2 && sub[1] == "terminate" && r.Method == http.MethodPost:
		var req struct {
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		_ = decode(r, &req)
		wf, err := en.TerminateWorkflowWithReason(ns, sub[0], r.URL.Query().Get("runId"), req.Reason, req.Identity)
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
		runId := r.URL.Query().Get("runId")
		afterID := parseInt64(r.URL.Query().Get("after"))
		pageSize := int(parseInt64(r.URL.Query().Get("pageSize")))
		reverse := r.URL.Query().Get("reverse") == "true"
		events, next, err := en.GetWorkflowHistory(ns, sub[0], runId, afterID, pageSize, reverse)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		writeOK(w, nil, map[string]any{"events": events, "nextCursor": next})
	case len(sub) == 2 && sub[1] == "query" && r.Method == http.MethodPost:
		var req struct {
			QueryType string `json:"queryType"`
			Args      any    `json:"args"`
		}
		_ = decode(r, &req)
		runId := r.URL.Query().Get("runId")
		out, err := en.QueryWorkflowCtx(r.Context(), ns, sub[0], runId, req.QueryType, req.Args)
		if err == ErrNoWorkersSubscribed {
			writeErr(w, 503, err.Error())
			return
		}
		if err != nil && strings.Contains(err.Error(), "timeout") {
			writeErr(w, 504, err.Error())
			return
		}
		writeOK(w, err, map[string]any{"queryResult": out})
	case len(sub) == 2 && sub[1] == "metadata" && r.Method == http.MethodPost:
		var req WorkflowUserMetadata
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		runId := r.URL.Query().Get("runId")
		wf, err := en.UpdateWorkflowMetadata(ns, sub[0], runId, req)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		writeOK(w, nil, wf)
	case len(sub) == 2 && sub[1] == "reset" && r.Method == http.MethodPost:
		var req struct {
			RunId    string `json:"runId"`
			EventId  int64  `json:"eventId"`
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		wf, err := en.ResetWorkflow(ns, sub[0], req.RunId, req.EventId, req.Reason, req.Identity)
		writeOK(w, err, wf)
	default:
		http.NotFound(w, r)
	}
}

func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
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
	case len(sub) == 2 && sub[1] == "terminate" && r.Method == http.MethodPost:
		var req struct {
			Reason   string `json:"reason"`
			Identity string `json:"identity"`
		}
		_ = decode(r, &req)
		b, err := en.TerminateBatch(ns, sub[0], req.Reason, req.Identity)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		writeOK(w, nil, b)
	default:
		http.NotFound(w, r)
	}
}

// handleSearchAttributes — POST adds, GET lists.
func handleSearchAttributes(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	if len(sub) != 0 {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := en.ListSearchAttributes(ns)
		writeOK(w, err, map[string]any{"searchAttributes": rows})
	case http.MethodPost:
		var req SearchAttribute
		if err := decode(r, &req); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if err := en.AddSearchAttribute(ns, req); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				writeErr(w, 409, err.Error())
				return
			}
			if strings.Contains(err.Error(), "not registered") {
				writeErr(w, 404, err.Error())
				return
			}
			writeErr(w, 400, err.Error())
			return
		}
		writeOK(w, nil, req)
	default:
		http.NotFound(w, r)
	}
}

// handleNamespaceMetadata — POST patches namespace metadata.
func handleNamespaceMetadata(w http.ResponseWriter, r *http.Request, en *engine, ns string, sub []string) {
	if len(sub) != 0 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req NamespaceMetadataPatch
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	n, err := en.UpdateNamespaceMetadata(ns, req)
	if err != nil {
		if strings.Contains(err.Error(), "not registered") {
			writeErr(w, 404, err.Error())
			return
		}
		writeErr(w, 400, err.Error())
		return
	}
	writeOK(w, nil, n)
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
	opGetWorkflowHistory      uint16 = 0x0066
	opQueryWorkflow           uint16 = 0x0067
	opResetWorkflow           uint16 = 0x0068
	opSignalWithStartWorkflow uint16 = 0x0069
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
	// wrapPeer exposes the caller's peerID to the handler so subscription
	// state can be keyed off it. Used by Subscribe / Schedule / Respond.
	wrapPeer := func(fn func(from string, req map[string]any) (any, uint32, string)) zap.Handler {
		return func(_ context.Context, from string, msg *zap.Message) (*zap.Message, error) {
			req := map[string]any{}
			if msg != nil {
				root := msg.Root()
				if !root.IsNull() {
					if rb := root.Bytes(envelopeBody); len(rb) > 0 {
						_ = json.Unmarshal(rb, &req)
					}
				}
			}
			out, status, errMsg := fn(from, req)
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
			reason := strOr(req, "reason", "")
			identity := strOr(req, "identity", "")
			wf, err := en.TerminateWorkflowWithReason(ns, wfID, runID, reason, identity)
			if err != nil {
				return nil, 400, err.Error()
			}
			return wfInfoSDK(wf), 200, ""
		}),
		opGetWorkflowHistory: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			after := int64Field(req, "after_event_id")
			page := int(int64Field(req, "page_size"))
			reverse := false
			if v, ok := req["reverse"].(bool); ok {
				reverse = v
			}
			events, next, err := en.GetWorkflowHistory(ns, wfID, runID, after, page, reverse)
			if err != nil {
				return nil, 404, err.Error()
			}
			return map[string]any{"events": events, "next_cursor": next}, 200, ""
		}),
		opQueryWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			qType := strOr(req, "query_type", str(req, "queryType"))
			out, err := en.QueryWorkflow(ns, wfID, runID, qType, req["args"])
			if err != nil {
				return nil, 404, err.Error()
			}
			return map[string]any{"query_result": out}, 200, ""
		}),
		opResetWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			eventID := int64Field(req, "event_id")
			reason := strOr(req, "reason", "")
			identity := strOr(req, "identity", "")
			wf, err := en.ResetWorkflow(ns, wfID, runID, eventID, reason, identity)
			if err != nil {
				return nil, 400, err.Error()
			}
			return wfInfoSDK(wf), 200, ""
		}),
		opSignalWithStartWorkflow: wrap(func(req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			typeName := strOr(req, "workflow_type", "")
			tq := strOr(req, "task_queue", "")
			wfID := strOr(req, "workflow_id", str(req, "workflowId"))
			runID := strOr(req, "run_id", str(req, "runId"))
			sigName := strOr(req, "signal_name", str(req, "signalName"))
			reqID := strOr(req, "request_id", "")
			if sigName == "" {
				return nil, 400, "signal_name required"
			}
			wf, err := en.SignalWithStartWorkflow(ns, wfID, runID, TypeRef{Name: typeName}, tq, req["input"], sigName, req["signal_input"], reqID)
			if err != nil {
				return nil, 400, err.Error()
			}
			return map[string]any{"workflow_id": wf.Execution.WorkflowId, "run_id": wf.Execution.RunId}, 200, ""
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

		// ── subscribe / deliver task plumbing ─────────────────────
		OpcodeSubscribeWorkflowTasks: wrapPeer(func(from string, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			q := strOr(req, "task_queue", str(req, "taskQueue"))
			id, err := en.disp.Subscribe(from, ns, q, kindWorkflow)
			if err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"subscription_id": id}, 200, ""
		}),
		OpcodeSubscribeActivityTasks: wrapPeer(func(from string, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			q := strOr(req, "task_queue", str(req, "taskQueue"))
			id, err := en.disp.Subscribe(from, ns, q, kindActivity)
			if err != nil {
				return nil, 400, err.Error()
			}
			return map[string]string{"subscription_id": id}, 200, ""
		}),
		OpcodeUnsubscribeTasks: wrap(func(req map[string]any) (any, uint32, string) {
			id := strOr(req, "subscription_id", str(req, "subscriptionId"))
			en.disp.Unsubscribe(id)
			return map[string]bool{"ok": true}, 200, ""
		}),

		// ── responses (object-field frames, see client/worker_transport.go) ──
		client.OpcodeRespondWorkflowTaskCompleted: respondWorkflowHandler(en),
		client.OpcodeRespondActivityTaskCompleted: respondActivityCompletedHandler(en),
		client.OpcodeRespondActivityTaskFailed:    respondActivityFailedHandler(en),
		client.OpcodeRecordActivityTaskHeartbeat:  heartbeatHandler(),

		// ── activity scheduling (envelope + JSON) ─────────────────
		client.OpcodeScheduleActivity: wrapPeer(func(from string, req map[string]any) (any, uint32, string) {
			ns := strOr(req, "namespace", defaultNS)
			q := strOr(req, "task_queue", "")
			wfID := strOr(req, "workflow_id", "")
			runID := strOr(req, "run_id", "")
			actType := strOr(req, "activity_type", "")
			if actType == "" {
				return nil, 400, "activity_type required"
			}
			input, _ := decodeBytesField(req, "input")
			startMs := int64Field(req, "start_to_close_ms")
			hbMs := int64Field(req, "heartbeat_ms")
			actID, token := en.disp.ScheduleActivity(from, ns, q, wfID, runID, actType, input, startMs, hbMs)
			return map[string]any{
				"activity_task_id": actID,
				"task_token":       string(token),
			}, 200, ""
		}),
		// 0x006D — child workflows ship in a follow-up.
		client.OpcodeStartChildWorkflow: wrap(func(_ map[string]any) (any, uint32, string) {
			return nil, 501, "start_child_workflow: not yet implemented"
		}),

		// 0x00C4 — worker → server query response.
		OpcodeRespondQuery: wrap(func(req map[string]any) (any, uint32, string) {
			token := strOr(req, "token", "")
			if token == "" {
				return nil, 400, "token required"
			}
			result, _ := decodeBytesField(req, "result")
			errMsg := strOr(req, "error", "")
			if !en.disp.CompleteQuery(token, result, errMsg) {
				return nil, 404, "query token not found"
			}
			return map[string]bool{"ok": true}, 200, ""
		}),
	}
}

// ── object-field handlers for worker Respond / Heartbeat ──────────────

// respondWorkflowHandler decodes the object-field frame the worker
// produces in encodeRespondWorkflowCompleted, completes the workflow
// task, and applies the worker's command list to the engine state.
func respondWorkflowHandler(en *engine) zap.Handler {
	return func(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
		token, commands := decodeRespondFrame(msg, client.FieldCommandsBytes)
		t, ok := en.disp.CompleteWorkflowTask(token)
		if !ok {
			return objectAck(0, "task token not found", 404)
		}
		// Apply commands.
		var env struct {
			Version  int8 `json:"v"`
			Commands []struct {
				Kind           int8   `json:"kind"`
				Result         []byte `json:"result,omitempty"`
				Failure        []byte `json:"failure,omitempty"`
				ActivityTaskID string `json:"activityTaskId,omitempty"`
			} `json:"cmds"`
		}
		if len(commands) > 0 {
			_ = json.Unmarshal(commands, &env)
		}
		for _, c := range env.Commands {
			switch c.Kind {
			case 0: // complete
				_, _ = en.terminalTransition(t.ns, t.workflowID, t.runID, "WORKFLOW_EXECUTION_STATUS_COMPLETED", "workflow.completed", "WORKFLOW_EXECUTION_COMPLETED", map[string]any{"result": string(c.Result)})
			case 1: // fail
				_, _ = en.terminalTransition(t.ns, t.workflowID, t.runID, "WORKFLOW_EXECUTION_STATUS_FAILED", "workflow.failed", "WORKFLOW_EXECUTION_FAILED", map[string]any{"failure": string(c.Failure)})
			case 2: // schedule_activity (already scheduled by 0x006B; no-op)
			case 3: // canceled — worker ack of a CANCELING handshake
				_, _ = en.AckCanceled(t.ns, t.workflowID, t.runID, string(c.Failure), "")
			}
		}
		return objectAck(0, "", 200)
	}
}

func respondActivityCompletedHandler(en *engine) zap.Handler {
	return func(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
		token, result := decodeRespondFrame(msg, client.FieldResultBytes)
		if _, ok := en.disp.CompleteActivityTask(token, result, nil); !ok {
			return objectAck(0, "task token not found", 404)
		}
		return objectAck(0, "", 200)
	}
}

func respondActivityFailedHandler(en *engine) zap.Handler {
	return func(_ context.Context, _ string, msg *zap.Message) (*zap.Message, error) {
		token, failure := decodeRespondFrame(msg, client.FieldFailureBytes)
		if _, ok := en.disp.CompleteActivityTask(token, nil, failure); !ok {
			return objectAck(0, "task token not found", 404)
		}
		return objectAck(0, "", 200)
	}
}

func heartbeatHandler() zap.Handler {
	return func(_ context.Context, _ string, _ *zap.Message) (*zap.Message, error) {
		// v1: never request cancel.
		return objectAck(0, "", 200)
	}
}

// decodeRespondFrame extracts the task_token and the secondary bytes
// field (commands / result / failure / details) from a worker-encoded
// object frame.
func decodeRespondFrame(msg *zap.Message, secondaryField int) (token, secondary []byte) {
	if msg == nil {
		return nil, nil
	}
	root := msg.Root()
	if root.IsNull() {
		return nil, nil
	}
	t := root.Bytes(client.FieldTaskToken)
	s := root.Bytes(secondaryField)
	out1 := make([]byte, len(t))
	copy(out1, t)
	out2 := make([]byte, len(s))
	copy(out2, s)
	return out1, out2
}

// objectAck builds a minimal object-field response for worker Respond
// ops. cancelRequested defaults to 0 (false). status/error are folded
// into a tiny envelope-compatible shape so callers that decode either
// the heartbeat object form or the envelope shape both succeed.
func objectAck(cancelRequested uint8, errMsg string, status uint32) (*zap.Message, error) {
	b := zap.NewBuilder(64)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetUint32(envelopeStatus, status)
	if cancelRequested != 0 {
		obj.SetBytes(client.FieldRespCancelRequested, []byte{cancelRequested})
	}
	if errMsg != "" {
		obj.SetBytes(envelopeError, []byte(errMsg))
	}
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

// decodeBytesField pulls a base64-encoded []byte field out of the
// generic map[string]any decode. Go's default JSON unmarshal yields a
// string for []byte fields; we round-trip via base64 to recover the
// original bytes. Empty / missing → nil, nil.
func decodeBytesField(req map[string]any, key string) ([]byte, error) {
	v, ok := req[key]
	if !ok || v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		// Numbers / objects come through as the marshaled form — re-encode.
		return json.Marshal(v)
	}
	if s == "" {
		return nil, nil
	}
	// Encoded as base64 (Go json default for []byte).
	dec, err := base64DecodeStd(s)
	if err == nil {
		return dec, nil
	}
	// Some callers pass the raw string; fall back to its bytes.
	return []byte(s), nil
}

func base64DecodeStd(s string) ([]byte, error) {
	return base64Std.DecodeString(s)
}

func int64Field(req map[string]any, key string) int64 {
	v, ok := req[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
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
