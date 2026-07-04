// Copyright © 2026 Hanzo AI. MIT License.
//
// Workflow-side primitives. Import these inside workflow functions. The API
// deliberately mirrors @temporalio/workflow so a Postiz/NestJS migration is a
// dependency swap. Semantics are the Hanzo Tasks event-sourced replay model
// (see runtime.ts).

import { randomUUID } from "node:crypto";
import { toMs, type Duration } from "../common/duration";
import {
  currentContext,
  ContinueAsNewNotSupported,
  type ActivityOptions,
  type WorkflowInfo,
} from "./runtime";

export {
  ContinueAsNewNotSupported,
  type ActivityOptions,
  type WorkflowInfo,
  type DeciderContext,
} from "./runtime";

// ── activities ──

/** Options accepted by proxyActivities (superset: ms numbers or durations). */
export interface ProxyActivityOptions {
  startToCloseTimeout?: Duration;
  scheduleToCloseTimeout?: Duration;
  heartbeatTimeout?: Duration;
  taskQueue?: string;
  retry?: {
    initialInterval?: Duration;
    backoffCoefficient?: number;
    maximumInterval?: Duration;
    maximumAttempts?: number;
    nonRetryableErrorTypes?: string[];
  };
}

function normalizeActivityOptions(opts: ProxyActivityOptions): ActivityOptions {
  return {
    startToCloseTimeoutMs: opts.startToCloseTimeout !== undefined ? toMs(opts.startToCloseTimeout) : undefined,
    scheduleToCloseTimeoutMs:
      opts.scheduleToCloseTimeout !== undefined ? toMs(opts.scheduleToCloseTimeout) : undefined,
    heartbeatTimeoutMs: opts.heartbeatTimeout !== undefined ? toMs(opts.heartbeatTimeout) : undefined,
    taskQueue: opts.taskQueue,
    retry: opts.retry
      ? {
          initialIntervalMs: opts.retry.initialInterval !== undefined ? toMs(opts.retry.initialInterval) : undefined,
          backoffCoefficient: opts.retry.backoffCoefficient,
          maximumIntervalMs: opts.retry.maximumInterval !== undefined ? toMs(opts.retry.maximumInterval) : undefined,
          maximumAttempts: opts.retry.maximumAttempts,
          nonRetryableErrorTypes: opts.retry.nonRetryableErrorTypes,
        }
      : undefined,
  };
}

/**
 * Create typed activity stubs. Calling a stub schedules a durable activity
 * and returns a Promise that resolves with its result (or rejects with a
 * TaskError). `A` is an interface of activity functions.
 */
export function proxyActivities<A extends Record<string, (...args: any[]) => any>>(
  opts: ProxyActivityOptions,
): A {
  const normalized = normalizeActivityOptions(opts);
  return new Proxy(
    {},
    {
      get(_target, prop) {
        if (typeof prop !== "string") return undefined;
        return (...args: unknown[]) => currentContext().scheduleActivity(prop, args, normalized);
      },
    },
  ) as A;
}

// ── timers ──

/** Durable sleep. Backed by an internal sleeper activity (see README). */
export function sleep(duration: Duration): Promise<void> {
  return currentContext().sleep(toMs(duration));
}

/** Alias matching @temporalio's Timer semantics. */
export function startTimer(duration: Duration): Promise<void> {
  return sleep(duration);
}

// ── condition ──

/** Resolve when `fn()` becomes true, or false when the optional timeout fires. */
export function condition(fn: () => boolean, timeout?: Duration): Promise<boolean> {
  return currentContext().condition(fn, timeout !== undefined ? toMs(timeout) : undefined);
}

// ── signals & queries ──

export interface SignalDefinition<Args extends any[] = []> {
  type: "signal";
  name: string;
  readonly _args?: Args;
}

export interface QueryDefinition<Ret = unknown, Args extends any[] = []> {
  type: "query";
  name: string;
  readonly _ret?: Ret;
  readonly _args?: Args;
}

export function defineSignal<Args extends any[] = []>(name: string): SignalDefinition<Args> {
  return { type: "signal", name };
}

export function defineQuery<Ret = unknown, Args extends any[] = []>(name: string): QueryDefinition<Ret, Args> {
  return { type: "query", name };
}

/** Register a signal or query handler. */
export function setHandler<Args extends any[], Ret>(
  def: SignalDefinition<Args> | QueryDefinition<Ret, Args>,
  handler: (...args: Args) => Ret | void,
): void {
  const ctx = currentContext();
  if (def.type === "signal") {
    ctx.registerSignalHandler(def.name, (arg: unknown) => {
      const args = (Array.isArray(arg) ? arg : [arg]) as Args;
      handler(...args);
    });
  } else {
    ctx.registerQueryHandler(def.name, (...args: unknown[]) => handler(...(args as Args)) as Ret);
  }
}

// ── info & versioning ──

export function workflowInfo(): WorkflowInfo {
  return currentContext().info;
}

export function getVersion(changeId: string, minSupported: number, maxSupported: number): number {
  return currentContext().getVersion(changeId, minSupported, maxSupported);
}

export function upsertSearchAttributes(attrs: Record<string, unknown>): void {
  currentContext().upsertSearchAttributes(attrs);
}

/**
 * continueAsNew — API-present, not yet durable on the v1 wire (no server
 * command). Throws ContinueAsNewNotSupported at runtime. See README.
 */
export function continueAsNew<_F extends (...args: any[]) => any>(..._args: unknown[]): Promise<never> {
  return Promise.reject(new ContinueAsNewNotSupported(currentContext().info.workflowType));
}

/** Deterministic-friendly UUID (delegates to crypto; document determinism). */
export function uuid4(): string {
  return randomUUID();
}
