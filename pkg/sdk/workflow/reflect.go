package workflow

import (
	"reflect"
	"runtime"
	"strings"
)

// funcName returns the best-effort name for an activity function
// value. Used by the stub env to key its registered responses and
// by the worker at Phase 2 to dispatch by registered name.
//
// Kept in its own file so the reflect dependency is obvious and
// easy to audit — this is the only place the workflow package
// reflects.
func funcName(v any) string {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.Kind() != reflect.Func {
		return "unknown"
	}
	rf := runtime.FuncForPC(rv.Pointer())
	if rf == nil {
		return "unknown"
	}
	full := rf.Name() // e.g. github.com/x/y/z.DoWork
	// Trim to "DoWork" — what Temporal also does by default.
	if i := strings.LastIndex(full, "."); i >= 0 && i < len(full)-1 {
		return full[i+1:]
	}
	return full
}
