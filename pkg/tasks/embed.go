// Copyright © 2026 Hanzo AI. MIT License.

// Embed runs the in-process Hanzo Tasks server: a luxfi/zap node that
// answers the SDK opcodes defined in pkg/sdk/client. Workflow execution
// semantics are not yet wired — handlers return a `not_implemented`
// envelope. The shape exists so callers (cmd/tasksd, base) can depend
// on the API while the native engine is built behind it.
//
// No gRPC. No go.temporal.io. No upstream protobuf. Just ZAP + stdlib.

package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/luxfi/zap"
)

// EmbedConfig configures the in-process Tasks server.
type EmbedConfig struct {
	// DataDir holds the workflow + task persistence (SQLite by default).
	// "" → "./tasks-data".
	DataDir string

	// ZAPPort is the _tasks._tcp listener. 0 picks an ephemeral port;
	// the chosen port is reported by Embedded.ZAPPort().
	ZAPPort int

	// Namespace is the default namespace registered on first boot.
	// "" → "default".
	Namespace string

	// Logger receives slog records. nil → slog.Default().
	Logger *slog.Logger
}

// Embedded is the handle to a running in-process Tasks server.
type Embedded struct {
	cfg  EmbedConfig
	node *zap.Node
}

// Embed starts the in-process Tasks server with cfg. The caller MUST
// call Stop before exit to release the listener cleanly.
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

	for _, op := range stubOpcodes {
		node.Handle(op, notImplemented(op))
	}
	node.Handle(opHealth, healthHandler(cfg.Namespace))

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

// Opcodes mirror pkg/sdk/client (canonical allocation lives there;
// duplicated here only as a server-side dispatch table). Append-only —
// never rebind.
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

var stubOpcodes = []uint16{
	opStartWorkflow, opSignalWorkflow, opCancelWorkflow, opTerminateWorkflow,
	opDescribeWorkflow, opListWorkflows, opSignalWithStartWorkflow, opQueryWorkflow,
	opCreateSchedule, opDeleteSchedule, opListSchedules, opPauseSchedule,
	opUnpauseSchedule, opUpdateSchedule, opDescribeSchedule, opTriggerSchedule,
	opRegisterNamespace, opDescribeNamespace, opListNamespaces,
}

const (
	envelopeBody       = 0
	envelopeStatus     = 8
	envelopeError      = 12
	envelopeObjectSize = 24
)

func notImplemented(op uint16) zap.Handler {
	return func(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
		return envelope(nil, 501, fmt.Sprintf("opcode 0x%04X: not yet implemented in native server", op))
	}
}

func healthHandler(namespace string) zap.Handler {
	body, _ := json.Marshal(map[string]any{
		"service":   "tasks",
		"status":    "ok",
		"namespace": namespace,
	})
	return func(ctx context.Context, from string, msg *zap.Message) (*zap.Message, error) {
		return envelope(body, 200, "")
	}
}

// envelope builds a {body, status, error} ZAP response frame and parses
// it back into a *zap.Message — what zap.Handler must return.
func envelope(body []byte, status uint32, errMsg string) (*zap.Message, error) {
	b := zap.NewBuilder(envelopeObjectSize + len(body) + len(errMsg) + 64)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.SetUint32(envelopeStatus, status)
	obj.SetBytes(envelopeError, []byte(errMsg))
	obj.FinishAsRoot()
	return zap.Parse(b.Finish())
}
