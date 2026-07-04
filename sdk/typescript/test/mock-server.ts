// Copyright © 2026 Hanzo AI. MIT License.
//
// MockTasksServer — a faithful, in-memory reimplementation of the tasksd ZAP
// frontend + durable engine, sufficient to exercise the SDK end to end. It
// speaks the exact wire luxfi/zap does: length-framed messages, a node-id
// handshake, reqID/flag correlation for Calls, and bare `Send` frames for
// server-pushed task deliveries. The engine mirrors pkg/tasks: per-run
// event-sourced history, seq-keyed activity scheduling, and re-delivery of
// workflow tasks as progress lands.

import net from "node:net";
import { randomBytes } from "node:crypto";
import {
  Builder,
  ZapMessage,
  encodeResponseEnvelope,
  envelopeBody,
  decodeFieldObject,
  encodeEnvelope,
  encodeHeartbeatResponse,
  Opcode,
} from "../src/zap";

const REQ_FLAG_REQ = 1;
const REQ_FLAG_RESP = 2;

interface Run {
  workflowId: string;
  runId: string;
  workflowType: string;
  taskQueue: string;
  history: Array<{ eventId: number; eventType: string; attributes?: Record<string, unknown> }>;
  status: number; // 1 running, 2 completed, 3 failed, 4 canceled, 5 terminated
  scheduledSeqs: Set<number>;
  memo?: Record<string, unknown>;
}

interface Subscriber {
  socket: net.Socket;
  taskQueue: string;
}

const WorkflowStatus = { Running: 1, Completed: 2, Failed: 3, Canceled: 4, Terminated: 5 };

function encodeHandshake(nodeId: string): Buffer {
  const b = new Builder(128);
  const obj = b.startObject(64);
  const idBytes = Buffer.from(nodeId, "utf8");
  for (let i = 0; i < Math.min(idBytes.length, 60); i++) obj.setUint8(i, idBytes[i]);
  obj.setUint32(60, idBytes.length >>> 0);
  obj.finishAsRoot();
  return b.finish();
}

export interface MockOptions {
  /** Optional canned query result value returned by QueryWorkflow. */
  queryResult?: unknown;
}

export class MockTasksServer {
  private readonly server: net.Server;
  private readonly runs = new Map<string, Run>();
  private readonly byWorkflowId = new Map<string, string>(); // workflowId → latest runId
  private readonly wfTokens = new Map<string, string>(); // token → runId
  private readonly actTokens = new Map<string, { runId: string; seq: number; activityType: string }>();
  private readonly wfSubs: Subscriber[] = [];
  private readonly actSubs: Subscriber[] = [];
  private readonly sockets = new Set<net.Socket>();
  /** Records every RPC opcode the server received (for assertions). */
  readonly seenOpcodes: number[] = [];

  constructor(private readonly opts: MockOptions = {}) {
    this.server = net.createServer((sock) => this.onConnection(sock));
  }

  listen(): Promise<number> {
    return new Promise((resolve) => {
      this.server.listen(0, "127.0.0.1", () => {
        const addr = this.server.address() as net.AddressInfo;
        resolve(addr.port);
      });
    });
  }

  async close(): Promise<void> {
    for (const s of this.sockets) s.destroy();
    await new Promise<void>((resolve) => this.server.close(() => resolve()));
  }

  getRun(runId: string): Run | undefined {
    return this.runs.get(runId);
  }

  // ── connection handling (server side of node.go) ──

  private onConnection(sock: net.Socket): void {
    this.sockets.add(sock);
    sock.setNoDelay(true);
    let acc: Buffer = Buffer.alloc(0);
    let handshakeDone = false;
    sock.on("data", (chunk) => {
      acc = acc.length === 0 ? chunk : Buffer.concat([acc, chunk]);
      for (;;) {
        if (acc.length < 4) return;
        const len = acc.readUInt32LE(0);
        if (acc.length < 4 + len) return;
        const payload = Buffer.from(acc.subarray(4, 4 + len));
        acc = acc.subarray(4 + len);
        if (!handshakeDone) {
          handshakeDone = true;
          this.writeFrame(sock, encodeHandshake("mock-tasksd"));
          continue;
        }
        this.onFrame(sock, payload);
      }
    });
    sock.on("error", () => this.cleanup(sock));
    sock.on("close", () => this.cleanup(sock));
  }

  private cleanup(sock: net.Socket): void {
    this.sockets.delete(sock);
    const drop = (arr: Subscriber[]) => {
      for (let i = arr.length - 1; i >= 0; i--) if (arr[i].socket === sock) arr.splice(i, 1);
    };
    drop(this.wfSubs);
    drop(this.actSubs);
  }

  private onFrame(sock: net.Socket, data: Buffer): void {
    if (data.length < 8) return;
    const flag = data.readUInt32LE(4);
    if (flag !== REQ_FLAG_REQ) return; // acks / responses from client are ignored
    const reqId = data.readUInt32LE(0);
    const frame = Buffer.from(data.subarray(8));
    let msg: ZapMessage;
    try {
      msg = ZapMessage.parse(frame);
    } catch {
      return;
    }
    const opcode = msg.opcode();
    this.seenOpcodes.push(opcode);
    const response = this.dispatch(sock, opcode, frame);
    this.writeCorrelated(sock, reqId, REQ_FLAG_RESP, response);
  }

  private dispatch(sock: net.Socket, opcode: number, frame: Buffer): Buffer {
    switch (opcode) {
      case Opcode.StartWorkflow:
        return this.handleStart(opcode, frame);
      case Opcode.SignalWorkflow:
        return this.handleSignal(opcode, frame);
      case Opcode.SignalWithStartWorkflow:
        return this.handleStart(opcode, frame, true);
      case Opcode.DescribeWorkflow:
        return this.handleDescribe(opcode, frame);
      case Opcode.QueryWorkflow:
        return this.handleQuery(opcode, frame);
      case Opcode.CancelWorkflow:
        return this.handleTerminal(opcode, frame, WorkflowStatus.Canceled);
      case Opcode.TerminateWorkflow:
        return this.handleTerminal(opcode, frame, WorkflowStatus.Terminated);
      case Opcode.Health:
        return encodeResponseEnvelope(opcode, Buffer.from(JSON.stringify({ service: "hanzo-tasks", status: "ok" })));
      case Opcode.RegisterNamespace:
        return encodeResponseEnvelope(opcode, Buffer.alloc(0));
      case Opcode.SubscribeWorkflowTasks:
        return this.handleSubscribe(sock, opcode, frame, this.wfSubs);
      case Opcode.SubscribeActivityTasks:
        return this.handleSubscribe(sock, opcode, frame, this.actSubs);
      case Opcode.UnsubscribeTasks:
        return encodeResponseEnvelope(opcode, Buffer.alloc(0));
      case Opcode.RespondWorkflowTaskCompleted:
        return this.handleRespondWorkflow(opcode, frame);
      case Opcode.RespondActivityTaskCompleted:
        return this.handleRespondActivity(opcode, frame, false);
      case Opcode.RespondActivityTaskFailed:
        return this.handleRespondActivity(opcode, frame, true);
      case Opcode.RecordActivityTaskHeartbeat:
        return encodeHeartbeatResponse(false);
      default:
        return encodeResponseEnvelope(opcode, Buffer.alloc(0), 400, `unknown opcode 0x${opcode.toString(16)}`);
    }
  }

  // ── client RPCs ──

  private handleStart(opcode: number, frame: Buffer, withSignal = false): Buffer {
    const req = JSON.parse(envelopeBody(frame).toString("utf8"));
    const runId = randomHex();
    const workflowId = req.workflow_id || `wf-${runId}`;
    const run: Run = {
      workflowId,
      runId,
      workflowType: req.workflow_type,
      taskQueue: req.task_queue,
      history: [
        { eventId: 1, eventType: "WORKFLOW_EXECUTION_STARTED", attributes: { input: req.input ?? [] } },
      ],
      status: WorkflowStatus.Running,
      scheduledSeqs: new Set(),
      memo: req.memo,
    };
    if (withSignal && req.signal_name) {
      run.history.push({
        eventId: run.history.length + 1,
        eventType: "WORKFLOW_EXECUTION_SIGNALED",
        attributes: { signal: req.signal_name, input: req.signal_input },
      });
    }
    this.runs.set(runId, run);
    this.byWorkflowId.set(workflowId, runId);
    setImmediate(() => this.pushWorkflowTask(run));
    return encodeResponseEnvelope(opcode, Buffer.from(JSON.stringify({ run_id: runId })));
  }

  private handleSignal(opcode: number, frame: Buffer): Buffer {
    const req = JSON.parse(envelopeBody(frame).toString("utf8"));
    const run = this.resolveRun(req.workflow_id, req.run_id);
    if (run && run.status === WorkflowStatus.Running) {
      run.history.push({
        eventId: run.history.length + 1,
        eventType: "WORKFLOW_EXECUTION_SIGNALED",
        attributes: { signal: req.signal_name, input: req.input },
      });
      setImmediate(() => this.pushWorkflowTask(run));
    }
    return encodeResponseEnvelope(opcode, Buffer.alloc(0));
  }

  private handleDescribe(opcode: number, frame: Buffer): Buffer {
    const req = JSON.parse(envelopeBody(frame).toString("utf8"));
    const run = this.resolveRun(req.workflow_id, req.run_id);
    if (!run) return encodeResponseEnvelope(opcode, Buffer.alloc(0), 404, "not found");
    const info = {
      workflow_id: run.workflowId,
      run_id: run.runId,
      workflow_type: run.workflowType,
      status: run.status,
      history_length: run.history.length,
      task_queue: run.taskQueue,
      memo: run.memo,
    };
    return encodeResponseEnvelope(opcode, Buffer.from(JSON.stringify({ info })));
  }

  private handleQuery(opcode: number, frame: Buffer): Buffer {
    // Canned response: result is a Go []byte (base64) holding the value JSON.
    const value = this.opts.queryResult ?? null;
    const resultB64 = Buffer.from(JSON.stringify(value), "utf8").toString("base64");
    return encodeResponseEnvelope(opcode, Buffer.from(JSON.stringify({ result: resultB64 })));
  }

  private handleTerminal(opcode: number, frame: Buffer, status: number): Buffer {
    const req = JSON.parse(envelopeBody(frame).toString("utf8"));
    const run = this.resolveRun(req.workflow_id, req.run_id);
    if (run) run.status = status;
    return encodeResponseEnvelope(opcode, Buffer.alloc(0));
  }

  private handleSubscribe(sock: net.Socket, opcode: number, frame: Buffer, list: Subscriber[]): Buffer {
    const req = JSON.parse(envelopeBody(frame).toString("utf8"));
    const subId = randomHex();
    list.push({ socket: sock, taskQueue: req.task_queue });
    return encodeResponseEnvelope(opcode, Buffer.from(JSON.stringify({ subscription_id: subId })));
  }

  // ── worker responses ──

  private handleRespondWorkflow(opcode: number, frame: Buffer): Buffer {
    const { token, payload } = decodeFieldObject(frame);
    const runId = this.wfTokens.get(token.toString("utf8"));
    const run = runId ? this.runs.get(runId) : undefined;
    if (!run) return encodeResponseEnvelope(opcode, Buffer.alloc(0));

    const env = JSON.parse(payload.toString("utf8")) as { v: number; cmds: any[] };
    for (const cmd of env.cmds ?? []) {
      if (cmd.kind === 2) {
        const seq: number = cmd.seq ?? 0;
        if (run.scheduledSeqs.has(seq)) continue;
        run.scheduledSeqs.add(seq);
        const argsJSON = cmd.input ? Buffer.from(cmd.input, "base64").toString("utf8") : "[]";
        run.history.push({
          eventId: run.history.length + 1,
          eventType: "ACTIVITY_TASK_SCHEDULED",
          attributes: { seq, activityType: cmd.activityType, input: JSON.parse(argsJSON) },
        });
        this.pushActivityTask(run, seq, cmd.activityType, argsJSON, cmd.taskQueue || run.taskQueue, cmd);
      } else if (cmd.kind === 0) {
        run.status = WorkflowStatus.Completed;
      } else if (cmd.kind === 1) {
        run.status = WorkflowStatus.Failed;
      }
    }
    return encodeResponseEnvelope(opcode, Buffer.alloc(0));
  }

  private handleRespondActivity(opcode: number, frame: Buffer, failed: boolean): Buffer {
    const { token, payload } = decodeFieldObject(frame);
    const info = this.actTokens.get(token.toString("utf8"));
    const run = info ? this.runs.get(info.runId) : undefined;
    if (!run || !info) return encodeResponseEnvelope(opcode, Buffer.alloc(0));

    if (failed) {
      const failureVal = payload.length > 0 ? JSON.parse(payload.toString("utf8")) : null;
      run.history.push({
        eventId: run.history.length + 1,
        eventType: "ACTIVITY_TASK_FAILED",
        attributes: { seq: info.seq, failure: failureVal },
      });
    } else {
      const resultVal = payload.length > 0 ? JSON.parse(payload.toString("utf8")) : null;
      run.history.push({
        eventId: run.history.length + 1,
        eventType: "ACTIVITY_TASK_COMPLETED",
        attributes: { seq: info.seq, result: resultVal },
      });
    }
    if (run.status === WorkflowStatus.Running) setImmediate(() => this.pushWorkflowTask(run));
    return encodeResponseEnvelope(opcode, Buffer.alloc(0));
  }

  // ── deliveries (Send) ──

  private pushWorkflowTask(run: Run): void {
    if (run.status !== WorkflowStatus.Running) return;
    const sub = this.wfSubs.find((s) => s.taskQueue === run.taskQueue);
    if (!sub) return;
    const token = randomHex();
    this.wfTokens.set(token, run.runId);
    const body = {
      task_token: token,
      workflow_id: run.workflowId,
      run_id: run.runId,
      workflow_type_name: run.workflowType,
      history: JSON.stringify(run.history),
    };
    this.writeFrame(sub.socket, encodeEnvelope(Opcode.DeliverWorkflowTask, Buffer.from(JSON.stringify(body))));
  }

  private pushActivityTask(
    run: Run,
    seq: number,
    activityType: string,
    argsJSON: string,
    taskQueue: string,
    cmd: any,
  ): void {
    const sub = this.actSubs.find((s) => s.taskQueue === taskQueue);
    if (!sub) return;
    const token = randomHex();
    this.actTokens.set(token, { runId: run.runId, seq, activityType });
    const body = {
      task_token: token,
      workflow_id: run.workflowId,
      run_id: run.runId,
      activity_id: `act-${seq}`,
      activity_type_name: activityType,
      input: argsJSON,
      scheduled_time_ms: Date.now(),
      start_to_close_timeout_ms: cmd.startToCloseMs ?? 0,
      heartbeat_timeout_ms: cmd.heartbeatMs ?? 0,
    };
    this.writeFrame(sub.socket, encodeEnvelope(Opcode.DeliverActivityTask, Buffer.from(JSON.stringify(body))));
  }

  private resolveRun(workflowId: string, runId?: string): Run | undefined {
    if (runId) return this.runs.get(runId);
    const latest = this.byWorkflowId.get(workflowId);
    return latest ? this.runs.get(latest) : undefined;
  }

  // ── framing ──

  private writeFrame(sock: net.Socket, data: Buffer): void {
    const len = Buffer.allocUnsafe(4);
    len.writeUInt32LE(data.length >>> 0, 0);
    sock.write(len);
    sock.write(data);
  }

  private writeCorrelated(sock: net.Socket, reqId: number, flag: number, frame: Buffer): void {
    const wrapped = Buffer.allocUnsafe(8 + frame.length);
    wrapped.writeUInt32LE(reqId >>> 0, 0);
    wrapped.writeUInt32LE(flag >>> 0, 4);
    frame.copy(wrapped, 8);
    this.writeFrame(sock, wrapped);
  }
}

function randomHex(): string {
  return randomBytes(12).toString("hex");
}
