// Copyright © 2026 Hanzo AI. MIT License.

// Package inproc provides an in-process [client.Transport] that
// dispatches RPCs synchronously into a frontend ZAP handler. It is the
// canonical seam for callers that live inside the same Go binary as
// the frontend (history's frontend self-dial, system workers,
// temporaltest, embedded boot tests). Compared to the loopback ZAP
// path it skips:
//
//   - the network listener bind on :9999
//   - the ZAP node start / discovery handshake
//   - the encode → frame → decode round trip
//
// The Dispatcher interface is satisfied by *frontend.ZAPHandler. Tests
// can supply any opcode → handler shim that matches the same shape.
//
// Wire compatibility with the network path is total: every opcode
// reaches the same handler function, so a workflow lifecycle exercised
// over inproc and one exercised over ZAP-on-loopback see identical
// server behavior. There is no semantic divergence and no opt-out
// flag.
package inproc

import (
	"context"
	"errors"
	"fmt"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/luxfi/zap"
)

// Dispatcher is the subset of *frontend.ZAPHandler that inproc.Transport
// requires. Keeping it small avoids a hard import cycle and lets tests
// inject lightweight fakes without standing up the real handler.
type Dispatcher interface {
	// Dispatch invokes the handler registered for opcode synchronously
	// and returns the response *zap.Message. Errors propagate raw.
	Dispatch(ctx context.Context, opcode uint16, msg *zap.Message) (*zap.Message, error)
}

// Transport implements [client.Transport] by parsing the caller's
// frame, looking up the handler in the dispatcher, and emitting the
// returned message's bytes. Closed transports return ErrClosed for
// every subsequent Call.
type Transport struct {
	dispatcher Dispatcher
	closed     bool
}

// ErrClosed is returned when Call is invoked after Close.
var ErrClosed = errors.New("hanzo/tasks/inproc: transport closed")

// NewTransport constructs a Transport that routes every Call through
// dispatcher. dispatcher must remain valid for the lifetime of the
// transport — Close on the transport does not Stop the dispatcher.
func NewTransport(dispatcher Dispatcher) *Transport {
	return &Transport{dispatcher: dispatcher}
}

// Call satisfies [client.Transport]. It accepts the same frame layout
// the network transport produces (a complete ZAP frame) or a raw
// envelope body that needs wrapping. Either way it ends up calling
// the dispatcher with a parsed *zap.Message.
//
// The returned bytes are an independent copy of the response frame,
// matching the network transport's contract that the response buffer
// does not alias internal state.
func (t *Transport) Call(ctx context.Context, opcode uint16, body []byte) ([]byte, error) {
	if t.closed {
		return nil, ErrClosed
	}
	if t.dispatcher == nil {
		return nil, errors.New("hanzo/tasks/inproc: nil dispatcher")
	}

	frame, err := client.FrameBody(opcode, body)
	if err != nil {
		return nil, fmt.Errorf("frame body: %w", err)
	}
	msg, err := zap.Parse(frame)
	if err != nil {
		return nil, fmt.Errorf("parse request: %w", err)
	}

	resp, err := t.dispatcher.Dispatch(ctx, opcode, msg)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("hanzo/tasks/inproc: handler 0x%04x returned nil response", opcode)
	}

	src := resp.Bytes()
	out := make([]byte, len(src))
	copy(out, src)
	return out, nil
}

// Handle satisfies client.Transport. The inproc transport is request /
// response only — there is no listener for the server to push to.
// Callers that need server-pushed deliveries (worker tasks) must use
// the network ZAP transport. Calling Handle is a no-op so the inproc
// path satisfies the Transport interface without surprising callers.
func (t *Transport) Handle(opcode uint16, fn func(from string, body []byte)) {
	// Intentionally empty — see doc comment.
	_ = opcode
	_ = fn
}

// Close marks the transport closed. Subsequent Call invocations return
// ErrClosed. Idempotent — repeated Close is a no-op. Does not stop the
// underlying dispatcher; the dispatcher's lifetime is owned by whoever
// constructed it.
func (t *Transport) Close() error {
	t.closed = true
	return nil
}

// NewClient constructs a [client.Client] backed by an in-process
// transport over dispatcher. opts.Transport must be nil — supplying
// both an explicit Transport and a Dispatcher is ambiguous and is
// rejected.
//
// opts.HostPort is ignored for inproc (no dial happens). All other
// fields (Namespace, Identity, CallTimeout, ...) flow through to
// [client.Dial] unchanged.
func NewClient(dispatcher Dispatcher, opts client.Options) (client.Client, error) {
	if opts.Transport != nil {
		return nil, errors.New("hanzo/tasks/inproc: opts.Transport must be nil — use either inproc or a Transport, not both")
	}
	opts.Transport = NewTransport(dispatcher)
	// HostPort is required by Dial when no Transport is set; we set
	// the Transport above, so the value is ignored downstream. Pass a
	// sentinel so logs are unambiguous.
	if opts.HostPort == "" {
		opts.HostPort = "inproc://frontend"
	}
	return client.Dial(opts)
}
