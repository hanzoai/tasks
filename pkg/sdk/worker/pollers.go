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
		// Honour MaxConcurrentWorkflowTaskExecutionSize if set. Nil
		// semaphore = unlimited.
		if w.workflowExecSem != nil {
			select {
			case w.workflowExecSem <- struct{}{}:
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			}
		}
		w.dispatchWorkflowTask(ctx, task)
		if w.workflowExecSem != nil {
			<-w.workflowExecSem
		}
	}
}

// activityPollLoop is one activity-task poller. Structure mirrors
// workflowPollLoop; the dispatch path differs per-task-type.
//
// Rate-limit wiring (§2 gap — worker.Options rate fields):
//   - w.activityLimiter.Wait gates every dispatch to the
//     WorkerActivitiesPerSecond rate.
//   - w.taskQueueLimiter.Wait additionally gates against the
//     TaskQueueActivitiesPerSecond rate. Enforced per-worker in v1 —
//     true cross-worker coordination arrives with server-side limits.
//   - Nil limiters are a no-op (unlimited).
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
		// Block on configured rate limits before dispatching. A
		// cancelled ctx (Stop) causes Wait to return an error, which
		// we treat as "don't dispatch; loop out."
		if w.activityLimiter != nil {
			if err := w.activityLimiter.Wait(ctx); err != nil {
				return
			}
		}
		if w.taskQueueLimiter != nil {
			if err := w.taskQueueLimiter.Wait(ctx); err != nil {
				return
			}
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
