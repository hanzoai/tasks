// Copyright © 2026 Hanzo AI. MIT License.
//
// Activity execution context — heartbeat, info, cancellation. Mirrors
// pkg/sdk/activity (Go) and @temporalio/activity's Context.

import { AsyncLocalStorage } from "node:async_hooks";

export interface ActivityInfo {
  taskToken: Buffer;
  workflowId: string;
  runId: string;
  activityId: string;
  activityType: string;
  taskQueue: string;
  attempt: number;
  scheduledTimeMs: number;
  startedTimeMs: number;
}

export class ActivityContext {
  readonly info: ActivityInfo;
  private readonly heartbeatSink: (details: unknown[]) => void;
  private readonly abort: AbortController;

  constructor(info: ActivityInfo, heartbeatSink: (details: unknown[]) => void, abort: AbortController) {
    this.info = info;
    this.heartbeatSink = heartbeatSink;
    this.abort = abort;
  }

  /** Record a heartbeat with optional details. */
  heartbeat(...details: unknown[]): void {
    this.heartbeatSink(details);
  }

  /** True once the server requested cancellation via a heartbeat response. */
  get cancelled(): boolean {
    return this.abort.signal.aborted;
  }

  /** AbortSignal that fires when the server requests cancellation. */
  get cancellationSignal(): AbortSignal {
    return this.abort.signal;
  }
}

const storage = new AsyncLocalStorage<ActivityContext>();

/** The current activity's context. Throws outside an activity. */
export function activityContext(): ActivityContext {
  const ctx = storage.getStore();
  if (!ctx) throw new Error("hanzo/tasks: activityContext() called outside an activity");
  return ctx;
}

/** Convenience: current activity info. */
export function activityInfo(): ActivityInfo {
  return activityContext().info;
}

/** Record a heartbeat from the current activity. */
export function heartbeat(...details: unknown[]): void {
  activityContext().heartbeat(...details);
}

export function runInActivityContext<T>(ctx: ActivityContext, fn: () => T): T {
  return storage.run(ctx, fn);
}
