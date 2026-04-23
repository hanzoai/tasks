// Copyright © 2026 Hanzo AI. MIT License.

// Package converter provides the EncodedValue surface returned by
// Client.QueryWorkflow and other RPCs that ship an opaque payload.
//
// EncodedValue mirrors go.temporal.io/sdk/converter.EncodedValue
// exactly so caller code migrating from the upstream package swaps
// only the import path. The Phase-1 default implementation is JSON.
//
// Zero go.temporal.io/* imports. Zero google.golang.org/grpc imports.
package converter

import (
	"encoding/json"
	"errors"
)

// EncodedValue is a decoded-on-demand handle to a single payload. The
// caller inspects whether data is present via HasValue and decodes it
// into a destination pointer via Get. Both methods are safe to call
// any number of times on the same value.
type EncodedValue interface {
	// HasValue reports whether a non-empty payload is present.
	// Callers use it to distinguish "query returned nil" from
	// "query returned empty string/struct".
	HasValue() bool

	// Get decodes the payload into valuePtr. valuePtr must be a
	// non-nil pointer to a decodable Go value; passing nil returns
	// ErrNilPointer. An empty payload with a non-nil pointer leaves
	// valuePtr untouched and returns nil (matches upstream).
	Get(valuePtr any) error
}

// ErrNilPointer is returned by Get when valuePtr is nil.
var ErrNilPointer = errors.New("converter: Get requires a non-nil pointer")

// jsonValue is the Phase-1 implementation: the payload is a JSON
// document. Phase-2 will swap this for a schema-driven codec without
// changing the EncodedValue interface.
type jsonValue struct {
	data []byte
}

// NewJSONValue wraps a JSON-encoded payload. Callers receive one of
// these from RPCs that ship a single opaque result (Query, etc).
func NewJSONValue(data []byte) EncodedValue {
	// Defensive copy so mutation of the caller's slice does not
	// change what HasValue/Get observe.
	if len(data) == 0 {
		return &jsonValue{}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return &jsonValue{data: cp}
}

// HasValue implements EncodedValue.
func (v *jsonValue) HasValue() bool {
	if v == nil {
		return false
	}
	return len(v.data) > 0
}

// Get implements EncodedValue.
func (v *jsonValue) Get(valuePtr any) error {
	if valuePtr == nil {
		return ErrNilPointer
	}
	if v == nil || len(v.data) == 0 {
		return nil
	}
	return json.Unmarshal(v.data, valuePtr)
}
