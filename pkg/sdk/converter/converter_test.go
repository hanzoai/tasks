// Copyright © 2026 Hanzo AI. MIT License.

package converter

import (
	"errors"
	"testing"
)

func TestNewJSONValueEmpty(t *testing.T) {
	t.Parallel()
	v := NewJSONValue(nil)
	if v.HasValue() {
		t.Fatal("empty payload must report HasValue=false")
	}
	var out any
	if err := v.Get(&out); err != nil {
		t.Fatalf("Get on empty payload: %v", err)
	}
	if out != nil {
		t.Fatalf("Get on empty payload left %v in destination", out)
	}
}

func TestNewJSONValueRoundTrip(t *testing.T) {
	t.Parallel()
	v := NewJSONValue([]byte(`{"a":1,"b":"hello"}`))
	if !v.HasValue() {
		t.Fatal("non-empty payload must report HasValue=true")
	}
	var out struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	if err := v.Get(&out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if out.A != 1 || out.B != "hello" {
		t.Fatalf("decoded = %+v", out)
	}
}

func TestNilPointerRejected(t *testing.T) {
	t.Parallel()
	v := NewJSONValue([]byte(`"x"`))
	if err := v.Get(nil); !errors.Is(err, ErrNilPointer) {
		t.Fatalf("expected ErrNilPointer, got %v", err)
	}
}

func TestDefensiveCopy(t *testing.T) {
	t.Parallel()
	src := []byte(`{"n":1}`)
	v := NewJSONValue(src)
	// Mutate the source.
	src[0] = 'X'
	var out struct {
		N int `json:"n"`
	}
	if err := v.Get(&out); err != nil {
		t.Fatalf("Get after source mutation: %v", err)
	}
	if out.N != 1 {
		t.Fatalf("expected defensive copy; got %+v", out)
	}
}
