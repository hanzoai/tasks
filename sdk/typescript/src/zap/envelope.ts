// Copyright © 2026 Hanzo AI. MIT License.
//
// Frame codecs matching pkg/sdk/client/client.go and worker_transport.go.
//
// Two body framings exist on the Hanzo Tasks wire:
//
//  1. JSON envelope — a struct StartObject(24) with:
//       field 0  Bytes  JSON body
//       field 8  Uint32 status  (0/200 = ok)
//       field 12 Bytes  error detail
//     Used by every client RPC, Subscribe/Unsubscribe, StartChildWorkflow,
//     and every server-pushed delivery.
//
//  2. Field object — a struct StartObject(32) with:
//       field 0  Bytes  task token
//       field 8  Bytes  payload (commands / result / failure / details)
//     Used by RespondWorkflowTaskCompleted / RespondActivity* / Heartbeat.

import { Builder, ZapMessage } from "./wire";
import { Field } from "./opcodes";

const ENVELOPE_BODY = 0;
const ENVELOPE_STATUS = 8;
const ENVELOPE_ERROR = 12;
const ENVELOPE_OBJECT_SIZE = 24;
const FIELD_OBJECT_SIZE = 32;

const ZAP_MAGIC_PREFIX = Buffer.of(0x5a, 0x41, 0x50, 0x00);

/** Wrap a JSON body in the single-field ZAP envelope, flagged with opcode. */
export function encodeEnvelope(opcode: number, body: Buffer | Uint8Array | null): Buffer {
  const bodyBuf = body ? Buffer.from(body) : Buffer.alloc(0);
  const b = new Builder(bodyBuf.length + ENVELOPE_OBJECT_SIZE + 32);
  const obj = b.startObject(ENVELOPE_OBJECT_SIZE);
  obj.setBytes(ENVELOPE_BODY, bodyBuf);
  obj.finishAsRoot();
  return b.finishWithFlags((opcode << 8) & 0xffff);
}

/** Build a server-style response envelope (used by the test mock + parity). */
export function encodeResponseEnvelope(
  opcode: number,
  body: Buffer | Uint8Array | null,
  status = 0,
  errDetail = "",
): Buffer {
  const bodyBuf = body ? Buffer.from(body) : Buffer.alloc(0);
  const b = new Builder(bodyBuf.length + ENVELOPE_OBJECT_SIZE + 64);
  const obj = b.startObject(ENVELOPE_OBJECT_SIZE);
  obj.setBytes(ENVELOPE_BODY, bodyBuf);
  if (status !== 0) obj.setUint32(ENVELOPE_STATUS, status >>> 0);
  if (errDetail) obj.setText(ENVELOPE_ERROR, errDetail);
  obj.finishAsRoot();
  return b.finishWithFlags((opcode << 8) & 0xffff);
}

export interface DecodedEnvelope {
  status: number;
  detail: string;
  body: Buffer;
}

/** Read status / error / body out of a response frame. */
export function decodeEnvelope(frame: Buffer): DecodedEnvelope {
  const msg = ZapMessage.parse(frame);
  const root = msg.root();
  return {
    status: root.uint32(ENVELOPE_STATUS),
    detail: root.bytes(ENVELOPE_ERROR).toString("utf8"),
    body: root.bytes(ENVELOPE_BODY),
  };
}

/** Read just the JSON body from a delivery/ack envelope. */
export function envelopeBody(frame: Buffer): Buffer {
  return ZapMessage.parse(frame).root().bytes(ENVELOPE_BODY);
}

/**
 * Produce a complete ZAP frame for `opcode`. If `body` is already a framed
 * ZAP object (worker field-object path), re-stamp its flags; otherwise wrap
 * the raw bytes in the JSON envelope. Mirrors client.FrameBody.
 */
export function frameBody(opcode: number, body: Buffer): Buffer {
  if (body.length >= 8 && body.subarray(0, 4).equals(ZAP_MAGIC_PREFIX)) {
    const out = Buffer.from(body);
    out.writeUInt16LE((opcode << 8) & 0xffff, 6);
    return out;
  }
  return encodeEnvelope(opcode, body);
}

/** Build a field-object frame: token @0, payload @8. */
export function encodeFieldObject(token: Buffer, payload: Buffer): Buffer {
  const b = new Builder(token.length + payload.length + 64);
  const obj = b.startObject(FIELD_OBJECT_SIZE);
  obj.setBytes(Field.TaskToken, token);
  obj.setBytes(Field.Payload, payload);
  obj.finishAsRoot();
  return b.finish();
}

export interface FieldObject {
  token: Buffer;
  payload: Buffer;
}

/** Read a field-object frame (server side / tests). */
export function decodeFieldObject(frame: Buffer): FieldObject {
  const root = ZapMessage.parse(frame).root();
  return { token: root.bytes(Field.TaskToken), payload: root.bytes(Field.Payload) };
}

/** Encode the heartbeat response carrying the cancelRequested flag. */
export function encodeHeartbeatResponse(cancelRequested: boolean): Buffer {
  const b = new Builder(64);
  const obj = b.startObject(FIELD_OBJECT_SIZE);
  obj.setBool(Field.RespCancelRequested, cancelRequested);
  obj.finishAsRoot();
  return b.finish();
}

/** Read the cancelRequested flag from a heartbeat response frame. */
export function decodeHeartbeatResponse(frame: Buffer): boolean {
  return ZapMessage.parse(frame).root().bool(Field.RespCancelRequested);
}
