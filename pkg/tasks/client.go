// Package tasks provides the Hanzo Tasks client for Go applications.
//
// Drop-in replacement for Base's Tasks() / Cron():
//
//	// Before (Base cron):
//	e.App.Tasks().Add("settlement", "*/30 * * * * *", func() { ... })
//
//	// After (Hanzo Tasks):
//	tasks.Default().Add("settlement", "30s", func() { ... })
//
// Add() accepts both Go duration strings ("30s", "5m", "1h", "24h") and
// standard 5-field cron expressions ("0 3 * * *", "0 0 5 1,4,7,10 *",
// "*/5 * * * *"). Anything that parses as a Go duration is treated as an
// interval; anything else is treated as a cron expression.
//
// If zapAddr is set, schedules and one-shot tasks run as durable Hanzo
// Tasks over the ZAP binary transport (retries, dead letter, audit
// trail). The submit path stamps opStartWorkflow; the schedule path
// stamps opCreateSchedule — the same opcodes the full SDK client and the
// server dispatch on. If zapAddr is empty, everything runs locally via a
// goroutine timer (dev mode, same behaviour as cron but no persistence).
package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/luxfi/zap"
	"github.com/robfig/cron/v3"
)

// Handler processes a one-shot task (webhook delivery, settlement, etc.)
type Handler func(taskType string, payload map[string]any)

// Client manages both one-shot tasks and recurring schedules.
type Client struct {
	zapAddr   string
	handler   Handler
	logger    *slog.Logger
	mu        sync.RWMutex
	schedules map[string]context.CancelFunc // local schedule cancellers

	zapOnce sync.Once
	zapNode *zap.Node
	zapPeer string
	zapErr  error
}

// New creates a Client. If zapAddr is empty, everything runs locally.
// If zapAddr is set, tasks submit durably over ZAP.
func New(zapAddr string, handler Handler) *Client {
	return &Client{
		zapAddr:   zapAddr,
		handler:   handler,
		logger:    slog.Default(),
		schedules: make(map[string]context.CancelFunc),
	}
}

// ── Global singleton ────────────────────────────────────────────────────

var (
	defaultClient *Client
	defaultMu     sync.RWMutex
)

// SetDefault installs the process-wide task client. main() should call this
// once during boot. Subsequent callers use Default() to dispatch tasks.
func SetDefault(c *Client) {
	defaultMu.Lock()
	defaultClient = c
	defaultMu.Unlock()
}

// Default returns the process-wide client. If SetDefault was never called,
// lazily creates a client from the TASKS_ZAP env var so callers that
// register schedules before main() finished wiring still work. main() may
// replace this with a handler-bound client via SetDefault.
func Default() *Client {
	defaultMu.RLock()
	c := defaultClient
	defaultMu.RUnlock()
	if c != nil {
		return c
	}
	c = New(os.Getenv("TASKS_ZAP"), nil)
	defaultMu.Lock()
	if defaultClient == nil {
		defaultClient = c
	} else {
		c = defaultClient
	}
	defaultMu.Unlock()
	return c
}

// Add registers a recurring task.
//
// spec is either a Go duration ("30s", "5m", "1h") or a standard 5-field
// cron expression ("0 3 * * *", "*/5 * * * *", "0 14 8 * *"). The fn runs
// on that cadence. If zapAddr is set, creates a durable Hanzo Tasks
// schedule so retries, dead-letter and audit are handled server-side.
// Otherwise runs locally.
//
//	tasks.Default().Add("settlement.process", "30s", func() { ... })
//	tasks.Default().Add("audit.archive", "0 3 * * *", func() { ... })
func (c *Client) Add(name, spec string, fn func()) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return fmt.Errorf("taskqueue: empty schedule for %q", name)
	}

	// Go duration takes precedence when it parses cleanly.
	if dur, err := time.ParseDuration(spec); err == nil {
		return c.addInterval(name, dur, fn)
	}

	// Otherwise treat as cron expression.
	if _, err := cron.ParseStandard(spec); err != nil {
		return fmt.Errorf("taskqueue: %q is neither a duration nor a cron expression: %w", spec, err)
	}
	return c.addCron(name, spec, fn)
}

// addInterval registers a fixed-interval task. Durable over ZAP when a
// server is configured; otherwise a local ticker.
func (c *Client) addInterval(name string, dur time.Duration, fn func()) error {
	if c.zapAddr != "" {
		if err := c.scheduleIntervalZAP(name, dur); err == nil {
			return nil
		} else {
			c.logger.Warn("taskqueue: ZAP interval schedule failed, running locally",
				"name", name, "error", err)
		}
	}
	c.addLocalInterval(name, dur, fn)
	return nil
}

// addCron registers a cron-expression task. Durable over ZAP when a
// server is configured; otherwise a local schedule.
func (c *Client) addCron(name, expr string, fn func()) error {
	if c.zapAddr != "" {
		if err := c.scheduleCronZAP(name, expr); err == nil {
			return nil
		} else {
			c.logger.Warn("taskqueue: ZAP cron schedule failed, running locally",
				"name", name, "error", err)
		}
	}
	c.addLocalCron(name, expr, fn)
	return nil
}

// Now submits a one-shot task for durable execution over ZAP. When no
// server is configured, or the submit fails, the handler runs directly.
func (c *Client) Now(taskType string, payload map[string]any) error {
	if c == nil {
		return nil
	}
	if c.zapAddr == "" {
		return c.execDirect(taskType, payload)
	}
	if err := c.submitZAP(taskType, payload); err != nil {
		c.logger.Warn("taskqueue: ZAP submit failed, executing directly",
			"type", taskType, "zapAddr", c.zapAddr, "error", err)
		return c.execDirect(taskType, payload)
	}
	return nil
}

// Stop cancels all local schedules and closes ZAP connection. Call on shutdown.
func (c *Client) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, cancel := range c.schedules {
		cancel()
		delete(c.schedules, name)
	}
	if c.zapNode != nil {
		c.zapNode.Stop()
	}
}

// connectZAP lazily establishes the ZAP connection to the Tasks server.
func (c *Client) connectZAP() error {
	c.zapOnce.Do(func() {
		c.zapNode = zap.NewNode(zap.NodeConfig{
			NodeID:      "tasks-sdk",
			ServiceType: "_tasks._tcp",
			Port:        0, // ephemeral
			Logger:      c.logger,
			NoDiscovery: true,
		})
		if err := c.zapNode.Start(); err != nil {
			c.zapErr = fmt.Errorf("taskqueue: zap start: %w", err)
			return
		}
		if err := c.zapNode.ConnectDirect(c.zapAddr); err != nil {
			c.zapErr = fmt.Errorf("taskqueue: zap connect %s: %w", c.zapAddr, err)
			return
		}
		peers := c.zapNode.Peers()
		if len(peers) > 0 {
			c.zapPeer = peers[0]
		} else {
			c.zapPeer = c.zapAddr
		}
		c.logger.Info("taskqueue: ZAP connected", "addr", c.zapAddr, "peer", c.zapPeer)
	})
	return c.zapErr
}

// callDurable is the one ZAP request path shared by submit and schedule.
// It marshals body into the canonical envelope, stamps op, and reports a
// non-2xx status as an error.
func (c *Client) callDurable(op uint16, body any) error {
	if err := c.connectZAP(); err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("taskqueue: marshal: %w", err)
	}
	msg, err := wireSend(op, raw)
	if err != nil {
		return fmt.Errorf("taskqueue: build zap msg: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.zapNode.Call(ctx, c.zapPeer, msg)
	if err != nil {
		return fmt.Errorf("taskqueue: zap call: %w", err)
	}
	if status := resp.Root().Uint32(envelopeStatus); status != 0 && status != 200 {
		return fmt.Errorf("taskqueue: zap status %d: %s", status, string(resp.Root().Bytes(envelopeError)))
	}
	return nil
}

// submitZAP starts a one-shot workflow (opStartWorkflow). The task type is
// the workflow type; the payload is the workflow input.
func (c *Client) submitZAP(taskType string, payload map[string]any) error {
	return c.callDurable(opStartWorkflow, map[string]any{
		"namespace":     "default",
		"workflow_type": taskType,
		"task_queue":    "ats",
		"input":         payload,
	})
}

// scheduleIntervalZAP creates a recurring interval schedule (opCreateSchedule).
func (c *Client) scheduleIntervalZAP(name string, interval time.Duration) error {
	if err := c.callDurable(opCreateSchedule, scheduleEnvelope(name, map[string]any{
		"interval": []map[string]any{{"interval": interval.String()}},
	})); err != nil {
		return err
	}
	c.logger.Info("taskqueue: ZAP interval schedule created", "name", name, "interval", interval)
	return nil
}

// scheduleCronZAP creates a recurring cron schedule (opCreateSchedule).
func (c *Client) scheduleCronZAP(name, expr string) error {
	if err := c.callDurable(opCreateSchedule, scheduleEnvelope(name, map[string]any{
		"cron": []string{expr},
	})); err != nil {
		return err
	}
	c.logger.Info("taskqueue: ZAP cron schedule created", "name", name, "expr", expr)
	return nil
}

// scheduleEnvelope builds the create-schedule request body shared by the
// interval and cron paths. spec carries either an "interval" or a "cron"
// entry; the action starts a workflow named after the schedule.
func scheduleEnvelope(name string, spec map[string]any) map[string]any {
	return map[string]any{
		"namespace":   "default",
		"schedule_id": name,
		"schedule": map[string]any{
			"spec": spec,
			"action": map[string]any{
				"workflow_type": name,
				"task_queue":    "ats",
			},
		},
	}
}

// addLocalInterval runs fn on a ticker (dev fallback, no persistence).
func (c *Client) addLocalInterval(name string, interval time.Duration, fn func()) {
	ctx, cancel := context.WithCancel(context.Background())

	c.mu.Lock()
	if old, ok := c.schedules[name]; ok {
		old() // cancel previous
	}
	c.schedules[name] = cancel
	c.mu.Unlock()

	c.logger.Info("taskqueue: local interval schedule started", "name", name, "interval", interval)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.runFn(name, fn)
			}
		}
	}()
}

// addLocalCron runs fn on a robfig/cron schedule in a single goroutine.
func (c *Client) addLocalCron(name, expr string, fn func()) {
	schedule, err := cron.ParseStandard(expr)
	if err != nil {
		// Shouldn't happen — caller already validated.
		c.logger.Error("taskqueue: cron parse failed", "name", name, "expr", expr, "error", err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())

	c.mu.Lock()
	if old, ok := c.schedules[name]; ok {
		old()
	}
	c.schedules[name] = cancel
	c.mu.Unlock()

	c.logger.Info("taskqueue: local cron schedule started", "name", name, "expr", expr)

	go func() {
		for {
			now := time.Now()
			next := schedule.Next(now)
			wait := time.Until(next)
			if wait < 0 {
				wait = time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				c.runFn(name, fn)
			}
		}
	}()
}

// runFn invokes fn with panic recovery.
func (c *Client) runFn(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("taskqueue: schedule panic", "name", name, "panic", r)
		}
	}()
	fn()
}

// execDirect runs the handler in a goroutine (dev mode).
func (c *Client) execDirect(taskType string, payload map[string]any) error {
	c.mu.RLock()
	h := c.handler
	c.mu.RUnlock()

	if h == nil {
		c.logger.Warn("taskqueue: no handler, dropping task", "type", taskType)
		return nil
	}

	go h(taskType, payload)
	return nil
}
