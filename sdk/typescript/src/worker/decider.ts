// Copyright © 2026 Hanzo AI. MIT License.
//
// Workflow-task decider. One episode: seed a DeciderContext from history,
// replay the registered workflow function, drain microtasks, and translate
// the outcome into commands. Faithful async port of
// pkg/sdk/worker/dispatch.go:dispatchWorkflowTask + runWorkflowEpisode.

import {
  completeWorkflowCommand,
  failWorkflowCommand,
  parseHistory,
  workflowInput,
  type RawCommand,
} from "./history";
import {
  DeciderContext,
  runWithContext,
  type WorkflowInfo,
} from "../workflow/runtime";

export type WorkflowFn = (...args: any[]) => unknown | Promise<unknown>;

/**
 * Replay `fn` for one workflow-task episode and return the commands to ship
 * back. Returns only the schedule commands when the workflow parks on a
 * durable primitive (activity/timer) whose result is not yet in history.
 */
export async function runWorkflowEpisode(
  fn: WorkflowFn,
  info: WorkflowInfo,
  historyStr: string,
): Promise<RawCommand[]> {
  const events = parseHistory(historyStr);
  const ctx = new DeciderContext(info);
  ctx.loadHistory(events);
  const args = workflowInput(events);

  let settled = false;
  let isError = false;
  let value: unknown;
  let error: unknown;

  let promise: Promise<unknown>;
  try {
    promise = Promise.resolve(runWithContext(ctx, () => fn(...args)));
  } catch (e) {
    // Synchronous throw before the first await.
    return [...ctx.newCommands, failWorkflowCommand(e)];
  }
  promise.then(
    (v) => {
      settled = true;
      value = v;
    },
    (e) => {
      settled = true;
      isError = true;
      error = e;
    },
  );

  // A history-resolved replay settles entirely through microtasks, which are
  // fully drained before a `setImmediate` macrotask fires. Give a small
  // margin; if still unsettled the workflow is parked on a durable primitive.
  for (let i = 0; i < 3 && !settled; i++) {
    await new Promise<void>((resolve) => setImmediate(resolve));
  }

  const commands = ctx.newCommands.slice();
  if (!settled) {
    return commands; // parked — advance on the next task with results appended
  }
  if (isError) {
    return [...commands, failWorkflowCommand(error)];
  }
  return [...commands, completeWorkflowCommand(value)];
}
