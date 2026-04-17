// Package taskqueue provides a durable task client for Hanzo Tasks.
//
// Drop-in replacement for Base's Cron():
//
//	// Before (cron):
//	e.App.Cron().Add("settlement", "*/30 * * * * *", func() { ... })
//
//	// After (tasks):
//	tasks.Add("settlement", "30s", func() { ... })
//
// If TASKS_URL is set, schedules run as durable Temporal workflows
// (retries, dead letter, audit trail). If not, runs locally via goroutine
// timer (dev mode, same behavior as cron but no persistence).
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Handler processes a one-shot task (webhook delivery, settlement, etc.)
type Handler func(taskType string, payload map[string]any)

// Client manages both one-shot tasks and recurring schedules.
type Client struct {
	tasksURL   string
	httpClient *http.Client
	handler    Handler
	logger     *slog.Logger
	mu         sync.RWMutex
	schedules  map[string]context.CancelFunc // local schedule cancellers
}

// New creates a Client. If tasksURL is empty, everything runs locally.
func New(tasksURL string, handler Handler) *Client {
	return &Client{
		tasksURL: tasksURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		handler:   handler,
		logger:    slog.Default(),
		schedules: make(map[string]context.CancelFunc),
	}
}

// Add registers a recurring task. interval is a duration string ("30s", "5m", "1h").
// The fn runs every interval. If TASKS_URL is set, creates a durable Temporal schedule.
// Otherwise runs a local ticker (same as cron but cleaner).
//
//	tasks.Add("settlement.process", "30s", func() { processSettlements() })
//	tasks.Add("oracle.update", "5m", func() { updateOracle() })
func (c *Client) Add(name, interval string, fn func()) error {
	dur, err := time.ParseDuration(interval)
	if err != nil {
		return fmt.Errorf("taskqueue: invalid interval %q: %w", interval, err)
	}

	if c.tasksURL != "" {
		// Create durable schedule on Hanzo Tasks
		if err := c.createSchedule(name, dur); err != nil {
			c.logger.Warn("taskqueue: failed to create schedule, falling back to local",
				"name", name, "error", err)
			c.addLocal(name, dur, fn)
		}
		return nil
	}

	c.addLocal(name, dur, fn)
	return nil
}

// Enqueue submits a one-shot task for durable execution.
func (c *Client) Now(taskType string, payload map[string]any) error {
	if c == nil {
		return nil
	}
	if c.tasksURL == "" {
		return c.execDirect(taskType, payload)
	}
	return c.submitHTTP(taskType, payload)
}

// Stop cancels all local schedules. Call on shutdown.
func (c *Client) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, cancel := range c.schedules {
		cancel()
		delete(c.schedules, name)
	}
}

// addLocal runs fn on a ticker (dev fallback, no persistence).
func (c *Client) addLocal(name string, interval time.Duration, fn func()) {
	ctx, cancel := context.WithCancel(context.Background())

	c.mu.Lock()
	if old, ok := c.schedules[name]; ok {
		old() // cancel previous
	}
	c.schedules[name] = cancel
	c.mu.Unlock()

	c.logger.Info("taskqueue: local schedule started", "name", name, "interval", interval)

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				func() {
					defer func() {
						if r := recover(); r != nil {
							c.logger.Error("taskqueue: schedule panic", "name", name, "panic", r)
						}
					}()
					fn()
				}()
			}
		}
	}()
}

// createSchedule creates a durable schedule on Hanzo Tasks.
func (c *Client) createSchedule(name string, interval time.Duration) error {
	schedule := map[string]any{
		"schedule_id": name,
		"schedule": map[string]any{
			"spec": map[string]any{
				"interval": []map[string]any{
					{"every": interval.String()},
				},
			},
			"action": map[string]any{
				"start_workflow": map[string]any{
					"workflow_type": name,
					"task_queue":    "ats",
				},
			},
		},
	}

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
		c.logger.Info("taskqueue: durable schedule created", "name", name, "interval", interval)
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
