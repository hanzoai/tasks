// Copyright © 2026 Hanzo AI. MIT License.
//
// Worker runtime. Subscribes once per kind, receives server-pushed workflow
// and activity tasks over ZAP, dispatches each to a registered function, and
// ships the result back. Faithful port of pkg/sdk/worker (Go).

import { ZapNode, type Transport, type TokenProvider } from "../zap/node";
import { Opcode } from "../zap/opcodes";
import {
  decodeEnvelope,
  decodeHeartbeatResponse,
  encodeFieldObject,
} from "../zap/envelope";
import { encodeCommands, failWorkflowCommand } from "./history";
import { runWorkflowEpisode, type WorkflowFn } from "./decider";
import {
  ActivityContext,
  runInActivityContext,
  type ActivityInfo,
} from "./activity-context";
import { encodeFailure, TaskError } from "../common/failure";
import { encodeValue, decodeArgs } from "../common/converter";
import { TIMER_ACTIVITY_TYPE, type WorkflowInfo } from "../workflow/runtime";

export type ActivityFn = (...args: any[]) => unknown | Promise<unknown>;

export interface WorkerOptions {
  /** A shared connection (from Connection.connect). Preferred. */
  connection?: { transport: Transport };
  /** "host:port" of tasksd. Ignored when `connection`/`transport` is set. */
  address?: string;
  namespace?: string;
  /** Required — the queue this worker serves. */
  taskQueue: string;
  identity?: string;
  workflows?: Record<string, WorkflowFn>;
  activities?: Record<string, ActivityFn>;
  /** Inject a Transport directly (tests). */
  transport?: Transport;
  /** IAM bearer source for an identity-gated frontend (cloud ServeGated :9999). */
  token?: TokenProvider;
  dialTimeoutMs?: number;
  callTimeoutMs?: number;
}

interface WorkflowDelivery {
  task_token: string;
  workflow_id: string;
  run_id: string;
  workflow_type_name: string;
  history?: string;
}

interface ActivityDelivery {
  task_token: string;
  workflow_id: string;
  run_id: string;
  activity_id: string;
  activity_type_name: string;
  input?: string;
  scheduled_time_ms: number;
  start_to_close_timeout_ms?: number;
  heartbeat_timeout_ms?: number;
}

const delay = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

export class Worker {
  private readonly workflows = new Map<string, WorkflowFn>();
  private readonly activities = new Map<string, ActivityFn>();
  private readonly inflight = new Set<Promise<void>>();
  private wfSubId = "";
  private actSubId = "";
  private started = false;
  private stopped = false;
  private resolveDone: (() => void) | null = null;
  private donePromise: Promise<void> | null = null;

  private constructor(
    private readonly transport: Transport,
    private readonly namespace: string,
    private readonly taskQueue: string,
    private readonly identity: string,
    private readonly ownsTransport: boolean,
  ) {
    // Durable timers are backed by an internal sleeper activity.
    this.activities.set(TIMER_ACTIVITY_TYPE, async (ms: number) => {
      await delay(Math.max(0, Number(ms) || 0));
    });
  }

  static async create(opts: WorkerOptions): Promise<Worker> {
    if (!opts.taskQueue) throw new Error("hanzo/tasks: WorkerOptions.taskQueue is required");
    const namespace = opts.namespace && opts.namespace.length > 0 ? opts.namespace : "default";
    const identity = opts.identity && opts.identity.length > 0 ? opts.identity : "hanzo-tasks-ts-worker";

    let transport: Transport;
    let owns = false;
    if (opts.transport) {
      transport = opts.transport;
    } else if (opts.connection) {
      transport = opts.connection.transport;
    } else {
      if (!opts.address) throw new Error("hanzo/tasks: WorkerOptions requires connection, address, or transport");
      const [host, port] = splitHostPort(opts.address);
      const node = new ZapNode({
        host,
        port,
        nodeId: identity,
        dialTimeoutMs: opts.dialTimeoutMs,
        callTimeoutMs: opts.callTimeoutMs,
        token: opts.token,
      });
      await node.connect();
      transport = node;
      owns = true;
    }

    const w = new Worker(transport, namespace, opts.taskQueue, identity, owns);
    for (const [name, fn] of Object.entries(opts.workflows ?? {})) w.registerWorkflow(name, fn);
    for (const [name, fn] of Object.entries(opts.activities ?? {})) w.registerActivity(name, fn);
    return w;
  }

  registerWorkflow(name: string, fn: WorkflowFn): void {
    if (this.workflows.has(name)) throw new Error(`hanzo/tasks: workflow ${name} already registered`);
    this.workflows.set(name, fn);
  }

  registerActivity(name: string, fn: ActivityFn): void {
    if (this.activities.has(name) && name !== TIMER_ACTIVITY_TYPE) {
      throw new Error(`hanzo/tasks: activity ${name} already registered`);
    }
    this.activities.set(name, fn);
  }

  /** Install handlers and subscribe. Non-blocking. */
  async start(): Promise<void> {
    if (this.started) return;
    this.started = true;

    this.transport.handle(Opcode.DeliverWorkflowTask, (_from, body) => {
      this.track(this.onWorkflowTask(body));
    });
    this.transport.handle(Opcode.DeliverActivityTask, (_from, body) => {
      this.track(this.onActivityTask(body));
    });

    this.actSubId = await this.subscribeActivity();
    this.wfSubId = await this.subscribeWorkflow();
  }

  /** Start, then block until shutdown() is called. Mirrors @temporalio run(). */
  async run(): Promise<void> {
    await this.start();
    this.donePromise = new Promise<void>((resolve) => {
      this.resolveDone = resolve;
    });
    await this.donePromise;
  }

  /** Unsubscribe, drain in-flight, and close the connection if owned. */
  async shutdown(): Promise<void> {
    if (this.stopped) return;
    this.stopped = true;
    try {
      if (this.wfSubId) await this.unsubscribe(this.wfSubId);
      if (this.actSubId) await this.unsubscribe(this.actSubId);
    } catch {
      // Best effort — the server treats unknown sub-ids as already gone.
    }
    await Promise.allSettled([...this.inflight]);
    if (this.ownsTransport) await this.transport.close();
    if (this.resolveDone) this.resolveDone();
  }

  private track(p: Promise<void>): void {
    this.inflight.add(p);
    p.finally(() => this.inflight.delete(p)).catch(() => {});
  }

  // ── workflow task ──

  private async onWorkflowTask(body: Buffer): Promise<void> {
    let msg: WorkflowDelivery;
    try {
      msg = JSON.parse(body.toString("utf8"));
    } catch {
      return;
    }
    if (!msg.task_token) return;
    const token = Buffer.from(msg.task_token, "utf8");

    const fn = this.workflows.get(msg.workflow_type_name);
    if (!fn) {
      await this.respondWorkflow(token, encodeCommands([]));
      return;
    }
    const info: WorkflowInfo = {
      workflowId: msg.workflow_id,
      runId: msg.run_id,
      workflowType: msg.workflow_type_name,
      taskQueue: this.taskQueue,
      namespace: this.namespace,
      attempt: 1,
    };
    try {
      const commands = await runWorkflowEpisode(fn, info, msg.history ?? "");
      await this.respondWorkflow(token, encodeCommands(commands));
    } catch (err) {
      // A decider bug must not wedge the run — report a workflow failure.
      await this.respondWorkflow(token, encodeCommands([failWorkflowCommand(err)]));
    }
  }

  // ── activity task ──

  private async onActivityTask(body: Buffer): Promise<void> {
    let msg: ActivityDelivery;
    try {
      msg = JSON.parse(body.toString("utf8"));
    } catch {
      return;
    }
    if (!msg.task_token) return;
    const token = Buffer.from(msg.task_token, "utf8");

    const fn = this.activities.get(msg.activity_type_name);
    if (!fn) {
      await this.respondActivityFailed(
        token,
        encodeFailure(new TaskError(`activity ${msg.activity_type_name} not registered`, "NotFoundError", true)),
      );
      return;
    }

    const abort = new AbortController();
    const info: ActivityInfo = {
      taskToken: token,
      workflowId: msg.workflow_id,
      runId: msg.run_id,
      activityId: msg.activity_id,
      activityType: msg.activity_type_name,
      taskQueue: this.taskQueue,
      attempt: 1,
      scheduledTimeMs: msg.scheduled_time_ms ?? 0,
      startedTimeMs: Date.now(),
    };
    const ctx = new ActivityContext(
      info,
      (details) => {
        void this.heartbeat(token, details).then((cancelRequested) => {
          if (cancelRequested) abort.abort();
        });
      },
      abort,
    );

    let stopHeartbeat: (() => void) | null = null;
    if (msg.heartbeat_timeout_ms && msg.heartbeat_timeout_ms > 0) {
      stopHeartbeat = this.autoHeartbeat(token, Math.max(100, msg.heartbeat_timeout_ms / 2), abort);
    }

    try {
      const args = decodeArgs(msg.input ? Buffer.from(msg.input, "utf8") : null);
      const result = await runInActivityContext(ctx, () => Promise.resolve(fn(...args)));
      if (stopHeartbeat) stopHeartbeat();
      await this.respondActivityCompleted(token, encodeValue(result));
    } catch (err) {
      if (stopHeartbeat) stopHeartbeat();
      await this.respondActivityFailed(token, encodeFailure(err));
    }
  }

  private autoHeartbeat(token: Buffer, intervalMs: number, abort: AbortController): () => void {
    const timer = setInterval(() => {
      void this.heartbeat(token, []).then((cancelRequested) => {
        if (cancelRequested) abort.abort();
      });
    }, intervalMs);
    return () => clearInterval(timer);
  }

  // ── worker transport RPCs ──

  private async subscribeWorkflow(): Promise<string> {
    const body = Buffer.from(
      JSON.stringify({ namespace: this.namespace, task_queue: this.taskQueue, identity: this.identity }),
      "utf8",
    );
    const frame = await this.transport.call(Opcode.SubscribeWorkflowTasks, body);
    return this.decodeSubId(frame);
  }

  private async subscribeActivity(): Promise<string> {
    const body = Buffer.from(
      JSON.stringify({ namespace: this.namespace, task_queue: this.taskQueue, identity: this.identity }),
      "utf8",
    );
    const frame = await this.transport.call(Opcode.SubscribeActivityTasks, body);
    return this.decodeSubId(frame);
  }

  private async unsubscribe(subId: string): Promise<void> {
    const body = Buffer.from(JSON.stringify({ subscription_id: subId }), "utf8");
    await this.transport.call(Opcode.UnsubscribeTasks, body);
  }

  private decodeSubId(frame: Buffer): string {
    const { status, detail, body } = decodeEnvelope(frame);
    if (status !== 0 && status !== 200) throw new Error(`subscribe failed: status ${status}: ${detail}`);
    if (body.length === 0) return "";
    const resp = JSON.parse(body.toString("utf8")) as { subscription_id?: string };
    return resp.subscription_id ?? "";
  }

  private async respondWorkflow(token: Buffer, commands: Buffer): Promise<void> {
    await this.transport.call(Opcode.RespondWorkflowTaskCompleted, encodeFieldObject(token, commands));
  }

  private async respondActivityCompleted(token: Buffer, result: Buffer): Promise<void> {
    await this.transport.call(Opcode.RespondActivityTaskCompleted, encodeFieldObject(token, result));
  }

  private async respondActivityFailed(token: Buffer, failure: Buffer): Promise<void> {
    await this.transport.call(Opcode.RespondActivityTaskFailed, encodeFieldObject(token, failure));
  }

  private async heartbeat(token: Buffer, details: unknown[]): Promise<boolean> {
    try {
      const payload = details.length > 0 ? Buffer.from(JSON.stringify(details), "utf8") : Buffer.alloc(0);
      const frame = await this.transport.call(Opcode.RecordActivityTaskHeartbeat, encodeFieldObject(token, payload));
      return decodeHeartbeatResponse(frame);
    } catch {
      return false;
    }
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
