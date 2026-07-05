// Copyright © 2026 Hanzo AI. MIT License.
//
// Workflow replay runtime. Faithful async port of pkg/sdk/worker/env.go's
// event-sourced replay decider.
//
// Model: on each workflow task the server delivers the run's full history.
// The registered workflow function is re-run from the top. Each durable
// primitive (activity, timer) is assigned a deterministic per-run sequence
// number in program order. If its outcome is already in history it resolves
// immediately; otherwise the decider emits a command and the primitive's
// promise stays PENDING FOREVER — the async function suspends, the episode
// ends with the emitted commands, and the run advances when the server
// delivers the next task with the outcome appended.
//
// Suspending on a never-settling promise (rather than throwing a sentinel)
// makes blocking survive user `try/catch`, `Promise.all`, and `Promise.race`
// exactly as real Temporal workflows do.
//
// Determinism contract (identical to Temporal): a workflow function must be
// pure w.r.t. its input + history. Do not call Date.now(), Math.random(),
// setTimeout, or perform I/O directly — use sleep()/activities/side-effects.

import { AsyncLocalStorage } from "node:async_hooks";
import { TaskError, ActivityFailure, asApplicationFailure } from "../common/failure";
import {
  EventType,
  eventSeq,
  scheduleActivityCommand,
  continueAsNewCommand,
  startChildCommand,
  type HistoryEvent,
  type RawCommand,
} from "../worker/history";
import { decodeFailure } from "../common/failure";
import { toSearchAttributeRecord, TypedSearchAttributes } from "../common/search-attributes";

export interface WorkflowInfo {
  workflowId: string;
  runId: string;
  workflowType: string;
  taskQueue: string;
  namespace: string;
  attempt: number;
}

export interface ActivityOptions {
  /** Maximum time for a single activity attempt (required in practice). */
  startToCloseTimeoutMs?: number;
  /** Accepted for API compatibility; the v1 wire schedules on startToClose. */
  scheduleToCloseTimeoutMs?: number;
  heartbeatTimeoutMs?: number;
  /** Override the workflow's task queue for this activity. */
  taskQueue?: string;
  retry?: {
    initialIntervalMs?: number;
    backoffCoefficient?: number;
    maximumIntervalMs?: number;
    maximumAttempts?: number;
    nonRetryableErrorTypes?: string[];
  };
}

/** Internal activity type that backs durable timers (auto-registered). */
export const TIMER_ACTIVITY_TYPE = "__hanzo_timer__";
const TIMER_SLACK_MS = 60_000;

interface ActivityOutcome {
  failed: boolean;
  result?: unknown;
  failure?: TaskError;
}

/** Sentinel surfaced when a workflow calls the not-yet-supported continueAsNew. */
export class ContinueAsNewNotSupported extends Error {
  constructor(public readonly workflowType: string) {
    super(
      `continueAsNew is not yet supported by the Hanzo Tasks server wire ` +
        `(no ContinueAsNew command). Workflow type: ${workflowType}. See SDK README "Remaining".`,
    );
    this.name = "ContinueAsNewNotSupported";
  }
}

/** DeciderContext holds one episode's replay state. Mirrors workerEnv. */
export class DeciderContext {
  readonly info: WorkflowInfo;
  private readonly activityResults = new Map<number, ActivityOutcome>();
  private readonly scheduledSeqs = new Set<number>();
  private nextSeq = 0;
  readonly newCommands: RawCommand[] = [];

  /** Signal payloads by name, in history order. */
  private readonly signalsByName = new Map<string, unknown[]>();
  /** Registered signal handlers; a matching name drains buffered payloads. */
  private readonly signalHandlers = new Map<string, (arg: unknown) => void>();
  readonly queryHandlers = new Map<string, (...args: unknown[]) => unknown>();
  /** startChild seqs already CHILD_WORKFLOW_EXECUTION_STARTED in history. */
  private readonly childStartedBySeq = new Map<number, { workflowId: string; runId: string }>();

  /** Local, non-durable (mirrors Go: no wire command yet). */
  readonly searchAttributes: Record<string, unknown> = {};
  private readonly versions = new Map<string, number>();

  constructor(info: WorkflowInfo) {
    this.info = info;
  }

  /** Seed from the run's history: scheduled seqs, outcomes, buffered signals. */
  loadHistory(events: HistoryEvent[]): void {
    for (const ev of events) {
      if (ev.eventType === EventType.WorkflowSignaled) {
        // The engine records the signal name under `signalName` (see
        // engine.signalWorkflow); tolerate a legacy `signal` alias.
        const name = String(ev.attributes?.["signalName"] ?? ev.attributes?.["signal"] ?? "");
        if (name) {
          const arr = this.signalsByName.get(name) ?? [];
          arr.push(ev.attributes?.["input"]);
          this.signalsByName.set(name, arr);
        }
        continue;
      }
      const seq = eventSeq(ev);
      if (seq === undefined) continue;
      switch (ev.eventType) {
        case EventType.ActivityScheduled:
          this.scheduledSeqs.add(seq);
          break;
        case EventType.ActivityCompleted:
          this.activityResults.set(seq, { failed: false, result: ev.attributes?.["result"] });
          break;
        case EventType.ActivityFailed: {
          const failureAttr = ev.attributes?.["failure"];
          const failure = decodeFailure(Buffer.from(JSON.stringify(failureAttr ?? null), "utf8"));
          this.activityResults.set(seq, { failed: true, failure });
          break;
        }
        case EventType.ChildWorkflowStarted:
          this.childStartedBySeq.set(seq, {
            workflowId: String(ev.attributes?.["childWorkflowId"] ?? ""),
            runId: String(ev.attributes?.["childRunId"] ?? ""),
          });
          break;
      }
    }
  }

  /** Durable activity call. Resolves from history or emits a schedule + parks. */
  scheduleActivity(activityType: string, args: unknown[], opts: ActivityOptions): Promise<unknown> {
    if (!activityType) {
      return Promise.reject(new TaskError("activity type is empty", "ConfigError", true));
    }
    const seq = this.nextSeq++;
    const outcome = this.activityResults.get(seq);
    if (outcome) {
      if (!outcome.failed) return Promise.resolve(outcome.result);
      // Present the failure the @temporalio way: an ActivityFailure whose
      // `cause` is the ApplicationFailure the activity raised — the shape
      // workflow code pattern-matches (err.cause.type === "refresh_token").
      const cause = asApplicationFailure(outcome.failure ?? new TaskError("activity failed", "ApplicationError", true));
      // The internal timer activity is an implementation detail: surface its
      // failure directly rather than wrapping it as an ActivityFailure.
      if (activityType === TIMER_ACTIVITY_TYPE) return Promise.reject(cause);
      return Promise.reject(new ActivityFailure(activityType, cause));
    }
    if (!this.scheduledSeqs.has(seq)) {
      this.newCommands.push(
        scheduleActivityCommand({
          seq,
          activityType,
          args,
          taskQueue: opts.taskQueue || this.info.taskQueue,
          startToCloseMs: opts.startToCloseTimeoutMs,
          heartbeatMs: opts.heartbeatTimeoutMs,
          retryPolicy: opts.retry
            ? {
                initial_interval_ms: opts.retry.initialIntervalMs,
                backoff_coefficient: opts.retry.backoffCoefficient,
                maximum_interval_ms: opts.retry.maximumIntervalMs,
                maximum_attempts: opts.retry.maximumAttempts,
                non_retryable_error_types: opts.retry.nonRetryableErrorTypes,
              }
            : undefined,
        }),
      );
      this.scheduledSeqs.add(seq);
    }
    return parkForever();
  }

  /** Durable timer, backed by an internal sleeper activity. */
  sleep(ms: number): Promise<void> {
    if (ms <= 0) return Promise.resolve();
    return this.scheduleActivity(TIMER_ACTIVITY_TYPE, [ms], {
      startToCloseTimeoutMs: ms + TIMER_SLACK_MS,
    }) as Promise<void>;
  }

  /** Wait until predicate() is true. Re-evaluated each episode as signals land. */
  condition(predicate: () => boolean, timeoutMs?: number): Promise<boolean> {
    if (predicate()) return Promise.resolve(true);
    if (timeoutMs && timeoutMs > 0) {
      // Race a durable timer: when it fires, the predicate is re-checked and
      // condition resolves false if still unmet.
      const timer = this.sleep(timeoutMs).then(() => predicate());
      return timer;
    }
    return parkForever();
  }

  registerSignalHandler(name: string, fn: (arg: unknown) => void): void {
    this.signalHandlers.set(name, fn);
    const buffered = this.signalsByName.get(name);
    if (buffered) {
      for (const payload of buffered) fn(payload);
      this.signalsByName.delete(name);
    }
  }

  registerQueryHandler(name: string, fn: (...args: unknown[]) => unknown): void {
    this.queryHandlers.set(name, fn);
  }

  getVersion(changeId: string, minSupported: number, maxSupported: number): number {
    const existing = this.versions.get(changeId);
    if (existing !== undefined) return existing;
    // No durable marker command on the v1 wire — record maxSupported locally.
    void minSupported;
    this.versions.set(changeId, maxSupported);
    return maxSupported;
  }

  upsertSearchAttributes(attrs: Record<string, unknown>): void {
    Object.assign(this.searchAttributes, attrs);
  }

  /**
   * continueAsNew — close this run and start a successor of the same type
   * with fresh arguments. Emits a continueAsNew command and parks: the
   * function never settles, the episode ends carrying only that command, and
   * the server closes the run (CONTINUED_AS_NEW) and starts the successor.
   */
  continueAsNew(args: unknown[]): Promise<never> {
    this.newCommands.push(continueAsNewCommand(args, this.info.workflowType, this.info.taskQueue));
    return parkForever();
  }

  /**
   * startChild — start a detached (ABANDON) child workflow. seq-keyed and
   * durable: resolves from history once CHILD_WORKFLOW_EXECUTION_STARTED{seq}
   * lands, otherwise emits a startChild command and parks. Resolves with the
   * child's identity (the parent does not await the child's result).
   */
  startChild(
    workflowType: string,
    args: unknown[],
    opts: { workflowId?: string; taskQueue?: string; searchAttributes?: Record<string, unknown> | TypedSearchAttributes } = {},
  ): Promise<ChildWorkflowHandle> {
    const seq = this.nextSeq++;
    const started = this.childStartedBySeq.get(seq);
    if (started) {
      return Promise.resolve({ workflowId: started.workflowId, runId: started.runId, firstExecutionRunId: started.runId });
    }
    this.newCommands.push(
      startChildCommand({
        seq,
        workflowType,
        workflowId: opts.workflowId,
        args,
        taskQueue: opts.taskQueue ?? this.info.taskQueue,
        searchAttributes: toSearchAttributeRecord(opts.searchAttributes),
      }),
    );
    return parkForever();
  }
}

/** Minimal child-workflow handle returned by startChild (ABANDON). */
export interface ChildWorkflowHandle {
  workflowId: string;
  runId: string;
  firstExecutionRunId: string;
}

/** A promise that never settles — parks the async workflow on suspension. */
export function parkForever(): Promise<never> {
  return new Promise<never>(() => {});
}

const storage = new AsyncLocalStorage<DeciderContext>();

/** The active decider context. Throws if called outside a workflow. */
export function currentContext(): DeciderContext {
  const ctx = storage.getStore();
  if (!ctx) {
    throw new Error("hanzo/tasks: workflow primitive called outside a workflow context");
  }
  return ctx;
}

/** Run `fn` bound to `ctx` for one replay episode. */
export function runWithContext<T>(ctx: DeciderContext, fn: () => T): T {
  return storage.run(ctx, fn);
}
