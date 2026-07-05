// Copyright © 2026 Hanzo AI. MIT License.
//
// @temporalio/* compatibility layer. These shims mirror the surface of
// @temporalio/client and @temporalio/worker that social-orchestrator (and
// nestjs-temporal-core) depend on, so migrating is a dependency swap rather
// than a rewrite. Semantics are the Hanzo Tasks event-sourced model.
//
// Migration map:
//   @temporalio/client   Connection, Client, WorkflowClient, WorkflowHandle
//   @temporalio/worker   NativeConnection, Worker
//   @temporalio/common   ApplicationFailure
//   @temporalio/workflow proxyActivities, defineSignal, setHandler, condition,
//                        sleep, continueAsNew, workflowInfo  (see ./workflow)

import { ZapNode, type Transport } from "./zap/node";
import { TasksClient } from "./client/client";
import { WorkflowHandle } from "./client/handle";
import { toMs, type Duration } from "./common/duration";
import { TypedSearchAttributes, toSearchAttributeRecord } from "./common/search-attributes";
import type { RetryPolicy, StartWorkflowOptions } from "./client/options";

function splitHostPort(addr: string): [string, number] {
  const idx = addr.lastIndexOf(":");
  if (idx < 0) throw new Error(`hanzo/tasks: address must be host:port, got ${addr}`);
  return [addr.slice(0, idx) || "127.0.0.1", Number(addr.slice(idx + 1))];
}

/** A connected ZAP transport shared by clients and workers. */
export class Connection {
  protected constructor(
    readonly transport: Transport,
    readonly address: string,
  ) {}

  static async connect(opts: { address: string; identity?: string }): Promise<Connection> {
    const [host, port] = splitHostPort(opts.address);
    const node = new ZapNode({ host, port, nodeId: opts.identity });
    await node.connect();
    return new Connection(node, opts.address);
  }

  async close(): Promise<void> {
    await this.transport.close();
  }
}

/** Worker-side connection. Same wire as Connection (single ZAP transport). */
export class NativeConnection extends Connection {}

interface CompatRetry {
  initialInterval?: Duration;
  backoffCoefficient?: number;
  maximumInterval?: Duration;
  maximumAttempts?: number;
  nonRetryableErrorTypes?: string[];
}

export interface WorkflowStartOptions {
  taskQueue: string;
  workflowId?: string;
  args?: unknown[];
  workflowExecutionTimeout?: Duration;
  workflowRunTimeout?: Duration;
  workflowTaskTimeout?: Duration;
  retry?: CompatRetry;
  cronSchedule?: string;
  memo?: Record<string, unknown>;
  searchAttributes?: Record<string, unknown>;
  /** Typed search attributes (@temporalio TypedSearchAttributes) — flattened to the wire record. */
  typedSearchAttributes?: TypedSearchAttributes | Record<string, unknown>;
  /** Reuse policy for an existing run under workflowId: USE_EXISTING | TERMINATE_EXISTING | FAIL. */
  workflowIdConflictPolicy?: string;
}

function toRetryPolicy(r?: CompatRetry): RetryPolicy | undefined {
  if (!r) return undefined;
  return {
    initialIntervalMs: r.initialInterval !== undefined ? toMs(r.initialInterval) : undefined,
    backoffCoefficient: r.backoffCoefficient,
    maximumIntervalMs: r.maximumInterval !== undefined ? toMs(r.maximumInterval) : undefined,
    maximumAttempts: r.maximumAttempts,
    nonRetryableErrorTypes: r.nonRetryableErrorTypes,
  };
}

function toStartOptions(o: WorkflowStartOptions): StartWorkflowOptions {
  // typedSearchAttributes wins when both are present; both flatten to a
  // name→value record on the wire.
  const searchAttributes =
    toSearchAttributeRecord(o.typedSearchAttributes) ?? o.searchAttributes;
  return {
    taskQueue: o.taskQueue,
    workflowId: o.workflowId,
    args: o.args,
    workflowExecutionTimeoutMs: o.workflowExecutionTimeout !== undefined ? toMs(o.workflowExecutionTimeout) : undefined,
    workflowRunTimeoutMs: o.workflowRunTimeout !== undefined ? toMs(o.workflowRunTimeout) : undefined,
    workflowTaskTimeoutMs: o.workflowTaskTimeout !== undefined ? toMs(o.workflowTaskTimeout) : undefined,
    retryPolicy: toRetryPolicy(o.retry),
    cronSchedule: o.cronSchedule,
    memo: o.memo,
    searchAttributes,
    workflowIdConflictPolicy: o.workflowIdConflictPolicy,
  };
}

type WorkflowRef = string | { name?: string };

/** Mirrors @temporalio/client's WorkflowClient (Client.workflow). */
export class WorkflowClient {
  constructor(private readonly client: TasksClient) {}

  start(workflow: WorkflowRef, opts: WorkflowStartOptions): Promise<WorkflowHandle> {
    return this.client.startWorkflow(workflow, toStartOptions(opts));
  }

  async execute<T = unknown>(workflow: WorkflowRef, opts: WorkflowStartOptions): Promise<T | undefined> {
    const handle = await this.client.startWorkflow(workflow, toStartOptions(opts));
    return handle.result() as Promise<T | undefined>;
  }

  getHandle<T = unknown>(workflowId: string, runId = ""): WorkflowHandle<T> {
    return this.client.getHandle<T>(workflowId, runId);
  }

  signalWithStart(
    workflow: WorkflowRef,
    opts: WorkflowStartOptions & { signal: string; signalArgs?: unknown[] },
  ): Promise<WorkflowHandle> {
    const signalArg = opts.signalArgs && opts.signalArgs.length > 0 ? opts.signalArgs[0] : undefined;
    return this.client.signalWithStart(workflow, opts.signal, signalArg, toStartOptions(opts));
  }
}

/** Mirrors @temporalio/client's Client. */
export class Client {
  readonly workflow: WorkflowClient;
  private readonly client: TasksClient;

  private constructor(client: TasksClient) {
    this.client = client;
    this.workflow = new WorkflowClient(client);
  }

  static async create(opts: { connection: Connection; namespace?: string; identity?: string }): Promise<Client> {
    const client = await TasksClient.connect({
      address: opts.connection.address,
      namespace: opts.namespace,
      identity: opts.identity,
      transport: opts.connection.transport,
    });
    return new Client(client);
  }

  /** Direct access to the underlying TasksClient. */
  get connection(): TasksClient {
    return this.client;
  }

  async close(): Promise<void> {
    await this.client.close();
  }
}

// Worker (with static create) is re-exported unchanged from ./worker — its
// WorkerOptions already accepts `connection`, `address`, or `transport`, so
// `Worker.create({ connection, namespace, taskQueue, workflows, activities })`
// works exactly like @temporalio/worker.
//
// ApplicationFailure + ActivityFailure are the canonical @temporalio/common
// error types; they live in ./common/failure and are re-exported by index.ts.
