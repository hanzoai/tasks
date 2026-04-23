package client

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// CreateScheduleOptions configures CreateSchedule. Mirrors schema/tasks.zap
// Schedule / ScheduleSpec / ScheduleAction on the v1 JSON wire.
type CreateScheduleOptions struct {
	ID string

	// Spec: use either Cron expressions OR a single Interval.
	CronExpressions []string
	Interval        time.Duration
	StartTime       time.Time
	EndTime         time.Time
	Jitter          time.Duration
	Timezone        string

	// Action: the workflow to start on each tick.
	WorkflowID   string
	WorkflowType string
	TaskQueue    string
	Input        []any

	Paused bool
}

// Schedule mirrors schema/tasks.zap Schedule for the v1 wire.
type Schedule struct {
	ID     string         `json:"id"`
	Spec   ScheduleSpec   `json:"spec"`
	Action ScheduleAction `json:"action"`
	Paused bool           `json:"paused,omitempty"`
}

// ScheduleSpec mirrors schema/tasks.zap ScheduleSpec on the wire. Durations
// are exposed as Go time.Duration on the Go surface and as milliseconds
// on the wire.
type ScheduleSpec struct {
	Cron      []string      `json:"-"`
	Interval  time.Duration `json:"-"`
	StartTime time.Time     `json:"-"`
	EndTime   time.Time     `json:"-"`
	Jitter    time.Duration `json:"-"`
	Timezone  string        `json:"-"`
}

type scheduleSpecWire struct {
	Cron        []string `json:"cron,omitempty"`
	IntervalMs  int64    `json:"interval_ms,omitempty"`
	StartTimeMs int64    `json:"start_time_ms,omitempty"`
	EndTimeMs   int64    `json:"end_time_ms,omitempty"`
	JitterMs    int64    `json:"jitter_ms,omitempty"`
	Timezone    string   `json:"timezone,omitempty"`
}

// MarshalJSON implements the ms-on-the-wire contract.
func (s ScheduleSpec) MarshalJSON() ([]byte, error) {
	w := scheduleSpecWire{
		Cron:       append([]string(nil), s.Cron...),
		IntervalMs: s.Interval.Milliseconds(),
		JitterMs:   s.Jitter.Milliseconds(),
		Timezone:   s.Timezone,
	}
	if !s.StartTime.IsZero() {
		w.StartTimeMs = s.StartTime.UnixMilli()
	}
	if !s.EndTime.IsZero() {
		w.EndTimeMs = s.EndTime.UnixMilli()
	}
	return jsonMarshal(w)
}

// UnmarshalJSON implements the ms-on-the-wire contract.
func (s *ScheduleSpec) UnmarshalJSON(b []byte) error {
	var w scheduleSpecWire
	if err := jsonUnmarshal(b, &w); err != nil {
		return err
	}
	s.Cron = append([]string(nil), w.Cron...)
	s.Interval = time.Duration(w.IntervalMs) * time.Millisecond
	s.Jitter = time.Duration(w.JitterMs) * time.Millisecond
	s.Timezone = w.Timezone
	if w.StartTimeMs != 0 {
		s.StartTime = time.UnixMilli(w.StartTimeMs)
	}
	if w.EndTimeMs != 0 {
		s.EndTime = time.UnixMilli(w.EndTimeMs)
	}
	return nil
}

// ScheduleAction mirrors schema/tasks.zap ScheduleAction.
type ScheduleAction struct {
	WorkflowID   string `json:"workflow_id,omitempty"`
	WorkflowType string `json:"workflow_type"`
	TaskQueue    string `json:"task_queue"`
	Input        []any  `json:"input,omitempty"`
}

type createScheduleRequest struct {
	Namespace  string   `json:"namespace"`
	ScheduleID string   `json:"schedule_id"`
	Schedule   Schedule `json:"schedule"`
}

type listSchedulesRequest struct {
	Namespace     string `json:"namespace"`
	PageSize      int32  `json:"page_size,omitempty"`
	NextPageToken []byte `json:"next_page_token,omitempty"`
}

// ListSchedulesResponse is returned by ListSchedules.
type ListSchedulesResponse struct {
	Schedules     []Schedule `json:"schedules"`
	NextPageToken []byte     `json:"next_page_token,omitempty"`
}

type deleteScheduleRequest struct {
	Namespace  string `json:"namespace"`
	ScheduleID string `json:"schedule_id"`
}

type pauseScheduleRequest struct {
	Namespace  string `json:"namespace"`
	ScheduleID string `json:"schedule_id"`
	Paused     bool   `json:"paused"`
}

// CreateSchedule implements Client.
func (c *clientImpl) CreateSchedule(ctx context.Context, opts CreateScheduleOptions) error {
	if opts.ID == "" {
		return errors.New("hanzo/tasks/client: schedule ID is required")
	}
	if opts.WorkflowType == "" {
		return errors.New("hanzo/tasks/client: schedule action WorkflowType is required")
	}
	if opts.TaskQueue == "" {
		return errors.New("hanzo/tasks/client: schedule action TaskQueue is required")
	}
	if len(opts.CronExpressions) == 0 && opts.Interval == 0 {
		return errors.New("hanzo/tasks/client: schedule spec needs CronExpressions or Interval")
	}

	req := createScheduleRequest{
		Namespace:  c.namespace,
		ScheduleID: opts.ID,
		Schedule: Schedule{
			ID: opts.ID,
			Spec: ScheduleSpec{
				Cron:      opts.CronExpressions,
				Interval:  opts.Interval,
				StartTime: opts.StartTime,
				EndTime:   opts.EndTime,
				Jitter:    opts.Jitter,
				Timezone:  opts.Timezone,
			},
			Action: ScheduleAction{
				WorkflowID:   opts.WorkflowID,
				WorkflowType: opts.WorkflowType,
				TaskQueue:    opts.TaskQueue,
				Input:        opts.Input,
			},
			Paused: opts.Paused,
		},
	}
	return c.roundTrip(ctx, opCreateSchedule, req, nil)
}

// ListSchedules implements Client.
func (c *clientImpl) ListSchedules(ctx context.Context, pageSize int32, nextPageToken []byte) (*ListSchedulesResponse, error) {
	var resp ListSchedulesResponse
	req := listSchedulesRequest{
		Namespace:     c.namespace,
		PageSize:      pageSize,
		NextPageToken: nextPageToken,
	}
	if err := c.roundTrip(ctx, opListSchedules, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeleteSchedule implements Client.
func (c *clientImpl) DeleteSchedule(ctx context.Context, scheduleID string) error {
	if scheduleID == "" {
		return errors.New("hanzo/tasks/client: schedule ID is required")
	}
	return c.roundTrip(ctx, opDeleteSchedule, deleteScheduleRequest{
		Namespace:  c.namespace,
		ScheduleID: scheduleID,
	}, nil)
}

// PauseSchedule implements Client.
func (c *clientImpl) PauseSchedule(ctx context.Context, scheduleID string, paused bool) error {
	if scheduleID == "" {
		return errors.New("hanzo/tasks/client: schedule ID is required")
	}
	return c.roundTrip(ctx, opPauseSchedule, pauseScheduleRequest{
		Namespace:  c.namespace,
		ScheduleID: scheduleID,
		Paused:     paused,
	}, nil)
}

// Health implements Client.
func (c *clientImpl) Health(ctx context.Context) (service, status string, err error) {
	resp, herr := c.CheckHealth(ctx, nil)
	if herr != nil {
		return "", "", herr
	}
	return resp.Service, resp.Status, nil
}

// CheckHealth implements Client. Backed by opcode 0x0090. A nil req is
// treated as an empty request (the v1 wire reserves the request body
// for future filters).
func (c *clientImpl) CheckHealth(ctx context.Context, req *CheckHealthRequest) (*CheckHealthResponse, error) {
	if req == nil {
		req = &CheckHealthRequest{}
	}
	_ = req // empty in v1 — present so round-trip uses the canonical shape
	var resp CheckHealthResponse
	if err := c.roundTrip(ctx, opHealth, struct{}{}, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Internal helpers to keep json usage consistent and testable. These
// exist so every struct with a custom ms-on-the-wire marshaller uses
// the same encoding path.
func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
