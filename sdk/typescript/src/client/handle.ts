// Copyright © 2026 Hanzo AI. MIT License.

import { WorkflowStatus, type WorkflowExecutionInfo } from "./options";
import { TaskError, FailureCode } from "../common/failure";
import type { TasksClient } from "./client";

export interface WorkflowResultOptions {
  /** Poll cadence bounds; defaults 250ms → 5s. */
  initialIntervalMs?: number;
  maxIntervalMs?: number;
  /** When true, stop at a ContinuedAsNew terminal instead of following it. */
  followRuns?: boolean;
}

/**
 * WorkflowHandle mirrors @temporalio/client's WorkflowHandle for the RPCs the
 * v1 wire supports. Note: DescribeWorkflow does not yet return a workflow
 * result on the v1 wire, so `result()` blocks until terminal and resolves
 * `undefined` on success (throwing on failure/cancel/terminate).
 */
export class WorkflowHandle<T = unknown> {
  constructor(
    private readonly client: TasksClient,
    public readonly workflowId: string,
    public readonly runId: string = "",
  ) {}

  /** Post a signal to this execution. */
  signal(signalName: string, arg?: unknown): Promise<void> {
    return this.client.signalWorkflow(this.workflowId, this.runId, signalName, arg);
  }

  /** Run a registered query and decode its result. */
  query<R = unknown>(queryType: string, ...args: unknown[]): Promise<R> {
    return this.client.queryWorkflow<R>(this.workflowId, this.runId, queryType, args);
  }

  cancel(): Promise<void> {
    return this.client.cancelWorkflow(this.workflowId, this.runId);
  }

  terminate(reason = ""): Promise<void> {
    return this.client.terminateWorkflow(this.workflowId, this.runId, reason);
  }

  describe(): Promise<WorkflowExecutionInfo> {
    return this.client.describeWorkflow(this.workflowId, this.runId);
  }

  /**
   * Block until the workflow reaches a terminal state. Resolves `undefined`
   * on completion (the v1 wire does not surface the result value yet);
   * throws a TaskError on failure/cancel/terminate/timeout.
   */
  async result(opts: WorkflowResultOptions = {}): Promise<T | undefined> {
    let backoff = opts.initialIntervalMs ?? 250;
    const maxBackoff = opts.maxIntervalMs ?? 5000;
    let runId = this.runId;
    for (;;) {
      const info = await this.client.describeWorkflow(this.workflowId, runId);
      switch (info.status) {
        case WorkflowStatus.Completed:
          return undefined;
        case WorkflowStatus.Failed:
          throw new TaskError(`workflow failed: ${this.workflowId}`, FailureCode.Application, true);
        case WorkflowStatus.Canceled:
          throw new TaskError(`workflow canceled: ${this.workflowId}`, FailureCode.Canceled, true);
        case WorkflowStatus.Terminated:
          throw new TaskError(`workflow terminated: ${this.workflowId}`, FailureCode.Application, true);
        case WorkflowStatus.TimedOut:
          throw new TaskError(`workflow timed out: ${this.workflowId}`, FailureCode.Timeout, true);
        case WorkflowStatus.ContinuedAsNew:
          if (opts.followRuns === false) {
            throw new TaskError(`workflow continued as new: ${this.workflowId}`, FailureCode.Application, true);
          }
          runId = "";
          break;
        default:
          break;
      }
      await new Promise((r) => setTimeout(r, backoff));
      if (backoff < maxBackoff) backoff = Math.min(backoff * 2, maxBackoff);
    }
  }
}
