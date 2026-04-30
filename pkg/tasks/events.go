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
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/hanzoai/tasks/pkg/auth"
)

type Event struct {
	Kind       string `json:"kind"`        // workflow.started|workflow.canceled|workflow.terminated|workflow.signaled|schedule.created|schedule.paused|schedule.resumed|schedule.deleted|namespace.registered|batch.started
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
	mu       sync.RWMutex
	nextID   atomic.Uint64
	subs     map[uint64]chan Event
	bufSize  int
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

// sseHandler streams Events as text/event-stream. Disconnects unsubscribe.
func (e *Embedded) sseHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		// Hello frame so clients know the stream is live.
		fmt.Fprintf(w, ": hanzo-tasks event stream\n\n")
		flusher.Flush()

		id, ch := e.engine.broker.subscribe()
		defer e.engine.broker.unsubscribe(id)

		// Per-tenant filter: when the caller authenticated, only deliver
		// events tagged with their org. Empty caller-org (embedded/dev)
		// sees the full unscoped firehose — same surface as today.
		callerOrg := auth.OrgID(r.Context())

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				if callerOrg != "" && ev.OrgID != callerOrg {
					continue
				}
				body, err := json.Marshal(ev)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, body)
				flusher.Flush()
			}
		}
	})
}
