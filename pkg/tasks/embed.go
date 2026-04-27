// Copyright © 2026 Hanzo AI. MIT License.

// Embed runs the in-process Hanzo Tasks server. One backend, two
// transports: ZAP on :9999 (canonical, native, binary) and HTTP/JSON
// (browser-only, for the embedded UI). Both speak the same opcode
// surface and call the same model functions — there is exactly one
// way to answer each question. No gRPC. No go.temporal.io. No
// upstream protobuf. Just luxfi/zap + stdlib.
//
// Workflow execution semantics are not yet wired — list/describe ops
// return the empty/default state. The shape exists so the UI is
// navigable and SDK callers can depend on the API while the native
// engine is built behind it.

package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/luxfi/zap"
)

// EmbedConfig configures the in-process Tasks server.
type EmbedConfig struct {
	DataDir   string       // workflow + task persistence dir; "" → "./tasks-data"
	ZAPPort   int          // _tasks._tcp listener; 0 picks ephemeral
	Namespace string       // default namespace; "" → "default"
	Logger    *slog.Logger // nil → slog.Default()
}

// Embedded is the handle to a running in-process Tasks server.
type Embedded struct {
	cfg  EmbedConfig
	node *zap.Node
}

// Embed starts the in-process Tasks server. Stop before exit.
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

	node := zap.NewNode(zap.NodeConfig{
		NodeID:      "tasks-embed",
		ServiceType: "_tasks._tcp",
		Port:        cfg.ZAPPort,
		Logger:      cfg.Logger,
		NoDiscovery: true,
	})

	// ZAP handlers map opcode → model function via wrap().
	for op, model := range opModels(cfg.Namespace) {
		node.Handle(op, wrap(model))
	}
	node.Handle(opHealth, wrap(healthModel(cfg.Namespace)))

	if err := node.Start(); err != nil {
		return nil, fmt.Errorf("tasks.Embed: zap start: %w", err)
	}
	return &Embedded{cfg: cfg, node: node}, nil
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
	if e == nil || e.node == nil {
		return nil
	}
	e.node.Stop()
	e.node = nil
	_ = ctx
	return nil
}

// HTTPHandler returns the browser-only JSON shim. Each route maps to
// the same model function the ZAP handler uses, so the two transports
// can never drift.
func (e *Embedded) HTTPHandler() http.Handler {
	if e == nil {
		return http.NotFoundHandler()
	}
	ns := e.cfg.Namespace
	mux := http.NewServeMux()

	serve := func(w http.ResponseWriter, m model) {
		body, status, errMsg := m()
		writeJSON(w, body, status, errMsg)
	}
	// GET /v1/tasks/namespaces[?pageSize=...]
	mux.HandleFunc("/v1/tasks/namespaces", func(w http.ResponseWriter, r *http.Request) {
		serve(w, listNamespacesModel(ns))
	})
	// GET /v1/tasks/namespaces/{ns}/workflows
	// GET /v1/tasks/namespaces/{ns}/schedules
	// GET /v1/tasks/namespaces/{ns}/workflows/{id}
	mux.HandleFunc("/v1/tasks/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/v1/tasks/namespaces/")
		parts := strings.SplitN(rest, "/", 3)
		switch {
		case len(parts) == 2 && parts[1] == "workflows":
			serve(w, listWorkflowsModel(parts[0]))
		case len(parts) == 2 && parts[1] == "schedules":
			serve(w, listSchedulesModel(parts[0]))
		case len(parts) == 3 && parts[1] == "workflows":
			serve(w, describeWorkflowModel(parts[0], parts[2]))
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

// ── opcode allocation (mirror of pkg/sdk/client) ────────────────────
//
// Append-only; never rebind a value. Canonical home is pkg/sdk/client.

const (
	opStartWorkflow           uint16 = 0x0060
	opSignalWorkflow          uint16 = 0x0061
	opCancelWorkflow          uint16 = 0x0062
	opTerminateWorkflow       uint16 = 0x0063
	opDescribeWorkflow        uint16 = 0x0064
	opListWorkflows           uint16 = 0x0065
	opSignalWithStartWorkflow uint16 = 0x0066
	opQueryWorkflow           uint16 = 0x0067
	opCreateSchedule          uint16 = 0x0070
	opDeleteSchedule          uint16 = 0x0071
	opListSchedules           uint16 = 0x0072
	opPauseSchedule           uint16 = 0x0073
	opUnpauseSchedule         uint16 = 0x0074
	opUpdateSchedule          uint16 = 0x0075
	opDescribeSchedule        uint16 = 0x0076
	opTriggerSchedule         uint16 = 0x0077
	opRegisterNamespace       uint16 = 0x0080
	opDescribeNamespace       uint16 = 0x0081
	opListNamespaces          uint16 = 0x0082
	opHealth                  uint16 = 0x0090
)

// ── envelope ────────────────────────────────────────────────────────
//
// {body, status, error} — the only ZAP response shape on this wire.

const (
	envelopeBody       = 0
	envelopeStatus     = 8
	envelopeError      = 12
	envelopeObjectSize = 24
)

// model returns (body, status, errMsg). status=200 means success;
// other codes carry a human-readable errMsg.
type model func() (body []byte, status uint32, errMsg string)

// wrap turns a model into a zap.Handler.
func wrap(m model) zap.Handler {
	return func(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
		body, status, errMsg := m()
		return envelope(body, status, errMsg)
	}
}

func envelope(body []byte, status uint32, errMsg string) (*zap.Message, error) {
	b := zap.NewBuilder(envelopeObjectSize + len(body) + len(errMsg) + 64)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, status)
	obj.SetBytes(envelopeError, []byte(errMsg))
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}

// ── models ──────────────────────────────────────────────────────────
//
// Each model returns the same JSON the ZAP envelope and the HTTP shim
// emit. Today they return the empty/default state; the native engine
// will replace each one in place without changing the wire shape.

func opModels(namespace string) map[uint16]model {
	notImpl := func(op uint16) model {
		return func() ([]byte, uint32, string) {
			return nil, 501, fmt.Sprintf("opcode 0x%04X: not yet implemented", op)
		}
	}
	return map[uint16]model{
		opStartWorkflow:           notImpl(opStartWorkflow),
		opSignalWorkflow:          notImpl(opSignalWorkflow),
		opCancelWorkflow:          notImpl(opCancelWorkflow),
		opTerminateWorkflow:       notImpl(opTerminateWorkflow),
		opDescribeWorkflow:        describeWorkflowModel(namespace, ""),
		opListWorkflows:           listWorkflowsModel(namespace),
		opSignalWithStartWorkflow: notImpl(opSignalWithStartWorkflow),
		opQueryWorkflow:           notImpl(opQueryWorkflow),
		opCreateSchedule:          notImpl(opCreateSchedule),
		opDeleteSchedule:          notImpl(opDeleteSchedule),
		opListSchedules:           listSchedulesModel(namespace),
		opPauseSchedule:           notImpl(opPauseSchedule),
		opUnpauseSchedule:         notImpl(opUnpauseSchedule),
		opUpdateSchedule:          notImpl(opUpdateSchedule),
		opDescribeSchedule:        notImpl(opDescribeSchedule),
		opTriggerSchedule:         notImpl(opTriggerSchedule),
		opRegisterNamespace:       notImpl(opRegisterNamespace),
		opDescribeNamespace:       describeNamespaceModel(namespace),
		opListNamespaces:          listNamespacesModel(namespace),
	}
}

func listNamespacesModel(defaultNS string) model {
	return func() ([]byte, uint32, string) {
		body, _ := json.Marshal(map[string]any{
			"namespaces": []map[string]any{{
				"namespaceInfo": map[string]any{
					"name":  defaultNS,
					"state": "NAMESPACE_STATE_REGISTERED",
				},
				"config": map[string]any{
					"workflowExecutionRetentionTtl": "720h",
				},
			}},
		})
		return body, 200, ""
	}
}

func describeNamespaceModel(ns string) model {
	return func() ([]byte, uint32, string) {
		body, _ := json.Marshal(map[string]any{
			"namespaceInfo": map[string]any{
				"name":  ns,
				"state": "NAMESPACE_STATE_REGISTERED",
			},
		})
		return body, 200, ""
	}
}

func listWorkflowsModel(_ string) model {
	return func() ([]byte, uint32, string) {
		body, _ := json.Marshal(map[string]any{"executions": []any{}})
		return body, 200, ""
	}
}

func listSchedulesModel(_ string) model {
	return func() ([]byte, uint32, string) {
		body, _ := json.Marshal(map[string]any{"schedules": []any{}})
		return body, 200, ""
	}
}

func describeWorkflowModel(_, _ string) model {
	return func() ([]byte, uint32, string) {
		return nil, 404, "workflow not found"
	}
}

func healthModel(ns string) model {
	body, _ := json.Marshal(map[string]any{
		"service":   "tasks",
		"status":    "ok",
		"namespace": ns,
	})
	return func() ([]byte, uint32, string) {
		return body, 200, ""
	}
}

// ── http helpers ────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, body []byte, status uint32, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	if status == 0 {
		status = 200
	}
	if status >= 400 {
		w.WriteHeader(int(status))
		_ = json.NewEncoder(w).Encode(map[string]any{"error": errMsg, "code": status})
		return
	}
	w.WriteHeader(int(status))
	_, _ = w.Write(body)
}
