// Copyright © 2026 Hanzo AI. MIT License.

package examples

import (
	"context"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
)

type CronResult struct {
	ScheduleID  string
	ListedCount int
	Paused      bool
	Resumed     bool
}

// Cron creates a schedule, lists schedules, pauses + unpauses it,
// then deletes it.
func Cron(ctx context.Context, c client.Client, _ string) (*CronResult, error) {
	id := "cron-" + time.Now().UTC().Format("150405.000000")
	if err := c.CreateSchedule(ctx, client.CreateScheduleOptions{
		ID:              id,
		CronExpressions: []string{"0 3 * * *"},
		WorkflowType:    "DailyCleanup",
		TaskQueue:       "schedules",
	}); err != nil {
		return nil, err
	}
	res := &CronResult{ScheduleID: id}

	list, err := c.ListSchedules(ctx, 100, nil)
	if err != nil {
		return nil, err
	}
	if list != nil {
		for _, s := range list.Schedules {
			if s.ID == id {
				res.ListedCount++
			}
		}
	}

	if err := c.PauseSchedule(ctx, id, true); err == nil {
		res.Paused = true
	}
	if err := c.UnpauseSchedule(ctx, id); err == nil {
		res.Resumed = true
	}

	_ = c.DeleteSchedule(ctx, id)
	return res, nil
}
