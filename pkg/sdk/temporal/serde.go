package temporal

import (
	"encoding/json"
	"fmt"
)

// failureEnvelopeVersion is the envelope version prefix. Bump only
// when the wire layout changes incompatibly. The decoder rejects
// anything with a different version (and returns a DecodeError), so
// this is how we guarantee forward/backward isolation.
const failureEnvelopeVersion = 1

// failureEnvelope is the on-wire shape carried inside the
// ActivityTask / RespondActivityTaskFailed `failure Bytes` field.
//
// Body is JSON today; the plan (per the package doc) is to swap to
// native ZAP once zapc codegen lands. The envelope stays the same:
// { version, payload } bytes. Consumers that see Version != 1
// return DecodeError — no silent promotion.
type failureEnvelope struct {
	Version int             `json:"v"`
	Payload json.RawMessage `json:"p"`
}

// failurePayload is the inner JSON shape for Version=1. It is a
// direct mirror of *Error minus the non-serialisable Cause, which
// is projected into CauseMessage/CauseCode so it round-trips as a
// plain nested envelope.
type failurePayload struct {
	Message      string            `json:"message"`
	Code         string            `json:"code"`
	NonRetryable bool              `json:"nonRetryable,omitempty"`
	Details      []json.RawMessage `json:"details,omitempty"`
	Cause        *failurePayload   `json:"cause,omitempty"`
}

// Encode serialises err into the bytes that live in the `failure`
// field of RespondActivityTaskFailedRequest (schema/tasks.zap).
//
// If err is not already an *Error it is wrapped as an ApplicationError
// with the original message preserved. Non-JSON-serialisable details
// are replaced by their error string so Encode never fails on input
// (workers must not get stuck unable to report a failure).
func Encode(err error) ([]byte, error) {
	payload := toPayload(err)
	body, merr := json.Marshal(payload)
	if merr != nil {
		// Should be unreachable — sanitiseDetails strips anything
		// that would trip Marshal. If it does happen, emit a
		// well-formed envelope that decodes back to a DecodeError.
		return Encode(NewError(
			fmt.Sprintf("failed to encode failure: %v", merr),
			CodeDecode, true,
		))
	}
	env := failureEnvelope{
		Version: failureEnvelopeVersion,
		Payload: body,
	}
	return json.Marshal(env)
}

// Decode parses bytes produced by Encode back into an *Error.
//
// Fail-secure: any parsing problem returns a non-retryable
// DecodeError whose message identifies the failure class, never a
// nil pointer and never a panic. Callers can feed hostile bytes.
func Decode(b []byte) *Error {
	if len(b) == 0 {
		return NewError("empty failure payload", CodeDecode, true)
	}
	var env failureEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		return NewError(
			fmt.Sprintf("failure envelope unmarshal: %v", err),
			CodeDecode, true,
		)
	}
	if env.Version != failureEnvelopeVersion {
		return NewError(
			fmt.Sprintf("unsupported failure envelope version %d", env.Version),
			CodeDecode, true,
		)
	}
	var payload failurePayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return NewError(
			fmt.Sprintf("failure payload unmarshal: %v", err),
			CodeDecode, true,
		)
	}
	return fromPayload(&payload)
}

// toPayload converts any Go error into the JSON-serialisable
// failurePayload form. Non-Error inputs are treated as application
// errors with the default code; their message is the err.Error().
func toPayload(err error) *failurePayload {
	if err == nil {
		return nil
	}
	t, ok := AsError(err)
	if !ok {
		return &failurePayload{
			Message: err.Error(),
			Code:    CodeApplication,
		}
	}
	out := &failurePayload{
		Message:      t.Message,
		Code:         t.Code,
		NonRetryable: t.NonRetryable,
		Details:      sanitiseDetails(t.Details),
	}
	if t.Cause != nil {
		out.Cause = toPayload(t.Cause)
	}
	if out.Code == "" {
		out.Code = CodeApplication
	}
	return out
}

// fromPayload rehydrates an *Error from a failurePayload.
func fromPayload(p *failurePayload) *Error {
	if p == nil {
		return nil
	}
	e := &Error{
		Message:      p.Message,
		Code:         p.Code,
		NonRetryable: p.NonRetryable,
	}
	if e.Code == "" {
		e.Code = CodeApplication
	}
	if len(p.Details) > 0 {
		e.Details = make([]any, 0, len(p.Details))
		for _, raw := range p.Details {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				// Preserve the raw bytes as a string so callers
				// still see _something_ — better than dropping.
				e.Details = append(e.Details, string(raw))
				continue
			}
			e.Details = append(e.Details, v)
		}
	}
	if p.Cause != nil {
		e.Cause = fromPayload(p.Cause)
	}
	return e
}

// sanitiseDetails marshals each detail individually so one bad
// entry cannot poison the whole failure. Entries that fail to
// encode are replaced with a best-effort string representation.
func sanitiseDetails(details []any) []json.RawMessage {
	if len(details) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(details))
	for _, d := range details {
		raw, err := json.Marshal(d)
		if err != nil {
			fallback, _ := json.Marshal(fmt.Sprintf("%v", d))
			out = append(out, fallback)
			continue
		}
		out = append(out, raw)
	}
	return out
}
