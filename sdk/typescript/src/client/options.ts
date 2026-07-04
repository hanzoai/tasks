// Copyright © 2026 Hanzo AI. MIT License.

import type { Transport } from "../zap/node";

/** ConnectionOptions configures a client/worker connection to tasksd. */
export interface ConnectionOptions {
  /** "host:port" of the Hanzo Tasks ZAP frontend (default port 9999). */
  address: string;
  /** Namespace scoping every RPC. Empty is rewritten to "default". */
  namespace?: string;
  /** Caller identity sent on worker RPCs. Defaults to "hanzo-tasks-ts-sdk". */
  identity?: string;
  dialTimeoutMs?: number;
  callTimeoutMs?: number;
  /** Inject a Transport (tests / embedding). When set, `address` is ignored. */
  transport?: Transport;
}

export interface RetryPolicy {
  initialIntervalMs?: number;
  backoffCoefficient?: number;
  maximumIntervalMs?: number;
  maximumAttempts?: number;
  nonRetryableErrorTypes?: string[];
}

export interface StartWorkflowOptions {
  /** Workflow ID. Empty asks the server to generate one. */
  workflowId?: string;
  /** Required — workers poll this queue. */
  taskQueue: string;
  /** Positional workflow arguments. */
  args?: unknown[];
  workflowExecutionTimeoutMs?: number;
  workflowRunTimeoutMs?: number;
  workflowTaskTimeoutMs?: number;
  retryPolicy?: RetryPolicy;
  /** Cron expression; empty runs once. */
  cronSchedule?: string;
  /** Opaque metadata attached to the execution. */
  memo?: Record<string, unknown>;
  /**
   * Search attributes. Accepted for @temporalio API compatibility, but the
   * v1 Hanzo Tasks wire (StartWorkflowRequest) does not yet carry a search-
   * attribute field — see the SDK README "Remaining" section. Values are
   * currently ignored on the wire.
   */
  searchAttributes?: Record<string, unknown>;
}

export enum WorkflowStatus {
  Unspecified = 0,
  Running = 1,
  Completed = 2,
  Failed = 3,
  Canceled = 4,
  Terminated = 5,
  ContinuedAsNew = 6,
  TimedOut = 7,
}

export interface WorkflowExecutionInfo {
  workflowId: string;
  runId: string;
  workflowType: string;
  startTime?: string;
  closeTime?: string;
  status: WorkflowStatus;
  historyLength: number;
  taskQueue?: string;
  memo?: Record<string, unknown>;
}

export enum NamespaceState {
  Unspecified = 0,
  Registered = 1,
  Deprecated = 2,
  Deleted = 3,
}

export interface Namespace {
  info: {
    name: string;
    state: NamespaceState;
    description?: string;
    owner_email?: string;
    id?: string;
  };
  config: { retention_ms?: number };
}

export interface RegisterNamespaceOptions {
  name: string;
  description?: string;
  ownerEmail?: string;
  retentionMs?: number;
}
