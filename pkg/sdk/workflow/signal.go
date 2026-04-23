package workflow

// GetSignalChannel returns the ReceiveChannel for the named signal.
// Two calls with the same name return handles that observe the
// same queue — signals are keyed by name, not handle identity.
// This matches Temporal's semantics and is what base/plugins/tasks
// relies on for claim/complete/fail/cancel fan-in.
//
// Signals are durable: the worker persists them and re-delivers
// them on replay. Sending a signal is the client's job
// (pkg/sdk/client); this helper only exposes the workflow-side read.
func GetSignalChannel(ctx Context, name string) ReceiveChannel {
	if ctx == nil {
		// Return an empty, closed channel so the caller's next
		// Receive returns ok=false rather than deadlocking.
		c := newChan(name, 0)
		c.Close()
		return c
	}
	return ctx.env().GetSignalChannel(name)
}
