// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"
	"time"

	"github.com/hanzoai/tasks/pkg/sdk/client"
)

// pollerErrorBackoff is the pause between retries when PollWorkflowTask
// / PollActivityTask returns a non-nil error. The frontend disconnect
// / transient-error path is the only reason a poll returns err; a
// recovered backend re-accepts polls on the next tick.
const pollerErrorBackoff = 250 * time.Millisecond

// workflowPollLoop is one workflow-task poller. It runs until the
// worker stopCh closes; each iteration long-polls the frontend, and
// on a non-nil task dispatches it synchronously before re-polling.
func (w *workerImpl) workflowPollLoop(id int) {
	defer w.wg.Done()
	ctx, cancel := w.ctxWithStop(context.Background())
	defer cancel()

	req := client.PollWorkflowTaskRequest{
		Namespace:     w.namespace,
		TaskQueueName: w.taskQueue,
		TaskQueueKind: 0, // normal
		Identity:      w.identity,
	}

	for !w.stopped() {
		task, err := w.transport.PollWorkflowTask(ctx, req)
		if w.stopped() {
			return
		}
		if err != nil {
			if errIsFatal(err) {
				return
			}
			w.logger.Debug("workflow poll error",
				"poller", id, "err", err)
			sleepOrStop(ctx, pollerErrorBackoff, w.stopCh)
			continue
		}
		if task == nil {
			// Idle. Re-poll immediately.
			continue
		}
		w.dispatchWorkflowTask(ctx, task)
	}
}

// activityPollLoop is one activity-task poller. Structure mirrors
// workflowPollLoop; the dispatch path differs per-task-type.
func (w *workerImpl) activityPollLoop(id int) {
	defer w.wg.Done()
	ctx, cancel := w.ctxWithStop(context.Background())
	defer cancel()

	req := client.PollActivityTaskRequest{
		Namespace:     w.namespace,
		TaskQueueName: w.taskQueue,
		TaskQueueKind: 0,
		Identity:      w.identity,
	}

	for !w.stopped() {
		task, err := w.transport.PollActivityTask(ctx, req)
		if w.stopped() {
			return
		}
		if err != nil {
			if errIsFatal(err) {
				return
			}
			w.logger.Debug("activity poll error",
				"poller", id, "err", err)
			sleepOrStop(ctx, pollerErrorBackoff, w.stopCh)
			continue
		}
		if task == nil {
			continue
		}
		w.dispatchActivityTask(ctx, task)
	}
}

// sleepOrStop pauses for d or returns early if stopCh closes.
func sleepOrStop(ctx context.Context, d time.Duration, stopCh <-chan struct{}) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	case <-stopCh:
	}
}

// errIsFatal reports whether an error from a poll should terminate
// the poller permanently (rather than back off and retry). Phase 1
// treats only context.Canceled as fatal; transport errors are always
// transient because the frontend typically comes back. Phase 2 may
// classify credential / namespace errors as fatal.
func errIsFatal(err error) bool {
	return err == context.Canceled
}
