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
// If TASKS_URL is set, schedules run as durable Hanzo Tasks workflows
// (retries, dead letter, audit trail). If not, runs locally via goroutine
// timer (dev mode, same behaviour as cron but no persistence).
//
// If zapAddr is set, ZAP binary transport is preferred over HTTP for
// submitting tasks (lower latency, same semantics). HTTP is fallback.
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/luxfi/zap"
	"github.com/robfig/cron/v3"
)

// ZAP opcodes for task submission.
const (
	OpcodeTaskSubmit   uint16 = 0x0050 // one-shot task
	OpcodeTaskSchedule uint16 = 0x0051 // recurring schedule
)

// ZAP object field offsets.
const (
	fieldTaskType = 0  // Text: task/workflow type
	fieldPayload  = 8  // Bytes: JSON payload
	fieldInterval = 16 // Text: duration string (schedules only)
	fieldCron     = 24 // Text: cron expression (schedules only, alt to interval)
	fieldName     = 0  // Text: schedule name (schedules)
	respStatus    = 0  // Uint32: status code
	respBody      = 4  // Bytes: response JSON
)

// Handler processes a one-shot task (webhook delivery, settlement, etc.)
type Handler func(taskType string, payload map[string]any)

// Client manages both one-shot tasks and recurring schedules.
type Client struct {
	tasksURL   string
	zapAddr    string
	httpClient *http.Client
	handler    Handler
	logger     *slog.Logger
	mu         sync.RWMutex
	schedules  map[string]context.CancelFunc // local schedule cancellers

	zapOnce sync.Once
	zapNode *zap.Node
	zapPeer string
	zapErr  error
}

// New creates a Client.
// If both tasksURL and zapAddr are empty, everything runs locally.
// If zapAddr is set, ZAP transport is preferred. HTTP is fallback.
func New(tasksURL, zapAddr string, handler Handler) *Client {
	return &Client{
		tasksURL: tasksURL,
		zapAddr:  zapAddr,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
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
// lazily creates a client from TASKS_URL / TASKS_ZAP env vars so callers that
// register schedules before main() finished wiring still work. main() may
// replace this with a handler-bound client via SetDefault.
func Default() *Client {
	defaultMu.RLock()
	c := defaultClient
	defaultMu.RUnlock()
	if c != nil {
		return c
	}
	c = New(os.Getenv("TASKS_URL"), os.Getenv("TASKS_ZAP"), nil)
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
// on that cadence. If TASKS_URL is set, creates a durable Hanzo Tasks schedule
// so retries, dead-letter and audit are handled server-side. Otherwise
// runs locally.
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

// addInterval registers a fixed-interval task.
func (c *Client) addInterval(name string, dur time.Duration, fn func()) error {
	if c.zapAddr != "" || c.tasksURL != "" {
		scheduled := false
		if c.zapAddr != "" {
			if err := c.scheduleIntervalZAP(name, dur); err == nil {
				scheduled = true
			} else {
				c.logger.Warn("taskqueue: ZAP interval schedule failed", "name", name, "error", err)
			}
		}
		if !scheduled && c.tasksURL != "" {
			if err := c.createIntervalSchedule(name, dur); err != nil {
				c.logger.Warn("taskqueue: durable interval schedule failed, falling back to local",
					"name", name, "error", err)
				c.addLocalInterval(name, dur, fn)
			}
			return nil
		} else if !scheduled {
			c.addLocalInterval(name, dur, fn)
		}
		return nil
	}
	c.addLocalInterval(name, dur, fn)
	return nil
}

// addCron registers a cron-expression task.
func (c *Client) addCron(name, expr string, fn func()) error {
	if c.zapAddr != "" || c.tasksURL != "" {
		scheduled := false
		if c.zapAddr != "" {
			if err := c.scheduleCronZAP(name, expr); err == nil {
				scheduled = true
			} else {
				c.logger.Warn("taskqueue: ZAP cron schedule failed", "name", name, "error", err)
			}
		}
		if !scheduled && c.tasksURL != "" {
			if err := c.createCronSchedule(name, expr); err != nil {
				c.logger.Warn("taskqueue: durable cron schedule failed, falling back to local",
					"name", name, "error", err)
				c.addLocalCron(name, expr, fn)
			}
			return nil
		} else if !scheduled {
			c.addLocalCron(name, expr, fn)
		}
		return nil
	}
	c.addLocalCron(name, expr, fn)
	return nil
}

// Now submits a one-shot task for durable execution.
// Prefers ZAP transport when zapAddr is configured, falls back to HTTP.
func (c *Client) Now(taskType string, payload map[string]any) error {
	if c == nil {
		return nil
	}
	if c.tasksURL == "" && c.zapAddr == "" {
		return c.execDirect(taskType, payload)
	}
	if c.zapAddr != "" {
		if err := c.submitZAP(taskType, payload); err == nil {
			return nil
		}
		c.logger.Warn("taskqueue: ZAP submit failed, falling back to HTTP",
			"type", taskType, "zapAddr", c.zapAddr)
	}
	if c.tasksURL != "" {
		return c.submitHTTP(taskType, payload)
	}
	return c.execDirect(taskType, payload)
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
			ServiceType: "_tasks-sdk._tcp",
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

// submitZAP sends a one-shot task over ZAP (opcode 0x0050).
func (c *Client) submitZAP(taskType string, payload map[string]any) error {
	if err := c.connectZAP(); err != nil {
		return err
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("taskqueue: marshal: %w", err)
	}

	b := zap.NewBuilder(len(taskType) + len(payloadBytes) + 64)
	obj := b.StartObject(24)
	obj.SetText(fieldTaskType, taskType)
	obj.SetBytes(fieldPayload, payloadBytes)
	obj.FinishAsRoot()
	flags := uint16(OpcodeTaskSubmit) << 8
	data := b.FinishWithFlags(flags)

	msg, err := zap.Parse(data)
	if err != nil {
		return fmt.Errorf("taskqueue: build zap msg: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.zapNode.Call(ctx, c.zapPeer, msg)
	if err != nil {
		return fmt.Errorf("taskqueue: zap call: %w", err)
	}

	status := resp.Root().Uint32(respStatus)
	if status != 0 && status != 200 {
		body := resp.Root().Bytes(respBody)
		return fmt.Errorf("taskqueue: zap status %d: %s", status, string(body))
	}
	return nil
}

// scheduleIntervalZAP creates a recurring interval schedule over ZAP (opcode 0x0051).
func (c *Client) scheduleIntervalZAP(name string, interval time.Duration) error {
	if err := c.connectZAP(); err != nil {
		return err
	}

	b := zap.NewBuilder(len(name) + 64)
	obj := b.StartObject(32)
	obj.SetText(fieldName, name)
	obj.SetText(fieldInterval, interval.String())
	obj.FinishAsRoot()
	flags := uint16(OpcodeTaskSchedule) << 8
	data := b.FinishWithFlags(flags)

	msg, err := zap.Parse(data)
	if err != nil {
		return fmt.Errorf("taskqueue: build zap msg: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.zapNode.Call(ctx, c.zapPeer, msg)
	if err != nil {
		return fmt.Errorf("taskqueue: zap call: %w", err)
	}

	status := resp.Root().Uint32(respStatus)
	if status != 0 && status != 200 {
		body := resp.Root().Bytes(respBody)
		return fmt.Errorf("taskqueue: zap status %d: %s", status, string(body))
	}

	c.logger.Info("taskqueue: ZAP interval schedule created", "name", name, "interval", interval)
	return nil
}

// scheduleCronZAP creates a recurring cron schedule over ZAP (opcode 0x0051).
func (c *Client) scheduleCronZAP(name, expr string) error {
	if err := c.connectZAP(); err != nil {
		return err
	}

	b := zap.NewBuilder(len(name) + len(expr) + 64)
	obj := b.StartObject(32)
	obj.SetText(fieldName, name)
	obj.SetText(fieldCron, expr)
	obj.FinishAsRoot()
	flags := uint16(OpcodeTaskSchedule) << 8
	data := b.FinishWithFlags(flags)

	msg, err := zap.Parse(data)
	if err != nil {
		return fmt.Errorf("taskqueue: build zap msg: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := c.zapNode.Call(ctx, c.zapPeer, msg)
	if err != nil {
		return fmt.Errorf("taskqueue: zap call: %w", err)
	}

	status := resp.Root().Uint32(respStatus)
	if status != 0 && status != 200 {
		body := resp.Root().Bytes(respBody)
		return fmt.Errorf("taskqueue: zap status %d: %s", status, string(body))
	}

	c.logger.Info("taskqueue: ZAP cron schedule created", "name", name, "expr", expr)
	return nil
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

// createIntervalSchedule creates a durable interval schedule on Hanzo Tasks.
func (c *Client) createIntervalSchedule(name string, interval time.Duration) error {
	schedule := map[string]any{
		"schedule_id": name,
		"schedule": map[string]any{
			"spec": map[string]any{
				"interval": []map[string]any{
					{"every": interval.String()},
				},
			},
			"action": c.workflowAction(name),
		},
	}
	return c.putSchedule(name, schedule, fmt.Sprintf("interval=%s", interval))
}

// createCronSchedule creates a durable cron schedule on Hanzo Tasks. The
// server parses the standard 5-field expression into a ScheduleSpec.
func (c *Client) createCronSchedule(name, expr string) error {
	schedule := map[string]any{
		"schedule_id": name,
		"schedule": map[string]any{
			"spec": map[string]any{
				"cron_expressions": []string{expr},
			},
			"action": c.workflowAction(name),
		},
	}
	return c.putSchedule(name, schedule, fmt.Sprintf("cron=%q", expr))
}

// workflowAction returns the start_workflow action block shared by both
// interval and cron schedules.
func (c *Client) workflowAction(name string) map[string]any {
	return map[string]any{
		"start_workflow": map[string]any{
			"workflow_type": name,
			"task_queue":    "ats",
		},
	}
}

func (c *Client) putSchedule(name string, schedule map[string]any, detail string) error {
	body, err := json.Marshal(schedule)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	url := c.tasksURL + "/api/v1/namespaces/default/schedules/" + name
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		c.logger.Info("taskqueue: durable schedule created", "name", name, "detail", detail)
		return nil
	}
	return fmt.Errorf("status %d", resp.StatusCode)
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

// submitHTTP posts a one-shot task to the Hanzo Tasks HTTP API.
func (c *Client) submitHTTP(taskType string, payload map[string]any) error {
	envelope := map[string]any{
		"workflow_type": taskType,
		"task_queue":    "ats",
		"input":         payload,
	}

	body, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("taskqueue: marshal: %w", err)
	}

	url := c.tasksURL + "/api/v1/namespaces/default/workflows"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("taskqueue: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Warn("taskqueue: server unreachable, executing directly",
			"type", taskType, "error", err)
		return c.execDirect(taskType, payload)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	c.logger.Warn("taskqueue: server error, executing directly",
		"type", taskType, "status", resp.StatusCode)
	return c.execDirect(taskType, payload)
}
