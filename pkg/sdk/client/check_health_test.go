// Copyright © 2026 Hanzo AI. MIT License.

package client

import (
	"context"
	"errors"
	"testing"
)

// TestCheckHealthOpcode confirms CheckHealth uses opcode 0x0090 and
// returns the server-reported service + status.
func TestCheckHealthOpcode(t *testing.T) {
	t.Parallel()
	st := &stubTransport{
		dynamicReply: func(op uint16, _ []byte) (map[string]any, uint32, string, error) {
			if op != opHealth {
				return nil, 0, "", errors.New("wrong opcode")
			}
			return map[string]any{"service": "hanzo-tasks", "status": "ok"}, 0, "", nil
		},
	}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	resp, err := c.CheckHealth(context.Background(), &CheckHealthRequest{})
	if err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if resp.Service != "hanzo-tasks" {
		t.Fatalf("Service = %q", resp.Service)
	}
	if resp.Status != "ok" {
		t.Fatalf("Status = %q", resp.Status)
	}
}

// TestCheckHealthNilRequestAccepted treats nil as empty.
func TestCheckHealthNilRequestAccepted(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{"service": "hanzo-tasks", "status": "ok"}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	if _, err := c.CheckHealth(context.Background(), nil); err != nil {
		t.Fatalf("CheckHealth(nil): %v", err)
	}
}

// TestCheckHealthMatchesHealth verifies Health (flat strings) and
// CheckHealth (structured response) are shape-consistent.
func TestCheckHealthMatchesHealth(t *testing.T) {
	t.Parallel()
	st := &stubTransport{respBody: map[string]any{"service": "hanzo-tasks", "status": "ok"}}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	svc, status, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	resp, err := c.CheckHealth(context.Background(), nil)
	if err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if svc != resp.Service || status != resp.Status {
		t.Fatalf("shape mismatch: (%q,%q) vs %+v", svc, status, resp)
	}
}

// TestGetWorkflowHandleOnly exercises GetWorkflow, which issues no RPC
// and simply returns a handle.
func TestGetWorkflowHandleOnly(t *testing.T) {
	t.Parallel()
	st := &stubTransport{}
	c, _ := Dial(Options{Transport: st})
	defer c.Close()
	run := c.GetWorkflow(context.Background(), "wf-1", "run-1")
	if run.GetID() != "wf-1" || run.GetRunID() != "run-1" {
		t.Fatalf("handle = %+v", run)
	}
	if st.callCount != 0 {
		t.Fatalf("GetWorkflow must not issue an RPC; calls=%d", st.callCount)
	}
}
