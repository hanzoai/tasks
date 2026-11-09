// Copyright © 2026 Hanzo AI. MIT License.

package tasks

import "time"

// WithTiming sets the cadence of the sweeps and the terms of the leases for the tests
// that run several engines on one database: a membership that lapses in seconds and
// not in fifteen of them.
func WithTiming(cfg EmbedConfig, sweep, beat, ttl time.Duration) EmbedConfig {
	cfg.timing = &timing{sweep: sweep, beat: beat, member: ttl, lead: ttl, lock: defaultTiming.lock}
	return cfg
}

// Crash stops the engine the way a killed process does: its listeners and loops
// end, and its membership is not given up, so it lapses by itself.
func (e *Embedded) Crash() {
	if e.stop != nil {
		close(e.stop)
		e.stop = nil
	}
	if e.member != nil {
		e.member.halt()
	}
	e.nodesMu.Lock()
	nodes := e.nodes
	e.nodes = nil
	e.nodesMu.Unlock()
	for _, nd := range nodes {
		nd.Stop()
	}
}
