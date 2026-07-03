// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/hanzoai/tasks/pkg/sdk/temporal"
	"github.com/hanzoai/tasks/pkg/sdk/workflow"
)

// Phase-2a replay decider support: a workflow task carries the run's full
// event-sourced history; the worker replays the registered function from
// event 0, resolving each ExecuteActivity from history by seq. When the
// function blocks on an activity that is not yet terminal, the episode ends
// and the worker returns the new commands it collected.

// history event type strings — must match pkg/tasks (the producer).
const (
	evtWorkflowStarted   = "WORKFLOW_EXECUTION_STARTED"
	evtActivityScheduled = "ACTIVITY_TASK_SCHEDULED"
	evtActivityCompleted = "ACTIVITY_TASK_COMPLETED"
	evtActivityFailed    = "ACTIVITY_TASK_FAILED"
)

// histEvent is the worker-side view of a pkg/tasks HistoryEvent. Attributes
// are kept as raw JSON so activity result/failure payloads round-trip
// byte-exact into the futures the decider hands back.
type histEvent struct {
	EventId    int64                      `json:"eventId"`
	EventType  string                     `json:"eventType"`
	Attributes map[string]json.RawMessage `json:"attributes"`
}

// seq reads the deterministic command sequence number from an event's
// attributes. Numeric on the wire (survives the store's JSON round-trip as
// a float), so we decode via float64.
func (h histEvent) seq() (int, bool) {
	raw, ok := h.Attributes["seq"]
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return int(f), true
}

// rawAttr returns the raw JSON bytes of an attribute, or nil.
func (h histEvent) rawAttr(key string) []byte {
	raw, ok := h.Attributes[key]
	if !ok {
		return nil
	}
	return append([]byte(nil), raw...)
}

// parseHistory decodes the workflow-task History payload into events. An
// empty payload yields no events (a brand-new run before any event is
// unusual — the first task always carries WORKFLOW_EXECUTION_STARTED).
func parseHistory(b []byte) ([]histEvent, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var events []histEvent
	if err := json.Unmarshal(b, &events); err != nil {
		return nil, fmt.Errorf("parse workflow history: %w", err)
	}
	return events, nil
}

// workflowInputFromHistory extracts the workflow input (a JSON array of
// arguments) recorded on the WORKFLOW_EXECUTION_STARTED event.
func workflowInputFromHistory(events []histEvent) []byte {
	for i := range events {
		if events[i].EventType == evtWorkflowStarted {
			return events[i].rawAttr("input")
		}
	}
	return nil
}

// episodeBlocked is the sentinel panicked by blockedFuture.Get to unwind
// the workflow function when it awaits an activity whose result is not yet
// in history. The episode runner recovers it and ends the episode cleanly.
type episodeBlocked struct{}

var episodeBlockedSentinel = episodeBlocked{}

// blockedFuture is returned by the decider for an activity whose result is
// not yet in history. Calling Get ends the decision episode: the worker has
// already emitted the ScheduleActivity command and cannot make further
// progress until the server delivers the next task with the result
// appended. Get unwinds via episodeBlockedSentinel.
type blockedFuture struct{}

func (blockedFuture) Get(workflow.Context, any) error { panic(episodeBlockedSentinel) }
func (blockedFuture) IsReady() bool                   { return false }
func (blockedFuture) ReadyCh() <-chan struct{}        { return neverReadyCh }

// neverReadyCh is a shared channel that never closes — a blocked future is
// never "ready" for a Select fan-in.
var neverReadyCh = make(chan struct{})

// runWorkflowEpisode invokes the workflow function for one decision
// episode. It returns (result, nil, false) on normal completion,
// (nil, err, false) on a workflow error, and (nil, nil, true) when the
// function blocked on an activity awaiting its result. A genuine panic in
// user code is turned into a workflow failure, not propagated.
func runWorkflowEpisode(fn any, args []reflect.Value) (result any, runErr error, blocked bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(episodeBlocked); ok {
				blocked = true
				return
			}
			runErr = temporal.NewError(fmt.Sprintf("workflow panic: %v", r), "PanicError", false)
		}
	}()
	result, runErr = invokeFunc(fn, args)
	return
}
