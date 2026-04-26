// Copyright © 2026 Hanzo AI. MIT License.

package inproc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hanzoai/tasks/pkg/sdk/client"
	"github.com/luxfi/zap"
)

// fakeDispatcher echoes the request body back inside the standard
// envelope. Used to prove the transport's frame-in / frame-out path
// without standing up the real frontend handler.
type fakeDispatcher struct {
	calls int
	last  uint16
	fail  error
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, opcode uint16, msg *zap.Message) (*zap.Message, error) {
	f.calls++
	f.last = opcode
	if f.fail != nil {
		return nil, f.fail
	}
	body := msg.Root().Bytes(0)
	b := zap.NewBuilder(len(body) + 64)
	obj := b.StartObject(24)
	obj.SetBytes(0, body)
	obj.SetUint32(8, 200)
	obj.SetBytes(12, nil)
	obj.FinishAsRoot()
	flags := uint16(opcode) << 8
	frame := b.FinishWithFlags(flags)
	return zap.Parse(frame)
}

func TestTransport_RoundTrip_EnvelopeBody(t *testing.T) {
	d := &fakeDispatcher{}
	tr := NewTransport(d)

	body, err := json.Marshal(map[string]string{"namespace": "default", "workflow_id": "wf-1"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, err := tr.Call(context.Background(), 0x0060, body)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if d.calls != 1 {
		t.Fatalf("expected 1 dispatch, got %d", d.calls)
	}
	if d.last != 0x0060 {
		t.Fatalf("expected opcode 0x0060 dispatched, got 0x%04x", d.last)
	}

	msg, err := zap.Parse(resp)
	if err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if got := msg.Root().Uint32(8); got != 200 {
		t.Fatalf("expected envelope status 200, got %d", got)
	}
	if got := msg.Root().Bytes(0); string(got) != string(body) {
		t.Fatalf("expected echo body, got %q", got)
	}
}

func TestTransport_RoundTrip_AlreadyFramed(t *testing.T) {
	d := &fakeDispatcher{}
	tr := NewTransport(d)

	// Build a worker-shape frame: native object layout, not the envelope.
	b := zap.NewBuilder(128)
	obj := b.StartObject(40)
	obj.SetText(0, "default")
	obj.SetText(8, "test-queue")
	obj.SetUint32(16, 1)
	obj.FinishAsRoot()
	rawFrame := b.FinishWithFlags(uint16(0x00A0) << 8)

	// FrameBody re-stamps the opcode flags but otherwise passes through.
	resp, err := tr.Call(context.Background(), 0x00A0, rawFrame)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if d.last != 0x00A0 {
		t.Fatalf("expected opcode 0x00A0 dispatched, got 0x%04x", d.last)
	}
	if len(resp) == 0 {
		t.Fatalf("expected non-empty response frame")
	}
}

func TestTransport_AfterClose(t *testing.T) {
	d := &fakeDispatcher{}
	tr := NewTransport(d)
	if err := tr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := tr.Call(context.Background(), 0x0060, []byte("{}"))
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
	// Idempotent close.
	if err := tr.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestTransport_DispatcherError(t *testing.T) {
	want := errors.New("synthetic dispatch failure")
	d := &fakeDispatcher{fail: want}
	tr := NewTransport(d)

	_, err := tr.Call(context.Background(), 0x0060, []byte("{}"))
	if !errors.Is(err, want) {
		t.Fatalf("expected dispatcher error to surface, got %v", err)
	}
}

func TestNewClient_RejectsTransportConflict(t *testing.T) {
	d := &fakeDispatcher{}
	// Dummy transport just to trigger the rejection path.
	other := NewTransport(d)
	defer other.Close()

	_, err := NewClient(d, client.Options{Transport: other})
	if err == nil {
		t.Fatal("expected error when both Transport and Dispatcher provided")
	}
}

func TestNewClient_DefaultsHostPortSentinel(t *testing.T) {
	d := &fakeDispatcher{}
	c, err := NewClient(d, client.Options{Namespace: "default"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()

	// Health is the smallest round-trip; the fake dispatcher echoes
	// the request body, so the response decode will fail JSON-shape
	// for HealthResponse — but the transport plumbing succeeded if we
	// got that far without a connect error.
	_, _, err = c.Health(context.Background())
	if err == nil {
		t.Skip("fake dispatcher echo accidentally satisfied health decode")
	}
	// We only assert that the failure is from response decode, not
	// from HostPort dial — the latter would be "Options.HostPort is
	// required" before any RPC is issued.
	if got := err.Error(); got == "hanzo/tasks/client: Options.HostPort is required when no Transport is injected" {
		t.Fatalf("inproc.NewClient should not require HostPort, got %v", err)
	}
}
