// Copyright © 2026 Hanzo AI. MIT License.

// MCP — Model Context Protocol surface for Hanzo Tasks.
//
// Exposes workflow + schedule + namespace operations as MCP tools so
// agents can drive Tasks the same way humans drive the UI. The wire
// is JSON-RPC 2.0 over HTTP (single endpoint, POST), mirroring the
// canonical MCP spec at https://modelcontextprotocol.io.
//
// Transport choice: HTTP for MCP clients today (every MCP runtime
// speaks HTTP/SSE). The same tool surface is reachable over the
// canonical ZAP wire via opMCPInvoke (TBD opcode 0x00B0) once we
// add native MCP framing to the SDK.

package tasks

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// mcpHandler returns a single endpoint that accepts JSON-RPC 2.0 calls
// against the MCP method surface. Mounts at /v1/tasks/mcp.
func (e *Embedded) mcpHandler() http.Handler {
	if e == nil {
		return http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			mcpError(w, nil, -32700, "parse error: "+err.Error())
			return
		}
		switch req.Method {
		case "initialize":
			mcpResult(w, req.ID, map[string]any{
				"protocolVersion": "2025-06-18",
				"serverInfo": map[string]any{
					"name":    "hanzo-tasks",
					"version": "1.40.0",
				},
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
			})
		case "tools/list":
			mcpResult(w, req.ID, map[string]any{"tools": mcpTools()})
		case "tools/call":
			e.mcpToolCall(w, req)
		case "ping":
			mcpResult(w, req.ID, map[string]any{})
		default:
			mcpError(w, req.ID, -32601, "method not found: "+req.Method)
		}
	})
}

// mcpTools is the static MCP tool catalogue. Each tool maps onto an
// engine method; arg shapes follow the JSON Schema MCP spec expects.
func mcpTools() []map[string]any {
	str := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	obj := func(desc string) map[string]any {
		return map[string]any{"type": "object", "description": desc, "additionalProperties": true}
	}
	tool := func(name, desc string, props map[string]any, required ...string) map[string]any {
		return map[string]any{
			"name":        name,
			"description": desc,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		}
	}
	return []map[string]any{
		tool("list_namespaces", "List every registered namespace.", map[string]any{}),
		tool("describe_namespace", "Describe a namespace by name.", map[string]any{
			"name": str("Namespace name."),
		}, "name"),
		tool("list_workflows", "List workflow executions in a namespace.", map[string]any{
			"namespace": str("Namespace name."),
		}, "namespace"),
		tool("describe_workflow", "Describe a workflow execution.", map[string]any{
			"namespace":   str("Namespace name."),
			"workflow_id": str("Workflow ID."),
			"run_id":      str("Run ID; if empty, latest run is used."),
		}, "namespace", "workflow_id"),
		tool("start_workflow", "Start a new workflow execution.", map[string]any{
			"namespace":     str("Namespace name."),
			"workflow_id":   str("Workflow ID; if empty, server generates one."),
			"workflow_type": str("Workflow type (Go function name or registered name)."),
			"task_queue":    str("Task queue name; defaults to \"default\"."),
			"input":         obj("Workflow input — arbitrary JSON."),
		}, "namespace", "workflow_type"),
		tool("signal_workflow", "Send a signal to a running workflow.", map[string]any{
			"namespace":   str("Namespace name."),
			"workflow_id": str("Workflow ID."),
			"run_id":      str("Run ID; empty for latest."),
			"signal_name": str("Signal name."),
			"input":       obj("Signal payload — arbitrary JSON."),
		}, "namespace", "workflow_id", "signal_name"),
		tool("cancel_workflow", "Cancel a running workflow.", map[string]any{
			"namespace":   str("Namespace name."),
			"workflow_id": str("Workflow ID."),
			"run_id":      str("Run ID; empty for latest."),
		}, "namespace", "workflow_id"),
		tool("terminate_workflow", "Terminate a running workflow with prejudice.", map[string]any{
			"namespace":   str("Namespace name."),
			"workflow_id": str("Workflow ID."),
			"run_id":      str("Run ID; empty for latest."),
		}, "namespace", "workflow_id"),
		tool("list_schedules", "List schedules in a namespace.", map[string]any{
			"namespace": str("Namespace name."),
		}, "namespace"),
		tool("create_schedule", "Create a cron-or-interval schedule.", map[string]any{
			"namespace":     str("Namespace name."),
			"schedule_id":   str("Schedule ID."),
			"cron":          str("Cron expression, e.g. \"0 3 * * *\"."),
			"workflow_type": str("Workflow type to start on each tick."),
			"task_queue":    str("Task queue."),
		}, "namespace", "schedule_id", "workflow_type"),
		tool("pause_schedule", "Pause or unpause a schedule.", map[string]any{
			"namespace":   str("Namespace name."),
			"schedule_id": str("Schedule ID."),
			"paused":      map[string]any{"type": "boolean", "description": "true to pause, false to resume."},
		}, "namespace", "schedule_id", "paused"),
	}
}

func (e *Embedded) mcpToolCall(w http.ResponseWriter, req mcpRequest) {
	en := e.engine
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	body, _ := json.Marshal(req.Params)
	_ = json.Unmarshal(body, &p)
	a := p.Arguments
	str := func(k string) string {
		if v, ok := a[k].(string); ok {
			return v
		}
		return ""
	}

	emit := func(v any) {
		text, _ := json.Marshal(v)
		mcpResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(text)}},
		})
	}
	fail := func(code int, msg string) {
		mcpError(w, req.ID, code, msg)
	}

	switch p.Name {
	case "list_namespaces":
		rows, err := en.ListNamespaces()
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(map[string]any{"namespaces": rows})
	case "describe_namespace":
		n, ok, err := en.DescribeNamespace(str("name"))
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		if !ok {
			fail(-32004, "namespace not found")
			return
		}
		emit(n)
	case "list_workflows":
		rows, err := en.ListWorkflows(str("namespace"))
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(map[string]any{"executions": rows})
	case "describe_workflow":
		wf, ok, err := en.DescribeWorkflow(str("namespace"), str("workflow_id"), str("run_id"))
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		if !ok {
			fail(-32004, "workflow not found")
			return
		}
		emit(wf)
	case "start_workflow":
		typ := TypeRef{Name: str("workflow_type")}
		wf, err := en.StartWorkflow(str("namespace"), str("workflow_id"), str("run_id"), typ, str("task_queue"), a["input"])
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(wf)
	case "signal_workflow":
		if err := en.SignalWorkflow(str("namespace"), str("workflow_id"), str("run_id"), str("signal_name"), a["input"]); err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(map[string]string{"status": "signaled"})
	case "cancel_workflow":
		wf, err := en.CancelWorkflow(str("namespace"), str("workflow_id"), str("run_id"))
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(wf)
	case "terminate_workflow":
		wf, err := en.TerminateWorkflow(str("namespace"), str("workflow_id"), str("run_id"))
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(wf)
	case "list_schedules":
		rows, err := en.ListSchedules(str("namespace"))
		if err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(map[string]any{"schedules": rows})
	case "create_schedule":
		s := Schedule{
			ScheduleId: str("schedule_id"),
			Namespace:  str("namespace"),
			Spec:       ScheduleSpec{CronString: []string{str("cron")}},
			Action: ScheduleAction{
				WorkflowType: TypeRef{Name: str("workflow_type")},
				TaskQueue:    str("task_queue"),
			},
		}
		if err := en.CreateSchedule(s); err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(s)
	case "pause_schedule":
		paused, _ := a["paused"].(bool)
		if err := en.PauseSchedule(str("namespace"), str("schedule_id"), paused, ""); err != nil {
			fail(-32000, err.Error())
			return
		}
		emit(map[string]any{"paused": paused})
	default:
		fail(-32601, "unknown tool: "+p.Name)
	}
}

// mcpRequest is the JSON-RPC 2.0 request envelope.
type mcpRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func mcpResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

func mcpError(w http.ResponseWriter, id any, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	})
}

// mcpUnused silences the linter for response-shape constants.
func mcpUnused() string { return fmt.Sprintf("%v", "") }
