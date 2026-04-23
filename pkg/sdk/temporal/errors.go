package temporal

import (
	"errors"
	"fmt"
)

// Sentinel Code values carried on the wire in Error.Code.
// They are matched by IsCanceledError / IsTimeoutError and can be
// listed in RetryPolicy.NonRetryableErrorTypes.
const (
	// CodeApplication is the default code for an error produced by
	// user workflow/activity code via NewError. It carries the
	// user-supplied type tag in Error.Code; this constant is used
	// only when the caller did not supply one.
	CodeApplication = "ApplicationError"

	// CodeCanceled marks an error arising from a workflow / activity
	// cancellation. Non-retryable by construction.
	CodeCanceled = "CanceledError"

	// CodeTimeout marks an error arising from schedule-to-close,
	// start-to-close, or heartbeat timeouts. Non-retryable.
	CodeTimeout = "TimeoutError"

	// CodeDecode marks an error produced by the failure decoder
	// when given bytes it cannot parse. Non-retryable.
	CodeDecode = "DecodeError"
)

// Error is the one and only error type this SDK emits for
// workflow/activity failures. Having a single concrete type keeps
// classification trivial (type assertion + Code check) and
// serialisation obvious.
//
// The zero value is a usable but uninformative error. Prefer the
// constructors below.
type Error struct {
	// Message is the human-readable error text. Safe to log.
	Message string

	// Code is a stable, machine-readable tag. Callers compare
	// against this to decide whether to retry, branch, etc. It is
	// the field listed in RetryPolicy.NonRetryableErrorTypes.
	//
	// Reserved values: CodeApplication, CodeCanceled, CodeTimeout,
	// CodeDecode. Anything else is user-defined.
	Code string

	// NonRetryable short-circuits retry regardless of MaximumAttempts.
	// Canceled and Timeout errors are always non-retryable.
	NonRetryable bool

	// Details carries arbitrary user payloads attached at the
	// failure site. Must be JSON-serialisable. Decoders surface
	// them as []any.
	Details []any

	// Cause wraps the underlying Go error, if any. errors.Unwrap
	// exposes it so errors.Is / errors.As chains work.
	Cause error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	code := e.Code
	if code == "" {
		code = CodeApplication
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s (%s): %s", e.Message, code, e.Cause.Error())
	}
	return fmt.Sprintf("%s (%s)", e.Message, code)
}

// Unwrap allows errors.Is / errors.As to reach the underlying cause.
func (e *Error) Unwrap() error { return e.Cause }

// Is lets callers match via errors.Is(err, Canceled) etc., using
// the sentinel values declared below.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	// Sentinels are compared by Code only; instance identity is
	// irrelevant because the real value usually came off the wire.
	if t == canceledSentinel || t == timeoutSentinel {
		return e != nil && e.Code == t.Code
	}
	return e == t
}

// NewError constructs an Error. msg is the human-readable message,
// code is the machine-readable type tag (empty => CodeApplication),
// nonRetryable forces retry short-circuit, and details is an
// arbitrary JSON-serialisable payload attached to the failure.
//
// This is the one, canonical constructor. Previous upstream callers
// that used NewApplicationError(msg, code, details...) map to
// NewError(msg, code, false, details...).
func NewError(msg, code string, nonRetryable bool, details ...any) *Error {
	if code == "" {
		code = CodeApplication
	}
	return &Error{
		Message:      msg,
		Code:         code,
		NonRetryable: nonRetryable,
		Details:      details,
	}
}

// NewErrorWithCause is NewError with a wrapped underlying error.
// Equivalent to NewError + manual .Cause assignment; kept for call
// sites that previously used NewApplicationErrorWithCause.
func NewErrorWithCause(msg, code string, cause error, nonRetryable bool, details ...any) *Error {
	e := NewError(msg, code, nonRetryable, details...)
	e.Cause = cause
	return e
}

// NewCanceledError constructs a canceled error. Canceled errors are
// always non-retryable.
func NewCanceledError(details ...any) *Error {
	return &Error{
		Message:      "workflow canceled",
		Code:         CodeCanceled,
		NonRetryable: true,
		Details:      details,
	}
}

// NewTimeoutError constructs a timeout error. Timeout errors are
// always non-retryable.
func NewTimeoutError(msg string, details ...any) *Error {
	if msg == "" {
		msg = "operation timed out"
	}
	return &Error{
		Message:      msg,
		Code:         CodeTimeout,
		NonRetryable: true,
		Details:      details,
	}
}

// canceledSentinel / timeoutSentinel back the Canceled / Timeout
// package-level values used with errors.Is. They carry only the
// Code field so the Is() comparison is code-only.
var (
	canceledSentinel = &Error{Code: CodeCanceled, NonRetryable: true}
	timeoutSentinel  = &Error{Code: CodeTimeout, NonRetryable: true}

	// Canceled is the sentinel used with errors.Is to detect a
	// canceled-class error anywhere in the chain.
	Canceled error = canceledSentinel

	// Timeout is the sentinel used with errors.Is to detect a
	// timeout-class error anywhere in the chain.
	Timeout error = timeoutSentinel
)

// AsError is a typed wrapper around errors.As for *Error.
// Returns the embedded *Error and true on hit, nil and false otherwise.
func AsError(err error) (*Error, bool) {
	var t *Error
	if errors.As(err, &t) {
		return t, true
	}
	return nil, false
}

// IsApplicationError reports whether err (or anything it wraps) is
// an *Error with an application-class Code — i.e. it was produced
// by user code via NewError, not a canceled or timeout sentinel.
func IsApplicationError(err error) bool {
	t, ok := AsError(err)
	if !ok {
		return false
	}
	return t.Code != CodeCanceled && t.Code != CodeTimeout
}

// IsCanceledError reports whether err (or anything it wraps) is a
// canceled-class *Error.
func IsCanceledError(err error) bool {
	t, ok := AsError(err)
	return ok && t.Code == CodeCanceled
}

// IsTimeoutError reports whether err (or anything it wraps) is a
// timeout-class *Error.
func IsTimeoutError(err error) bool {
	t, ok := AsError(err)
	return ok && t.Code == CodeTimeout
}
