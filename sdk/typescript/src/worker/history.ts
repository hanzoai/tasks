// Copyright © 2026 Hanzo AI. MIT License.
//
// History + command wire model, matching pkg/tasks/types.go (HistoryEvent),
// pkg/tasks/dispatch.go (delivery JSON) and pkg/sdk/worker/dispatch.go
// (commandsEnvelope / rawCommand).
//
// Encoding asymmetry to respect exactly:
//   • History attributes (server → worker): `input`/`result`/`failure` are
//     RAW JSON values (json.RawMessage); `seq` is a number.
//   • Command fields (worker → server): `input`/`result`/`failure` are Go
//     []byte, i.e. BASE64 strings in the commands JSON.

import { encodeFailure } from "../common/failure";

export const EventType = {
  WorkflowStarted: "WORKFLOW_EXECUTION_STARTED",
  ActivityScheduled: "ACTIVITY_TASK_SCHEDULED",
  ActivityCompleted: "ACTIVITY_TASK_COMPLETED",
  ActivityFailed: "ACTIVITY_TASK_FAILED",
  WorkflowSignaled: "WORKFLOW_EXECUTION_SIGNALED",
  WorkflowContinuedAsNew: "WORKFLOW_EXECUTION_CONTINUED_AS_NEW",
  ChildWorkflowStarted: "CHILD_WORKFLOW_EXECUTION_STARTED",
} as const;

export interface HistoryEvent {
  eventId: number;
  eventTime?: string;
  eventType: string;
  attributes?: Record<string, unknown>;
}

export const CommandKind = {
  CompleteWorkflow: 0,
  FailWorkflow: 1,
  ScheduleActivity: 2,
  // 3 = canceled (worker cancel-ack) — not emitted by the TS decider.
  ContinueAsNew: 4,
  StartChildWorkflow: 5,
} as const;

export interface RawCommand {
  kind: number;
  result?: string; // base64
  failure?: string; // base64
  seq?: number;
  activityType?: string;
  workflowType?: string; // kind=4 continueAsNew (successor type) / kind=5 startChild (child type)
  childWorkflowId?: string; // kind=5 startChild
  input?: string; // base64
  taskQueue?: string;
  searchAttrs?: Record<string, unknown>; // kind=5 startChild
  startToCloseMs?: number;
  heartbeatMs?: number;
  retryPolicy?: {
    initial_interval_ms?: number;
    backoff_coefficient?: number;
    maximum_interval_ms?: number;
    maximum_attempts?: number;
    non_retryable_error_types?: string[];
  };
}

export interface CommandsEnvelope {
  v: number;
  cmds: RawCommand[];
}

/** Parse a delivered history string into events. Empty → []. */
export function parseHistory(history: string | undefined): HistoryEvent[] {
  if (!history || history.trim().length === 0) return [];
  const parsed = JSON.parse(history);
  return Array.isArray(parsed) ? (parsed as HistoryEvent[]) : [];
}

/** Numeric seq attribute, or undefined. */
export function eventSeq(ev: HistoryEvent): number | undefined {
  const seq = ev.attributes?.["seq"];
  return typeof seq === "number" ? seq : undefined;
}

/** Workflow args recorded on WORKFLOW_EXECUTION_STARTED. */
export function workflowInput(events: HistoryEvent[]): unknown[] {
  for (const ev of events) {
    if (ev.eventType === EventType.WorkflowStarted) {
      const input = ev.attributes?.["input"];
      if (Array.isArray(input)) return input;
      return input === undefined ? [] : [input];
    }
  }
  return [];
}

export function completeWorkflowCommand(result: unknown): RawCommand {
  const cmd: RawCommand = { kind: CommandKind.CompleteWorkflow };
  if (result !== undefined) {
    cmd.result = Buffer.from(JSON.stringify(result), "utf8").toString("base64");
  }
  return cmd;
}

export function failWorkflowCommand(err: unknown): RawCommand {
  return {
    kind: CommandKind.FailWorkflow,
    failure: encodeFailure(err).toString("base64"),
  };
}

export interface ScheduleActivitySpec {
  seq: number;
  activityType: string;
  args: unknown[];
  taskQueue: string;
  startToCloseMs?: number;
  heartbeatMs?: number;
  retryPolicy?: RawCommand["retryPolicy"];
}

export function scheduleActivityCommand(spec: ScheduleActivitySpec): RawCommand {
  return {
    kind: CommandKind.ScheduleActivity,
    seq: spec.seq,
    activityType: spec.activityType,
    input: Buffer.from(JSON.stringify(spec.args ?? []), "utf8").toString("base64"),
    taskQueue: spec.taskQueue,
    startToCloseMs: spec.startToCloseMs || undefined,
    heartbeatMs: spec.heartbeatMs || undefined,
    retryPolicy: spec.retryPolicy,
  };
}

/** continueAsNew command (kind=4): carries the successor run's arguments. */
export function continueAsNewCommand(args: unknown[], workflowType?: string, taskQueue?: string): RawCommand {
  return {
    kind: CommandKind.ContinueAsNew,
    input: Buffer.from(JSON.stringify(args ?? []), "utf8").toString("base64"),
    workflowType: workflowType || undefined,
    taskQueue: taskQueue || undefined,
  };
}

export interface StartChildSpec {
  seq: number;
  workflowType: string;
  workflowId?: string;
  args: unknown[];
  taskQueue?: string;
  searchAttributes?: Record<string, unknown>;
}

/** startChild command (kind=5): a detached (ABANDON) child, seq-keyed. */
export function startChildCommand(spec: StartChildSpec): RawCommand {
  return {
    kind: CommandKind.StartChildWorkflow,
    seq: spec.seq,
    workflowType: spec.workflowType,
    childWorkflowId: spec.workflowId || undefined,
    input: Buffer.from(JSON.stringify(spec.args ?? []), "utf8").toString("base64"),
    taskQueue: spec.taskQueue || undefined,
    searchAttrs: spec.searchAttributes,
  };
}

export function encodeCommands(cmds: RawCommand[]): Buffer {
  const env: CommandsEnvelope = { v: 1, cmds };
  return Buffer.from(JSON.stringify(env), "utf8");
}

export const emptyCommands = (): Buffer => encodeCommands([]);
