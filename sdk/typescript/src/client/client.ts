// Copyright © 2026 Hanzo AI. MIT License.
//
// TasksClient — the workflow client. JSON-over-ZAP round trips against the
// Hanzo Tasks frontend, wire-identical to pkg/sdk/client (Go). Every request
// body matches the corresponding *Request struct's JSON tags in
// pkg/sdk/client/{workflow,namespace}.go.

import { ZapNode, type Transport } from "../zap/node";
import { Opcode } from "../zap/opcodes";
import { decodeEnvelope } from "../zap/envelope";
import {
  WorkflowStatus,
  type ConnectionOptions,
  type Namespace,
  type RegisterNamespaceOptions,
  type RetryPolicy,
  type StartWorkflowOptions,
  type WorkflowExecutionInfo,
} from "./options";
import { WorkflowHandle } from "./handle";

type WorkflowTypeArg = string | { name?: string };

function resolveType(workflow: WorkflowTypeArg): string {
  if (typeof workflow === "string") {
    if (!workflow) throw new Error("hanzo/tasks: workflow type name is empty");
    return workflow;
  }
  const name = workflow?.name;
  if (!name) throw new Error("hanzo/tasks: cannot resolve workflow type from an anonymous function");
  return name;
}

function retryToWire(rp?: RetryPolicy): Record<string, unknown> | undefined {
  if (!rp) return undefined;
  return {
    initial_interval_ms: rp.initialIntervalMs,
    backoff_coefficient: rp.backoffCoefficient,
    maximum_interval_ms: rp.maximumIntervalMs,
    maximum_attempts: rp.maximumAttempts,
    non_retryable_error_types: rp.nonRetryableErrorTypes,
  };
}

function timeoutsToWire(o: StartWorkflowOptions): Record<string, unknown> {
  return {
    workflow_execution_ms: o.workflowExecutionTimeoutMs,
    workflow_run_ms: o.workflowRunTimeoutMs,
    workflow_task_ms: o.workflowTaskTimeoutMs,
  };
}

interface RawWorkflowInfo {
  workflow_id: string;
  run_id: string;
  workflow_type: string;
  start_time?: string;
  close_time?: string;
  status: number;
  history_length: number;
  task_queue?: string;
  memo?: Record<string, unknown>;
}

function toInfo(r: RawWorkflowInfo): WorkflowExecutionInfo {
  return {
    workflowId: r.workflow_id,
    runId: r.run_id,
    workflowType: r.workflow_type,
    startTime: r.start_time,
    closeTime: r.close_time,
    status: (r.status ?? 0) as WorkflowStatus,
    historyLength: r.history_length ?? 0,
    taskQueue: r.task_queue,
    memo: r.memo,
  };
}

export interface HealthStatus {
  service: string;
  status: string;
}

export interface ListWorkflowsResult {
  executions: WorkflowExecutionInfo[];
  nextPageToken?: string;
}

export class TasksClient {
  private constructor(
    private readonly transport: Transport,
    public readonly namespace: string,
    public readonly identity: string,
    private readonly owned: boolean,
  ) {}

  /** Connect to a Hanzo Tasks frontend (or wrap an injected Transport). */
  static async connect(opts: ConnectionOptions): Promise<TasksClient> {
    const namespace = opts.namespace && opts.namespace.length > 0 ? opts.namespace : "default";
    const identity = opts.identity && opts.identity.length > 0 ? opts.identity : "hanzo-tasks-ts-sdk";
    if (opts.transport) {
      return new TasksClient(opts.transport, namespace, identity, false);
    }
    const [host, portStr] = splitHostPort(opts.address);
    const node = new ZapNode({
      host,
      port: portStr,
      nodeId: identity,
      dialTimeoutMs: opts.dialTimeoutMs,
      callTimeoutMs: opts.callTimeoutMs,
      token: opts.token,
    });
    await node.connect();
    return new TasksClient(node, namespace, identity, true);
  }

  /** The underlying transport (shared connections; used by the worker). */
  get rawTransport(): Transport {
    return this.transport;
  }

  private async roundTrip<R = unknown>(opcode: number, req: unknown): Promise<R | undefined> {
    const body = Buffer.from(JSON.stringify(req ?? {}), "utf8");
    const respFrame = await this.transport.call(opcode, body);
    const { status, detail, body: payload } = decodeEnvelope(respFrame);
    if (status !== 0 && status !== 200) {
      throw new Error(`hanzo/tasks: server status ${status}: ${detail}`);
    }
    if (payload.length === 0) return undefined;
    return JSON.parse(payload.toString("utf8")) as R;
  }

  // ── workflow lifecycle ──

  async startWorkflow(workflow: WorkflowTypeArg, opts: StartWorkflowOptions): Promise<WorkflowHandle> {
    if (!opts.taskQueue) throw new Error("hanzo/tasks: StartWorkflowOptions.taskQueue is required");
    const req = {
      namespace: this.namespace,
      workflow_id: opts.workflowId ?? "",
      workflow_type: resolveType(workflow),
      task_queue: opts.taskQueue,
      input: opts.args ?? [],
      retry_policy: retryToWire(opts.retryPolicy),
      timeouts: timeoutsToWire(opts),
      memo: opts.memo,
      search_attributes: opts.searchAttributes,
      workflow_id_conflict_policy: opts.workflowIdConflictPolicy || undefined,
      cron_schedule: opts.cronSchedule,
      identity: this.identity,
    };
    const resp = await this.roundTrip<{ run_id: string }>(Opcode.StartWorkflow, req);
    return new WorkflowHandle(this, opts.workflowId ?? "", resp?.run_id ?? "");
  }

  async signalWithStart(
    workflow: WorkflowTypeArg,
    signalName: string,
    signalArg: unknown,
    opts: StartWorkflowOptions,
  ): Promise<WorkflowHandle> {
    if (!opts.workflowId) throw new Error("hanzo/tasks: workflowId is required for signalWithStart");
    if (!signalName) throw new Error("hanzo/tasks: signalName is required");
    if (!opts.taskQueue) throw new Error("hanzo/tasks: taskQueue is required");
    const req = {
      namespace: this.namespace,
      workflow_id: opts.workflowId,
      workflow_type: resolveType(workflow),
      task_queue: opts.taskQueue,
      input: opts.args ?? [],
      retry_policy: retryToWire(opts.retryPolicy),
      timeouts: timeoutsToWire(opts),
      memo: opts.memo,
      search_attributes: opts.searchAttributes,
      cron_schedule: opts.cronSchedule,
      identity: this.identity,
      signal_name: signalName,
      signal_input: signalArg,
    };
    const resp = await this.roundTrip<{ run_id: string }>(Opcode.SignalWithStartWorkflow, req);
    return new WorkflowHandle(this, opts.workflowId, resp?.run_id ?? "");
  }

  async signalWorkflow(workflowId: string, runId: string, signalName: string, arg?: unknown): Promise<void> {
    if (!workflowId) throw new Error("hanzo/tasks: workflowId is required");
    if (!signalName) throw new Error("hanzo/tasks: signalName is required");
    await this.roundTrip(Opcode.SignalWorkflow, {
      namespace: this.namespace,
      workflow_id: workflowId,
      run_id: runId || undefined,
      signal_name: signalName,
      input: arg,
    });
  }

  async queryWorkflow<R = unknown>(
    workflowId: string,
    runId: string,
    queryType: string,
    args: unknown[] = [],
  ): Promise<R> {
    if (!workflowId) throw new Error("hanzo/tasks: workflowId is required");
    if (!queryType) throw new Error("hanzo/tasks: queryType is required");
    const resp = await this.roundTrip<{ result?: string }>(Opcode.QueryWorkflow, {
      namespace: this.namespace,
      workflow_id: workflowId,
      run_id: runId || undefined,
      query_type: queryType,
      query_args: args,
    });
    // result is a Go []byte → base64 on the wire → raw JSON of the value.
    if (!resp?.result) return undefined as R;
    const raw = Buffer.from(resp.result, "base64").toString("utf8").trim();
    return (raw.length === 0 ? undefined : JSON.parse(raw)) as R;
  }

  async cancelWorkflow(workflowId: string, runId = ""): Promise<void> {
    await this.roundTrip(Opcode.CancelWorkflow, {
      namespace: this.namespace,
      workflow_id: workflowId,
      run_id: runId || undefined,
    });
  }

  async terminateWorkflow(workflowId: string, runId = "", reason = ""): Promise<void> {
    await this.roundTrip(Opcode.TerminateWorkflow, {
      namespace: this.namespace,
      workflow_id: workflowId,
      run_id: runId || undefined,
      reason: reason || undefined,
    });
  }

  async describeWorkflow(workflowId: string, runId = ""): Promise<WorkflowExecutionInfo> {
    const resp = await this.roundTrip<{ info: RawWorkflowInfo }>(Opcode.DescribeWorkflow, {
      namespace: this.namespace,
      workflow_id: workflowId,
      run_id: runId || undefined,
    });
    if (!resp?.info) throw new Error(`hanzo/tasks: describe returned no info for ${workflowId}`);
    return toInfo(resp.info);
  }

  async listWorkflows(query = "", pageSize = 0, nextPageToken?: string): Promise<ListWorkflowsResult> {
    const resp = await this.roundTrip<{ executions?: RawWorkflowInfo[]; next_page_token?: string }>(
      Opcode.ListWorkflows,
      {
        namespace: this.namespace,
        query: query || undefined,
        page_size: pageSize || undefined,
        next_page_token: nextPageToken,
      },
    );
    return {
      executions: (resp?.executions ?? []).map(toInfo),
      nextPageToken: resp?.next_page_token,
    };
  }

  getHandle<T = unknown>(workflowId: string, runId = ""): WorkflowHandle<T> {
    return new WorkflowHandle<T>(this, workflowId, runId);
  }

  // ── namespace ops ──

  async registerNamespace(opts: RegisterNamespaceOptions): Promise<void> {
    if (!opts.name) throw new Error("hanzo/tasks: namespace name is required");
    await this.roundTrip(Opcode.RegisterNamespace, {
      name: opts.name,
      description: opts.description,
      owner_email: opts.ownerEmail,
      retention_ms: opts.retentionMs,
    });
  }

  async describeNamespace(name: string): Promise<Namespace> {
    const resp = await this.roundTrip<{ namespace: Namespace }>(Opcode.DescribeNamespace, { name });
    if (!resp?.namespace) throw new Error(`hanzo/tasks: namespace ${name} not found`);
    return resp.namespace;
  }

  async listNamespaces(pageSize = 0, nextPageToken?: string): Promise<{ namespaces: Namespace[]; nextPageToken?: string }> {
    const resp = await this.roundTrip<{ namespaces?: Namespace[]; next_page_token?: string }>(Opcode.ListNamespaces, {
      page_size: pageSize || undefined,
      next_page_token: nextPageToken,
    });
    return { namespaces: resp?.namespaces ?? [], nextPageToken: resp?.next_page_token };
  }

  // ── health ──

  async health(): Promise<HealthStatus> {
    const resp = await this.roundTrip<HealthStatus>(Opcode.Health, {});
    return { service: resp?.service ?? "", status: resp?.status ?? "" };
  }

  /** Close the underlying connection (only if this client owns it). */
  async close(): Promise<void> {
    if (this.owned) await this.transport.close();
  }
}

function splitHostPort(addr: string): [string, number] {
  const idx = addr.lastIndexOf(":");
  if (idx < 0) throw new Error(`hanzo/tasks: address must be host:port, got ${addr}`);
  const host = addr.slice(0, idx) || "127.0.0.1";
  const port = Number(addr.slice(idx + 1));
  if (!Number.isFinite(port) || port <= 0) throw new Error(`hanzo/tasks: invalid port in ${addr}`);
  return [host, port];
}
