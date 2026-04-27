// Copyright © 2026 Hanzo AI. MIT License.

// Native realtime event tail for Go callers. Streams Events from
// http://<host>:<httpPort>/v1/tasks/events without spinning up an
// HTTP browser stack — just a long-lived TCP request parsing the
// SSE framing inline.
//
// The transport is HTTP/SSE today because that's the canonical wire
// for the same stream the browser consumes. A pure-ZAP streaming
// opcode (0x00B1, reserved) lands when luxfi/zap exposes a streaming
// frame primitive on its Node API.
//
// No gRPC. No protobuf. No upstream client.

package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Event mirrors pkg/tasks.Event on the wire.
type Event struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	ScheduleID string `json:"schedule_id,omitempty"`
	BatchID    string `json:"batch_id,omitempty"`
	At         string `json:"at"`
	Data       any    `json:"data,omitempty"`
}

// SubscribeEvents opens an SSE stream to the daemon's /v1/tasks/events
// endpoint and returns a channel that yields decoded Events until ctx
// is canceled or the connection drops. Cancel ctx to stop.
//
// httpAddr is the daemon's HTTP listener (e.g. "127.0.0.1:7243").
// Pass empty to use http://<HostPort host>:7243 — the canonical sibling
// port shipped by tasksd alongside ZAP :9999.
func SubscribeEvents(ctx context.Context, httpAddr string) (<-chan Event, error) {
	if httpAddr == "" {
		return nil, errors.New("hanzo/tasks/client.SubscribeEvents: httpAddr required")
	}
	if !strings.HasPrefix(httpAddr, "http") {
		httpAddr = "http://" + httpAddr
	}
	url := strings.TrimRight(httpAddr, "/") + "/v1/tasks/events"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("hanzo/tasks/client.SubscribeEvents: %s → %d", url, resp.StatusCode)
	}

	out := make(chan Event, 16)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		// SSE frames are well under any sensible cap; bump the buffer
		// to 256 KiB to handle large workflow data payloads.
		scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
		for scanner.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(payload), &ev); err != nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out, nil
}
