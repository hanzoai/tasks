package temporal

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// RetryPolicy
// ---------------------------------------------------------------------------

func TestDefaultRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()
	if p.InitialInterval != time.Second {
		t.Errorf("InitialInterval = %v, want 1s", p.InitialInterval)
	}
	if p.BackoffCoefficient != 2.0 {
		t.Errorf("BackoffCoefficient = %v, want 2.0", p.BackoffCoefficient)
	}
	if p.MaximumInterval != 100*time.Second {
		t.Errorf("MaximumInterval = %v, want 100s", p.MaximumInterval)
	}
	if p.MaximumAttempts != 0 {
		t.Errorf("MaximumAttempts = %d, want 0 (unlimited)", p.MaximumAttempts)
	}
}

func TestRetryPolicy_Normalize(t *testing.T) {
	cases := []struct {
		name      string
		in        *RetryPolicy
		wantInit  time.Duration
		wantCoeff float64
		wantMax   time.Duration
	}{
		{"nil returns default",
			nil, time.Second, 2.0, 100 * time.Second},
		{"zero initial fills with default",
			&RetryPolicy{BackoffCoefficient: 3, MaximumInterval: 9 * time.Second},
			time.Second, 3, 9 * time.Second},
		{"coefficient < 1 fills with default",
			&RetryPolicy{InitialInterval: 2 * time.Second, BackoffCoefficient: 0.5},
			2 * time.Second, 2.0, 200 * time.Second},
		{"zero max derived from initial*100",
			&RetryPolicy{InitialInterval: 3 * time.Second, BackoffCoefficient: 2},
			3 * time.Second, 2, 300 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Normalize()
			if got.InitialInterval != tc.wantInit {
				t.Errorf("InitialInterval = %v, want %v", got.InitialInterval, tc.wantInit)
			}
			if got.BackoffCoefficient != tc.wantCoeff {
				t.Errorf("BackoffCoefficient = %v, want %v", got.BackoffCoefficient, tc.wantCoeff)
			}
			if got.MaximumInterval != tc.wantMax {
				t.Errorf("MaximumInterval = %v, want %v", got.MaximumInterval, tc.wantMax)
			}
		})
	}
}

func TestRetryPolicy_Normalize_DoesNotMutateInput(t *testing.T) {
	p := &RetryPolicy{} // all zeros
	_ = p.Normalize()
	if p.InitialInterval != 0 || p.BackoffCoefficient != 0 || p.MaximumInterval != 0 {
		t.Errorf("Normalize mutated input: %+v", p)
	}
}

func TestRetryPolicy_ShouldRetry(t *testing.T) {
	p := &RetryPolicy{MaximumAttempts: 3}
	cases := []struct {
		name    string
		err     error
		attempt int32
		want    bool
	}{
		{"nil err never retries", nil, 1, false},
		{"plain err within budget", errors.New("boom"), 1, true},
		{"plain err past budget", errors.New("boom"), 3, false},
		{"non-retryable error", NewError("nope", "HardFail", true), 1, false},
		{"error listed in NonRetryableErrorTypes",
			NewError("nope", "ValidationError", false), 1, false},
		{"canceled error", NewCanceledError(), 1, false},
		{"timeout error", NewTimeoutError(""), 1, false},
	}
	p.NonRetryableErrorTypes = []string{"ValidationError"}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := p.ShouldRetry(tc.err, tc.attempt)
			if got != tc.want {
				t.Errorf("ShouldRetry(%v, %d) = %v, want %v",
					tc.err, tc.attempt, got, tc.want)
			}
		})
	}
}

func TestRetryPolicy_NextInterval(t *testing.T) {
	p := &RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    10 * time.Second,
	}
	cases := []struct {
		attempt int32
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 10 * time.Second}, // clamped
		{100, 10 * time.Second},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("attempt=%d", tc.attempt), func(t *testing.T) {
			got := p.NextInterval(tc.attempt)
			if got != tc.want {
				t.Errorf("NextInterval(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}
}

func TestRetryPolicy_NextInterval_NormalizesNil(t *testing.T) {
	var p *RetryPolicy
	got := p.NextInterval(1)
	if got != time.Second {
		t.Errorf("nil policy NextInterval(1) = %v, want 1s", got)
	}
}

// ---------------------------------------------------------------------------
// Error constructors + classification
// ---------------------------------------------------------------------------

func TestNewError(t *testing.T) {
	e := NewError("oops", "MyCode", true, 1, "two")
	if e.Message != "oops" {
		t.Errorf("Message = %q, want oops", e.Message)
	}
	if e.Code != "MyCode" {
		t.Errorf("Code = %q, want MyCode", e.Code)
	}
	if !e.NonRetryable {
		t.Error("NonRetryable = false, want true")
	}
	if len(e.Details) != 2 {
		t.Errorf("len(Details) = %d, want 2", len(e.Details))
	}
}

func TestNewError_EmptyCodeDefaults(t *testing.T) {
	e := NewError("x", "", false)
	if e.Code != CodeApplication {
		t.Errorf("Code = %q, want %q", e.Code, CodeApplication)
	}
}

func TestNewErrorWithCause(t *testing.T) {
	cause := errors.New("underlying")
	e := NewErrorWithCause("wrapped", "IO", cause, false, "k", "v")
	if e.Cause != cause {
		t.Errorf("Cause = %v, want %v", e.Cause, cause)
	}
	if !errors.Is(e, cause) {
		t.Error("errors.Is(wrapped, cause) = false, want true")
	}
}

func TestError_ErrorString(t *testing.T) {
	e := NewError("boom", "Bad", false)
	got := e.Error()
	if got == "" {
		t.Error("Error() = empty")
	}
	// Must contain both the message and the code — that's the
	// observable contract for log aggregators.
	if !contains(got, "boom") || !contains(got, "Bad") {
		t.Errorf("Error() = %q, want to contain 'boom' and 'Bad'", got)
	}
}

func TestError_Unwrap(t *testing.T) {
	cause := errors.New("root")
	e := NewErrorWithCause("wrapped", "X", cause, false)
	if got := errors.Unwrap(e); got != cause {
		t.Errorf("Unwrap = %v, want %v", got, cause)
	}
}

func TestClassificationHelpers(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		isApp   bool
		isCancl bool
		isTO    bool
	}{
		{"nil", nil, false, false, false},
		{"plain error", errors.New("x"), false, false, false},
		{"application error", NewError("x", "Custom", false), true, false, false},
		{"canceled", NewCanceledError(), false, true, false},
		{"timeout", NewTimeoutError("slow"), false, false, true},
		{"wrapped application",
			fmt.Errorf("wrap: %w", NewError("x", "Y", false)),
			true, false, false},
		{"wrapped canceled",
			fmt.Errorf("wrap: %w", NewCanceledError()),
			false, true, false},
		{"wrapped timeout",
			fmt.Errorf("wrap: %w", NewTimeoutError("slow")),
			false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsApplicationError(tc.err); got != tc.isApp {
				t.Errorf("IsApplicationError = %v, want %v", got, tc.isApp)
			}
			if got := IsCanceledError(tc.err); got != tc.isCancl {
				t.Errorf("IsCanceledError = %v, want %v", got, tc.isCancl)
			}
			if got := IsTimeoutError(tc.err); got != tc.isTO {
				t.Errorf("IsTimeoutError = %v, want %v", got, tc.isTO)
			}
		})
	}
}

func TestSentinelMatch(t *testing.T) {
	if !errors.Is(NewCanceledError(), Canceled) {
		t.Error("errors.Is(canceled, Canceled) = false")
	}
	if !errors.Is(NewTimeoutError(""), Timeout) {
		t.Error("errors.Is(timeout, Timeout) = false")
	}
	if errors.Is(NewError("x", "Y", false), Canceled) {
		t.Error("errors.Is(app, Canceled) = true, want false")
	}
}

func TestCanceledIsNonRetryable(t *testing.T) {
	if !NewCanceledError().NonRetryable {
		t.Error("canceled error must be non-retryable")
	}
	if !NewTimeoutError("").NonRetryable {
		t.Error("timeout error must be non-retryable")
	}
}

// ---------------------------------------------------------------------------
// Round-trip serde
// ---------------------------------------------------------------------------

func TestEncodeDecode_RoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   error
	}{
		{"plain error",
			errors.New("plain")},
		{"application error",
			NewError("boom", "ValidationError", false, map[string]any{"field": "email"})},
		{"non-retryable",
			NewError("no", "HardFail", true)},
		{"canceled",
			NewCanceledError("reason", "user-abort")},
		{"timeout",
			NewTimeoutError("start-to-close", 30)},
		{"with cause",
			NewErrorWithCause("wrap", "IO",
				NewError("inner", "Disk", false), false)},
		{"no details",
			NewError("bare", "Bare", false)},
		{"nested cause chain",
			NewErrorWithCause("level1", "L1",
				NewErrorWithCause("level2", "L2",
					NewError("level3", "L3", true), false),
				false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			if len(b) == 0 {
				t.Fatal("Encode produced empty bytes")
			}
			got := Decode(b)
			if got == nil {
				t.Fatal("Decode returned nil")
			}

			// Compare observable fields. We can't just compare the
			// whole struct because Cause must be an *Error after a
			// round-trip, even if the input was a plain error.
			wantMsg := tc.in.Error()
			wantT, isT := AsError(tc.in)
			if isT {
				wantMsg = wantT.Message
			}
			if got.Message != wantMsg {
				t.Errorf("Message = %q, want %q", got.Message, wantMsg)
			}
			if isT {
				if got.Code != wantT.Code {
					t.Errorf("Code = %q, want %q", got.Code, wantT.Code)
				}
				if got.NonRetryable != wantT.NonRetryable {
					t.Errorf("NonRetryable = %v, want %v",
						got.NonRetryable, wantT.NonRetryable)
				}
				if (wantT.Cause == nil) != (got.Cause == nil) {
					t.Errorf("Cause presence mismatch: want %v, got %v",
						wantT.Cause, got.Cause)
				}
			} else if got.Code != CodeApplication {
				t.Errorf("Code = %q, want %q", got.Code, CodeApplication)
			}
		})
	}
}

func TestEncode_HandlesNonJSONDetails(t *testing.T) {
	// Channels cannot be marshalled. sanitiseDetails must turn
	// the offending entry into a string, not drop the whole
	// encode.
	bad := make(chan int)
	e := NewError("x", "Y", false, "ok", bad)
	b, err := Encode(e)
	if err != nil {
		t.Fatalf("Encode should not fail on bad detail, got %v", err)
	}
	got := Decode(b)
	if got == nil {
		t.Fatal("Decode returned nil")
	}
	if len(got.Details) != 2 {
		t.Fatalf("len(Details) = %d, want 2", len(got.Details))
	}
	if got.Details[0] != "ok" {
		t.Errorf("Details[0] = %v, want ok", got.Details[0])
	}
	// Details[1] is whatever fmt.Sprintf produced — we only care
	// that it survived as a non-empty string.
	if s, ok := got.Details[1].(string); !ok || s == "" {
		t.Errorf("Details[1] = %v (%T), want non-empty string",
			got.Details[1], got.Details[1])
	}
}

func TestDecode_FailSecureOnGarbage(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"not json", []byte("not-json")},
		{"wrong version", []byte(`{"v":99,"p":{}}`)},
		{"truncated payload", []byte(`{"v":1,"p":{`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decode(tc.in)
			if got == nil {
				t.Fatal("Decode returned nil on bad input")
			}
			if got.Code != CodeDecode {
				t.Errorf("Code = %q, want %q", got.Code, CodeDecode)
			}
			if !got.NonRetryable {
				t.Error("decode errors must be non-retryable")
			}
		})
	}
}

// contains is a tiny stdlib-free substring helper so the test file
// stays free of strings-pkg imports the reader doesn't expect.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
