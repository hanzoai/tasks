// Copyright © 2026 Hanzo AI. MIT License.

// Realtime event stream — Server-Sent Events for the Web UI.
//
// Every state-changing engine method emits an Event to the broker; HTTP
// clients subscribe via GET /v1/tasks/events (text/event-stream) and
// receive a live tail. The same event stream is reachable over the
// canonical ZAP wire as opcode 0x00B1 once a streaming opcode is
// added — until then SSE is the browser's only need.

package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/hanzoai/tasks/pkg/auth"
)

type Event struct {
	Kind       string `json:"kind"`             // workflow.started|workflow.canceled|workflow.terminated|workflow.signaled|schedule.created|schedule.paused|schedule.resumed|schedule.deleted|namespace.registered|batch.started
	OrgID      string `json:"org_id,omitempty"` // tenant scope; "" = unscoped (embedded/dev)
	Namespace  string `json:"namespace,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	ScheduleID string `json:"schedule_id,omitempty"`
	BatchID    string `json:"batch_id,omitempty"`
	At         string `json:"at"`
	Data       any    `json:"data,omitempty"`
}

// broker is a tiny fan-out: subscribers receive every event published.
// Bounded buffers per subscriber drop the oldest event under back-pressure
// rather than blocking the publisher. Designed for single-process tasks;
// distributed fan-out lands when persistence does.
type broker struct {
	mu      sync.RWMutex
	nextID  atomic.Uint64
	subs    map[uint64]chan Event
	bufSize int
}

func newBroker() *broker {
	return &broker{subs: map[uint64]chan Event{}, bufSize: 256}
}

func (b *broker) subscribe() (uint64, <-chan Event) {
	id := b.nextID.Add(1)
	ch := make(chan Event, b.bufSize)
	b.mu.Lock()
	b.subs[id] = ch
	b.mu.Unlock()
	return id, ch
}

func (b *broker) unsubscribe(id uint64) {
	b.mu.Lock()
	if ch, ok := b.subs[id]; ok {
		close(ch)
		delete(b.subs, id)
	}
	b.mu.Unlock()
}

func (b *broker) publish(e Event) {
	if e.At == "" {
		e.At = nowRFC3339()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			// Drop the oldest then enqueue. Keeps the latest events
			// flowing to slow clients without backing up the engine.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- e:
			default:
			}
		}
	}
}

// eventHeaders are the four an event stream carries, in the order the browser
// reads them. Both transports set exactly these before the first frame.
var eventHeaders = [][2]string{
	{"Content-Type", "text/event-stream"},
	{"Cache-Control", "no-cache"},
	{"Connection", "keep-alive"},
	{"X-Accel-Buffering", "no"},
}

// sseHandler streams Events as text/event-stream. Disconnects unsubscribe.
func (e *Embedded) sseHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			plain(http.StatusInternalServerError, "streaming unsupported").write(w)
			return
		}
		for _, h := range eventHeaders {
			w.Header().Set(h[0], h[1])
		}
		e.feed(r.Context(), auth.OrgID(r.Context()), w, func() error { flusher.Flush(); return nil })
	})
}

// feed writes the engine's event stream to w until ctx ends, the subscription
// closes, or the client stops reading. org "" sees the full unscoped firehose
// (embedded/dev); otherwise only events tagged with that tenant. flush pushes
// each frame and reports a connection that can no longer take one.
func (e *Embedded) feed(ctx context.Context, org string, w io.Writer, flush func() error) {
	// Hello frame so clients know the stream is live.
	if _, err := io.WriteString(w, ": hanzo-tasks event stream\n\n"); err != nil {
		return
	}
	if flush() != nil {
		return
	}

	id, ch := e.engine.broker.subscribe()
	defer e.engine.broker.unsubscribe(id)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if org != "" && ev.OrgID != org {
				continue
			}
			body, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, body); err != nil {
				return
			}
			if flush() != nil {
				return
			}
		}
	}
}
