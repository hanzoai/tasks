// Copyright © 2026 Hanzo AI. MIT License.
//
// ZAP opcodes owned by the Hanzo Tasks wire. Authoritative source:
// pkg/sdk/client/{client,transport}.go and pkg/tasks/dispatch.go. These
// values are stable across releases and never reused — mirror the Go
// constants exactly.

export const Opcode = {
  // ── client workflow lifecycle (0x0060–0x006F) ──
  StartWorkflow: 0x0060,
  SignalWorkflow: 0x0061,
  CancelWorkflow: 0x0062,
  TerminateWorkflow: 0x0063,
  DescribeWorkflow: 0x0064,
  ListWorkflows: 0x0065,
  SignalWithStartWorkflow: 0x0066,
  QueryWorkflow: 0x0067,

  // in-workflow scheduling
  ScheduleActivity: 0x006b,
  StartChildWorkflow: 0x006d,

  // ── client schedule ops (0x0070–0x007F) ──
  CreateSchedule: 0x0070,
  ListSchedules: 0x0071,
  DeleteSchedule: 0x0072,
  PauseSchedule: 0x0073,
  UpdateSchedule: 0x0074,
  TriggerSchedule: 0x0075,
  DescribeSchedule: 0x0076,

  // ── client namespace ops (0x0080–0x008F) ──
  RegisterNamespace: 0x0080,
  DescribeNamespace: 0x0081,
  ListNamespaces: 0x0082,

  // ── health + meta (0x0090–0x009F) ──
  Health: 0x0090,

  // ── worker subscribe / respond (0x00A0–0x00AF) ──
  SubscribeWorkflowTasks: 0x00a0,
  SubscribeActivityTasks: 0x00a1,
  RespondWorkflowTaskCompleted: 0x00a2,
  RespondActivityTaskCompleted: 0x00a3,
  RespondActivityTaskFailed: 0x00a4,
  RecordActivityTaskHeartbeat: 0x00a5,
  UnsubscribeTasks: 0x00a6,

  // ── server → worker deliveries (Send; 0x00B0–0x00BF) ──
  DeliverWorkflowTask: 0x00b0,
  DeliverActivityTask: 0x00b1,
  DeliverCancelRequest: 0x00b3,
  DeliverQuery: 0x00b4,

  // worker → server query response
  RespondQuery: 0x00c4,

  // generic error
  Error: 0x00ff,
} as const;

export type OpcodeValue = (typeof Opcode)[keyof typeof Opcode];

// Field offsets for the field-object framing used by worker respond /
// heartbeat RPCs (pkg/sdk/client/transport.go). token at 0; the single
// payload (commands / result / failure / details) at 8; cancel flag at 8.
export const Field = {
  TaskToken: 0,
  Payload: 8,
  RespStatus: 0,
  RespCancelRequested: 8,
} as const;
