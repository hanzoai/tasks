package scheduler

import (
	"time"

	schedulespb "github.com/hanzoai/tasks/api/schedule/v1"
	"github.com/hanzoai/tasks/chasm"
)

// Export unexported methods for testing.

func (s *Scheduler) RecordCompletedAction(
	ctx chasm.MutableContext,
	completed *schedulespb.CompletedResult,
	requestID string,
) time.Time {
	invoker := s.Invoker.Get(ctx)
	return invoker.recordCompletedAction(ctx, completed, requestID)
}

func (i *Invoker) RunningWorkflowID(requestID string) string {
	return i.runningWorkflowID(requestID)
}
