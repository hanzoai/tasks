package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/converter"
	"github.com/luxfi/zap"
)

// ZAP opcodes owned by this client. 0x0050-0x005F are reserved for the
// legacy pkg/tasks submit/schedule surface; do not reuse. Every new RPC
// must append a fresh opcode — never rebind an existing one.
//
// v1 wire note: request/response bodies are a single bytes field (field
// offset 0) holding a JSON document that matches the corresponding
// struct in schema/tasks.zap. Native ZAP serde replaces JSON in a
// follow-up without changing opcodes.
const (
	opStartWorkflow           uint16 = 0x0060
	opSignalWorkflow          uint16 = 0x0061
	opCancelWorkflow          uint16 = 0x0062
	opTerminateWorkflow       uint16 = 0x0063
	opDescribeWorkflow        uint16 = 0x0064
	opListWorkflows           uint16 = 0x0065
	opSignalWithStartWorkflow uint16 = 0x0066
	opQueryWorkflow           uint16 = 0x0067

	opCreateSchedule   uint16 = 0x0070
	opListSchedules    uint16 = 0x0071
	opDeleteSchedule   uint16 = 0x0072
	opPauseSchedule    uint16 = 0x0073
	opUpdateSchedule   uint16 = 0x0074
	opTriggerSchedule  uint16 = 0x0075
	opDescribeSchedule uint16 = 0x0076

	opRegisterNamespace uint16 = 0x0080
	opDescribeNamespace uint16 = 0x0081
	opListNamespaces    uint16 = 0x0082

	opHealth uint16 = 0x0090
)

// ZAP field offsets for the single-field JSON envelope used by the v1
// wire. Both request and response objects use the same layout:
//
//	field 0: Bytes — JSON-encoded body
//	field 8: Uint32 — status code (0 / 200 success; other = error)
//	field 12: Bytes — error detail (human-readable, only on non-zero status)
const (
	envelopeBody   = 0
	envelopeStatus = 8
	envelopeError  = 12

	envelopeObjectSize = 24
)

// Transport is the request/response abstraction used by Client for
// user-facing RPCs (workflow, schedule, namespace, health) and by
// WorkerTransport for worker poll/respond RPCs. Production code uses
// the default transport (luxfi/zap). Tests inject an in-memory fake.
//
// Call is frame-in / frame-out:
//   - body is the caller's request payload bytes. For user-facing
//     RPCs the body is a plain JSON document that the client wraps
//     in the ZAP envelope declared by envelopeBody/envelopeStatus/
//     envelopeError before invoking the transport. For worker RPCs
//     the body is a self-framed ZAP object emitted by the worker
//     transport's encoders.
//   - opcode is stamped on the ZAP flags of the outgoing frame by
//     the default transport. A caller that already builds a complete
//     ZAP frame (like the worker) can pass the body bytes verbatim —
//     the default transport wraps them when they are not yet framed
//     and forwards them when they are.
//   - The returned []byte is a complete ZAP response frame. Higher
//     levels (clientImpl.roundTrip for user-facing RPCs, the worker
//     transport decoders for worker RPCs) parse the frame with
//     zap.Parse and read the fields they care about.
type Transport interface {
	// Call issues a single request for opcode and returns the
	// response frame.
	Call(ctx context.Context, opcode uint16, body []byte) (respFrame []byte, err error)

	// Close releases transport resources.
	Close() error
}

// Client is the Hanzo Tasks workflow client. The surface mirrors the
// upstream client.Client one-for-one for the RPCs used by base/commerce/ta.
type Client interface {
	// ExecuteWorkflow starts a workflow (by type name when `workflow`
	// is a string, or by reflected name when it is a Go function).
	// Returns a handle whose Get blocks until the workflow terminates.
	ExecuteWorkflow(
		ctx context.Context,
		opts StartWorkflowOptions,
		workflow any,
		args ...any,
	) (WorkflowRun, error)

	// SignalWorkflow posts a signal to a running execution.
	SignalWorkflow(
		ctx context.Context,
		workflowID, runID, signalName string,
		arg any,
	) error

	// SignalWithStartWorkflow atomically delivers a signal to the
	// workflow identified by workflowID, starting it with the
	// supplied opts+workflow+args if no matching execution is
	// running. Opcode 0x0066.
	SignalWithStartWorkflow(
		ctx context.Context,
		workflowID, signalName string,
		signalArg any,
		opts StartWorkflowOptions,
		workflow any,
		workflowArgs ...any,
	) (WorkflowRun, error)

	// GetWorkflow returns a handle to an already-started workflow.
	// No RPC is issued; subsequent Get / GetWithOptions calls on the
	// handle poll DescribeWorkflow to observe terminal state.
	GetWorkflow(ctx context.Context, workflowID, runID string) WorkflowRun

	// QueryWorkflow runs a registered query on a (possibly running)
	// workflow execution and returns the query's encoded result.
	// Opcode 0x0067. Queries are read-only — they do not mutate
	// workflow history.
	QueryWorkflow(
		ctx context.Context,
		workflowID, runID, queryType string,
		args ...any,
	) (converter.EncodedValue, error)

	// CancelWorkflow asks the server to cancel an execution.
	CancelWorkflow(ctx context.Context, workflowID, runID string) error

	// TerminateWorkflow force-stops an execution with a reason.
	TerminateWorkflow(ctx context.Context, workflowID, runID, reason string) error

	// DescribeWorkflow returns the current info record.
	DescribeWorkflow(ctx context.Context, workflowID, runID string) (*WorkflowExecutionInfo, error)

	// ListWorkflows runs a visibility query.
	ListWorkflows(ctx context.Context, query string, pageSize int32, nextPageToken []byte) (*ListWorkflowsResponse, error)

	// RegisterNamespace creates a new namespace on the server.
	RegisterNamespace(ctx context.Context, req *RegisterNamespaceRequest) error
	// DescribeNamespace returns a namespace's info + config.
	DescribeNamespace(ctx context.Context, name string) (*Namespace, error)
	// ListNamespaces pages through registered namespaces.
	ListNamespaces(ctx context.Context, pageSize int32, nextPageToken []byte) (*ListNamespacesResponse, error)

	// CreateSchedule registers a recurring schedule.
	CreateSchedule(ctx context.Context, opts CreateScheduleOptions) error
	// ListSchedules pages through schedules.
	ListSchedules(ctx context.Context, pageSize int32, nextPageToken []byte) (*ListSchedulesResponse, error)
	// DeleteSchedule removes a schedule by ID.
	DeleteSchedule(ctx context.Context, scheduleID string) error
	// PauseSchedule toggles a schedule's paused flag.
	PauseSchedule(ctx context.Context, scheduleID string, paused bool) error
	// UnpauseSchedule resumes a paused schedule.
	UnpauseSchedule(ctx context.Context, scheduleID string) error
	// UpdateSchedule replaces the schedule definition. Partial updates
	// are not supported on the v1 wire.
	UpdateSchedule(ctx context.Context, opts UpdateScheduleOptions) error
	// TriggerSchedule fires the schedule once immediately. overlapPolicy
	// is one of "skip"/"allow"/"buffer-one"; "" defers to the schedule's
	// configured policy.
	TriggerSchedule(ctx context.Context, scheduleID, overlapPolicy string) error
	// DescribeSchedule returns the current definition + runtime info.
	DescribeSchedule(ctx context.Context, scheduleID string) (*DescribeScheduleResponse, error)

	// Health reports server reachability as two flat strings.
	Health(ctx context.Context) (service, status string, err error)

	// CheckHealth is the structured counterpart to Health, matching
	// the upstream client shape so callers migrating from
	// go.temporal.io/sdk/client compile unchanged. A nil req is
	// treated as an empty request. Backed by the same opcode
	// 0x0090 as Health.
	CheckHealth(ctx context.Context, req *CheckHealthRequest) (*CheckHealthResponse, error)

	// Close releases the underlying ZAP connection.
	Close()
}

// CheckHealthRequest is reserved for future filters; empty in v1.
type CheckHealthRequest struct{}

// CheckHealthResponse reports the backend's liveness.
type CheckHealthResponse struct {
	// Service is the opaque backend identifier ("hanzo-tasks").
	Service string `json:"service"`
	// Status is "ok" when the frontend is healthy, anything else on
	// degradation.
	Status string `json:"status"`
}

// ErrClosed is returned by RPCs issued on a Client after Close.
var ErrClosed = errors.New("hanzo/tasks/client: closed")

// ErrNotImplementedLocally marks RPCs that need a Worker-side history
// fetch RPC (currently missing from schema/tasks.zap). Callers see this
// when they ask a WorkflowRun for its result but the server has not
// exposed the history-fetch opcode yet.
var ErrNotImplementedLocally = errors.New("hanzo/tasks/client: RPC requires pkg/sdk/worker history fetch (not yet wired)")

// Dial connects to a Hanzo Tasks frontend. Callers must Close() when done.
func Dial(opts Options) (Client, error) {
	ns := opts.Namespace
	if ns == "" {
		ns = "default"
	}
	id := opts.Identity
	if id == "" {
		id = "hanzo-tasks-sdk"
	}

	tr := opts.Transport
	if tr == nil {
		if opts.HostPort == "" {
			return nil, errors.New("hanzo/tasks/client: Options.HostPort is required when no Transport is injected")
		}
		zt, err := newZAPTransport(opts.HostPort, opts.DialTimeout)
		if err != nil {
			return nil, err
		}
		tr = zt
	}

	return &clientImpl{
		transport:   tr,
		namespace:   ns,
		identity:    id,
		callTimeout: opts.CallTimeout,
	}, nil
}

// clientImpl is the concrete Client wired to a Transport.
type clientImpl struct {
	transport   Transport
	namespace   string
	identity    string
	callTimeout time.Duration

	closeOnce sync.Once
	closed    bool
	mu        sync.RWMutex
}

// Close implements Client.
func (c *clientImpl) Close() {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		if c.transport != nil {
			_ = c.transport.Close()
		}
	})
}

// roundTrip performs a JSON-over-ZAP round trip for a single opcode.
// req is marshalled to JSON and stuffed into the envelope; the response
// body JSON is decoded into resp (if non-nil). Any non-zero server
// status becomes a Go error with the detail appended.
func (c *clientImpl) roundTrip(ctx context.Context, opcode uint16, req, resp any) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return ErrClosed
	}
	c.mu.RUnlock()

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	if c.callTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callTimeout)
		defer cancel()
	}

	respFrame, err := c.transport.Call(ctx, opcode, body)
	if err != nil {
		return fmt.Errorf("zap call 0x%04x: %w", opcode, err)
	}

	status, detail, payload, err := decodeEnvelope(respFrame)
	if err != nil {
		return fmt.Errorf("decode response 0x%04x: %w", opcode, err)
	}
	if status != 0 && status != 200 {
		return fmt.Errorf("hanzo/tasks: server status %d: %s", status, detail)
	}

	if resp != nil && len(payload) > 0 {
		if err := json.Unmarshal(payload, resp); err != nil {
			return fmt.Errorf("decode response body 0x%04x: %w", opcode, err)
		}
	}
	return nil
}

// encodeEnvelope wraps a JSON body in the single-field ZAP envelope used
// by the v1 wire and flags the message with the target opcode.
func encodeEnvelope(opcode uint16, body []byte) ([]byte, error) {
	b := zap.NewBuilder(len(body) + envelopeObjectSize + 32)
	obj := b.StartObject(envelopeObjectSize)
	obj.SetBytes(envelopeBody, body)
	obj.FinishAsRoot()
	flags := uint16(opcode) << 8
	return b.FinishWithFlags(flags), nil
}

// decodeEnvelope pulls the status / error / body out of a response frame.
func decodeEnvelope(frame []byte) (status uint32, detail string, body []byte, err error) {
	msg, perr := zap.Parse(frame)
	if perr != nil {
		return 0, "", nil, perr
	}
	root := msg.Root()
	status = root.Uint32(envelopeStatus)
	detail = string(root.Bytes(envelopeError))
	body = root.Bytes(envelopeBody)
	return status, detail, body, nil
}

// zapTransport is the production Transport backed by luxfi/zap.
type zapTransport struct {
	node *zap.Node
	peer string
}

func newZAPTransport(addr string, dialTimeout time.Duration) (*zapTransport, error) {
	node := zap.NewNode(zap.NodeConfig{
		NodeID:      "hanzo-tasks-sdk",
		ServiceType: "_tasks._tcp",
		Port:        0,
		Logger:      slog.Default(),
		NoDiscovery: true,
	})
	if err := node.Start(); err != nil {
		return nil, fmt.Errorf("zap start: %w", err)
	}
	if err := node.ConnectDirect(addr); err != nil {
		node.Stop()
		return nil, fmt.Errorf("zap connect %s: %w", addr, err)
	}
	peer := addr
	if peers := node.Peers(); len(peers) > 0 {
		peer = peers[0]
	}
	_ = dialTimeout // reserved for future HandshakeDeadline wiring
	return &zapTransport{node: node, peer: peer}, nil
}

// zapMagic is the ZAP frame magic ("ZAP\x00"). Used to detect whether
// the body handed to Call is already a complete frame (worker path) or
// a raw JSON body that needs to be wrapped in the client envelope
// (user-facing RPC path).
var zapMagic = []byte{'Z', 'A', 'P', 0}

func (t *zapTransport) Call(ctx context.Context, opcode uint16, body []byte) ([]byte, error) {
	frame, err := FrameBody(opcode, body)
	if err != nil {
		return nil, err
	}
	msg, err := zap.Parse(frame)
	if err != nil {
		return nil, err
	}
	resp, err := t.node.Call(ctx, t.peer, msg)
	if err != nil {
		return nil, err
	}
	// Return an independent copy of the response frame — the
	// transport's receive buffer must not leak across goroutines.
	frameBytes := resp.Bytes()
	out := make([]byte, len(frameBytes))
	copy(out, frameBytes)
	return out, nil
}

// FrameBody returns a complete ZAP frame with opcode in the high byte
// of the flags. If body is already a framed ZAP object (worker path),
// FrameBody re-stamps the flags to the given opcode. If body is raw
// payload bytes (user-facing RPC path), FrameBody wraps body in the
// client envelope declared by envelopeBody / envelopeStatus /
// envelopeError.
//
// Exported so that pkg/sdk/inproc can produce the same on-the-wire
// shape as the network transport before handing the parsed message to
// the frontend's dispatch table.
func FrameBody(opcode uint16, body []byte) ([]byte, error) {
	if len(body) >= 8 && string(body[:len(zapMagic)]) == string(zapMagic) {
		// Already a ZAP frame. Copy and re-stamp the flags so the
		// opcode the caller passed is authoritative.
		out := make([]byte, len(body))
		copy(out, body)
		// Layout (little-endian): Magic[0..3] Version[4..5] Flags[6..7].
		flags := uint16(opcode) << 8
		out[6] = byte(flags & 0xFF)
		out[7] = byte(flags >> 8)
		return out, nil
	}
	return encodeEnvelope(opcode, body)
}

func (t *zapTransport) Close() error {
	if t.node != nil {
		t.node.Stop()
	}
	return nil
}
