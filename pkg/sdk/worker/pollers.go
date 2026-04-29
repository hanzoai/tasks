// Copyright © 2026 Hanzo AI. MIT License.

package worker

import (
	"context"

	"github.com/hanzoai/tasks/pkg/sdk/client"
)

// startSubscriptions wires the worker's server-push handlers and
// subscribes once per kind. Called from Start. The transport pushes
// task deliveries asynchronously after Subscribe returns; each
// delivery dispatches in a fresh goroutine bounded by the worker's
// existing semaphores so concurrency caps still apply.
func (w *workerImpl) startSubscriptions() error {
	ctx, cancel := w.ctxWithStop(context.Background())
	defer cancel()

	w.transport.OnWorkflowTask(func(task *client.WorkflowTask) {
		w.dispatchPushedWorkflowTask(task)
	})
	w.transport.OnActivityTask(func(task *client.ActivityTask) {
		w.dispatchPushedActivityTask(task)
	})
	w.transport.OnActivityResult(func(activityID string, result, failure []byte) {
		w.completeActivity(activityID, result, failure)
	})

	wfReq := client.PollWorkflowTaskRequest{
		Namespace:     w.namespace,
		TaskQueueName: w.taskQueue,
		TaskQueueKind: 0,
		Identity:      w.identity,
	}
	wfSub, err := w.transport.SubscribeWorkflowTasks(ctx, wfReq)
	if err != nil {
		return err
	}
	w.subMu.Lock()
	w.workflowSubID = wfSub
	w.subMu.Unlock()

	actReq := client.PollActivityTaskRequest{
		Namespace:     w.namespace,
		TaskQueueName: w.taskQueue,
		TaskQueueKind: 0,
		Identity:      w.identity,
	}
	actSub, err := w.transport.SubscribeActivityTasks(ctx, actReq)
	if err != nil {
		return err
	}
	w.subMu.Lock()
	w.activitySubID = actSub
	w.subMu.Unlock()

	return nil
}

// dispatchPushedWorkflowTask runs in the transport's delivery goroutine
// and hands the task off to a worker-owned goroutine bounded by the
// workflowExecSem cap so back-pressure still applies. The transport
// goroutine returns immediately so it can ack the push and accept the
// next delivery.
func (w *workerImpl) dispatchPushedWorkflowTask(task *client.WorkflowTask) {
	if w.stopped() {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ctx, cancel := w.ctxWithStop(context.Background())
		defer cancel()

		if w.workflowExecSem != nil {
			select {
			case w.workflowExecSem <- struct{}{}:
			case <-ctx.Done():
				return
			case <-w.stopCh:
				return
			}
			defer func() { <-w.workflowExecSem }()
		}
		w.dispatchWorkflowTask(ctx, task)
	}()
}

// dispatchPushedActivityTask is the activity counterpart. Rate limits
// are enforced before dispatch via activityLimiter and taskQueueLimiter,
// matching the contract the previous poll loop honoured.
func (w *workerImpl) dispatchPushedActivityTask(task *client.ActivityTask) {
	if w.stopped() {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ctx, cancel := w.ctxWithStop(context.Background())
		defer cancel()

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
	}()
}
