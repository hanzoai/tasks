// Copyright © 2026 Hanzo AI. MIT License.
//
// @hanzoai/tasks — TypeScript SDK for Hanzo Tasks durable execution over the
// native ZAP transport. Drop-in for @temporalio/* + nestjs-temporal-core.
//
// Public surface:
//   • Native client:   TasksClient, WorkflowHandle
//   • Worker:          Worker (activities + workflow decider)
//   • Workflow API:    proxyActivities, sleep, condition, defineSignal,
//                      setHandler, workflowInfo, continueAsNew, ...
//   • @temporalio shim: Connection, NativeConnection, Client, ApplicationFailure
//   • Errors:          TaskError, FailureCode
//   • Low-level ZAP:   exported under the `zap` namespace

export * from "./common/failure";
export * from "./common/converter";
export * from "./common/duration";
export * from "./common/search-attributes";

export * from "./client";
export * from "./worker";
export * from "./workflow";

export * from "./compat";

// Low-level ZAP wire (advanced / embedding / testing).
export * as zap from "./zap";
export type { Transport, TokenProvider } from "./zap/node";
