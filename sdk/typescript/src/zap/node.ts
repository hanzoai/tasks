// Copyright © 2026 Hanzo AI. MIT License.
//
// ZAP node — the TCP client half of luxfi/zap's node.go, in TypeScript.
//
// Socket framing:  [u32 LE length][message bytes]
// Handshake:       both sides send one message = StartObject(64) with the
//                  node id as raw bytes at offset 0 and its length at offset
//                  60. The client sends first, then reads the server's.
// Call:            request bytes are prefixed with [u32 reqID][u32 flag=1]
//                  before length-framing; the response carries flag=2 and the
//                  same reqID. Server-pushed deliveries arrive as bare frames
//                  (Send) and are routed to handlers by opcode.

import net from "node:net";
import { randomBytes } from "node:crypto";
import { Builder, ZapMessage } from "./wire";
import { encodeEnvelope, envelopeBody, frameBody } from "./envelope";

const REQ_FLAG_REQ = 1;
const REQ_FLAG_RESP = 2;
const MAX_MESSAGE = 10 * 1024 * 1024;

/** Transport is the request/response + push abstraction (mirrors Go Transport). */
export interface Transport {
  /** Issue one request for `opcode`; resolve with the response frame. */
  call(opcode: number, body: Buffer): Promise<Buffer>;
  /** Register a callback for server-pushed messages on `opcode`. */
  handle(opcode: number, fn: (from: string, body: Buffer) => void): void;
  /** Release transport resources. */
  close(): Promise<void>;
}

/**
 * TokenProvider yields the caller's IAM bearer for an identity-gated Tasks
 * frontend (cloud's ServeGated :9999). Called per request so a
 * client_credentials caller can refresh transparently. Mirrors the Go SDK's
 * client.TokenSource.
 */
export type TokenProvider = () => string | Promise<string>;

export interface ZapNodeOptions {
  host: string;
  port: number;
  /** Stable-ish client node id; a random suffix is appended for uniqueness. */
  nodeId?: string;
  dialTimeoutMs?: number;
  callTimeoutMs?: number;
  /**
   * IAM bearer source. When set, its token rides every JSON-body RPC (client
   * lifecycle + worker Subscribe) as `auth_token`, and the gated frontend
   * scopes the request to the token owner's org. Field-object frames (worker
   * Respond/Heartbeat) are authenticated by their HMAC task token and are not
   * touched.
   */
  token?: TokenProvider;
}

interface Pending {
  resolve: (frame: Buffer) => void;
  reject: (err: Error) => void;
  timer?: NodeJS.Timeout;
}

/** Build a handshake message: node id bytes @0, length @60, StartObject(64). */
function encodeHandshake(nodeId: string): Buffer {
  const b = new Builder(128);
  const obj = b.startObject(64);
  const idBytes = Buffer.from(nodeId, "utf8");
  const n = Math.min(idBytes.length, 60);
  for (let i = 0; i < n; i++) obj.setUint8(i, idBytes[i]);
  obj.setUint32(60, idBytes.length >>> 0);
  obj.finishAsRoot();
  return b.finish();
}

function decodeHandshakePeer(frame: Buffer): string {
  const root = ZapMessage.parse(frame).root();
  const len = root.uint32(60);
  if (len === 0 || len > 60) return "";
  const bytes = Buffer.alloc(len);
  for (let i = 0; i < len; i++) bytes[i] = root.uint8(i);
  return bytes.toString("utf8");
}

/** ZapNode is a single-peer ZAP client over one TCP connection. */
export class ZapNode implements Transport {
  private sock: net.Socket | null = null;
  private readonly nodeId: string;
  private peerId = "";
  private handshakeDone = false;
  private closed = false;
  private reqId = 0;
  private readonly pending = new Map<number, Pending>();
  private readonly handlers = new Map<number, (from: string, body: Buffer) => void>();
  private acc: Buffer = Buffer.alloc(0);
  private connectPromise: Promise<void> | null = null;

  constructor(private readonly opts: ZapNodeOptions) {
    const base = opts.nodeId && opts.nodeId.length > 0 ? opts.nodeId : "hanzo-tasks-sdk";
    this.nodeId = `${base}-${randomBytes(4).toString("hex")}`;
  }

  /** Establish the TCP connection and complete the ZAP handshake. */
  connect(): Promise<void> {
    if (this.connectPromise) return this.connectPromise;
    this.connectPromise = new Promise<void>((resolve, reject) => {
      const sock = net.createConnection({ host: this.opts.host, port: this.opts.port });
      sock.setNoDelay(true);
      this.sock = sock;

      const dialTimeout = this.opts.dialTimeoutMs ?? 10000;
      const timer = setTimeout(() => {
        sock.destroy(new Error(`zap dial timeout after ${dialTimeout}ms`));
      }, dialTimeout);

      let settled = false;
      const finishOk = () => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve();
      };
      const finishErr = (err: Error) => {
        if (settled) {
          this.failAll(err);
          return;
        }
        settled = true;
        clearTimeout(timer);
        reject(err);
      };

      sock.on("connect", () => {
        // Client sends its handshake first (mirrors ConnectDirect).
        this.writeFrame(encodeHandshake(this.nodeId));
      });
      sock.on("data", (chunk) => this.onData(chunk, finishOk));
      sock.on("error", (err) => finishErr(err));
      sock.on("close", () => {
        this.closed = true;
        this.failAll(new Error("zap connection closed"));
      });
    });
    return this.connectPromise;
  }

  private onData(chunk: Buffer, onHandshake: () => void): void {
    this.acc = this.acc.length === 0 ? chunk : Buffer.concat([this.acc, chunk]);
    for (;;) {
      if (this.acc.length < 4) return;
      const len = this.acc.readUInt32LE(0);
      if (len > MAX_MESSAGE) {
        this.sock?.destroy(new Error("zap: message too large"));
        return;
      }
      if (this.acc.length < 4 + len) return;
      const payload = this.acc.subarray(4, 4 + len);
      this.acc = this.acc.subarray(4 + len);

      if (!this.handshakeDone) {
        this.handshakeDone = true;
        try {
          this.peerId = decodeHandshakePeer(Buffer.from(payload));
        } catch {
          this.peerId = "";
        }
        onHandshake();
        continue;
      }
      this.route(Buffer.from(payload));
    }
  }

  private route(data: Buffer): void {
    if (data.length >= 8) {
      const flag = data.readUInt32LE(4);
      if (flag === REQ_FLAG_RESP) {
        const reqId = data.readUInt32LE(0);
        const p = this.pending.get(reqId);
        if (p) {
          this.pending.delete(reqId);
          if (p.timer) clearTimeout(p.timer);
          p.resolve(data.subarray(8));
        }
        return;
      }
      if (flag === REQ_FLAG_REQ) {
        // Server pushed a Call (waits for a correlated ack).
        const reqId = data.readUInt32LE(0);
        this.dispatch(data.subarray(8), reqId);
        return;
      }
    }
    // Bare frame (Send push) — no ack expected.
    this.dispatch(data, null);
  }

  private dispatch(frame: Buffer, reqId: number | null): void {
    let msg: ZapMessage;
    try {
      msg = ZapMessage.parse(frame);
    } catch {
      return;
    }
    const opcode = msg.opcode();
    const handler = this.handlers.get(opcode);
    if (handler) {
      let body: Buffer;
      try {
        body = envelopeBody(frame);
      } catch {
        body = Buffer.alloc(0);
      }
      try {
        handler(this.peerId, body);
      } catch {
        // Handlers must not crash the receive loop.
      }
    }
    if (reqId !== null) {
      // Ack a Call-style push so the server's Call completes.
      const ack = encodeEnvelope(opcode, null);
      this.writeCorrelated(reqId, REQ_FLAG_RESP, ack);
    }
  }

  async call(opcode: number, body: Buffer): Promise<Buffer> {
    if (this.closed || !this.sock) {
      throw new Error("zap: transport closed");
    }
    // Inject the IAM bearer into JSON-body RPCs (leading `{`); field-object
    // frames (worker Respond/Heartbeat) are HMAC-authenticated and left alone.
    if (this.opts.token && body.length > 0 && body[0] === 0x7b /* { */) {
      body = await this.injectAuthToken(body);
    }
    const frame = frameBody(opcode, body);
    this.reqId = (this.reqId + 1) >>> 0;
    const id = this.reqId;
    return new Promise<Buffer>((resolve, reject) => {
      const p: Pending = { resolve, reject };
      const timeout = this.opts.callTimeoutMs ?? 30000;
      if (timeout > 0) {
        p.timer = setTimeout(() => {
          if (this.pending.delete(id)) {
            reject(new Error(`zap call 0x${opcode.toString(16)} timed out after ${timeout}ms`));
          }
        }, timeout);
      }
      this.pending.set(id, p);
      try {
        this.writeCorrelated(id, REQ_FLAG_REQ, frame);
      } catch (err) {
        this.pending.delete(id);
        if (p.timer) clearTimeout(p.timer);
        reject(err as Error);
      }
    });
  }

  /** Merge `auth_token` into a JSON-object request body. */
  private async injectAuthToken(body: Buffer): Promise<Buffer> {
    const tok = await this.opts.token!();
    if (!tok) return body;
    try {
      const obj = JSON.parse(body.toString("utf8"));
      if (obj && typeof obj === "object" && !Array.isArray(obj)) {
        obj.auth_token = tok;
        return Buffer.from(JSON.stringify(obj), "utf8");
      }
    } catch {
      // Not a JSON object — leave the body untouched.
    }
    return body;
  }

  handle(opcode: number, fn: (from: string, body: Buffer) => void): void {
    this.handlers.set(opcode, fn);
  }

  async close(): Promise<void> {
    this.closed = true;
    const s = this.sock;
    this.sock = null;
    if (s) {
      await new Promise<void>((resolve) => {
        s.end(() => resolve());
        s.once("close", () => resolve());
        setTimeout(() => {
          s.destroy();
          resolve();
        }, 1000);
      });
    }
    this.failAll(new Error("zap: transport closed"));
  }

  private writeCorrelated(reqId: number, flag: number, frame: Buffer): void {
    const wrapped = Buffer.allocUnsafe(8 + frame.length);
    wrapped.writeUInt32LE(reqId >>> 0, 0);
    wrapped.writeUInt32LE(flag >>> 0, 4);
    frame.copy(wrapped, 8);
    this.writeFrame(wrapped);
  }

  private writeFrame(data: Buffer): void {
    if (!this.sock) throw new Error("zap: not connected");
    const len = Buffer.allocUnsafe(4);
    len.writeUInt32LE(data.length >>> 0, 0);
    this.sock.write(len);
    this.sock.write(data);
  }

  private failAll(err: Error): void {
    for (const [, p] of this.pending) {
      if (p.timer) clearTimeout(p.timer);
      p.reject(err);
    }
    this.pending.clear();
  }
}
