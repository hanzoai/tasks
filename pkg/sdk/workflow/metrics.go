// Copyright © 2026 Hanzo AI. MIT License.

package workflow

import "time"

// MetricsHandler is the workflow-scoped metrics surface. It mirrors the
// shape system workers (scheduler, deletenamespace, workerdeployment)
// consume from upstream so call sites migrate by import-path swap only.
//
// All methods are safe to call from inside a workflow function. A
// noop implementation is returned when no provider is wired so workflow
// code never has to nil-check.
type MetricsHandler interface {
	// WithTags returns a derived handler that emits all metrics with
	// the supplied tags merged in. Tag values must be stable strings;
	// dynamic, per-call values still belong on individual metric calls.
	WithTags(tags map[string]string) MetricsHandler

	// Counter returns an additive metric. Counters never go down.
	Counter(name string) MetricsCounter

	// Gauge returns a sampled metric. The last value wins per tag set.
	Gauge(name string) MetricsGauge

	// Timer returns a timing metric. Record duration with Record.
	Timer(name string) MetricsTimer
}

// MetricsCounter is the additive metric handle.
type MetricsCounter interface {
	Inc(delta int64)
}

// MetricsGauge is the sampled metric handle.
type MetricsGauge interface {
	Update(value float64)
}

// MetricsTimer is the timing metric handle.
type MetricsTimer interface {
	Record(d time.Duration)
}

// GetMetricsHandler returns the workflow-scoped metrics handler. When
// no provider is wired (tests, stubs) the returned handler is a noop;
// callers can chain WithTags / Counter / Inc unconditionally.
func GetMetricsHandler(ctx Context) MetricsHandler {
	if ctx == nil {
		return noopMetrics{}
	}
	if h := ctx.env().MetricsHandler(); h != nil {
		return h
	}
	return noopMetrics{}
}

// noopMetrics is the zero-cost handler returned when no provider is
// supplied. All methods accept their inputs and discard them.
type noopMetrics struct{}

func (noopMetrics) WithTags(map[string]string) MetricsHandler { return noopMetrics{} }
func (noopMetrics) Counter(string) MetricsCounter             { return noopCounter{} }
func (noopMetrics) Gauge(string) MetricsGauge                 { return noopGauge{} }
func (noopMetrics) Timer(string) MetricsTimer                 { return noopTimer{} }

type noopCounter struct{}

func (noopCounter) Inc(int64) {}

type noopGauge struct{}

func (noopGauge) Update(float64) {}

type noopTimer struct{}

func (noopTimer) Record(time.Duration) {}
