// Copyright © 2026 Hanzo AI. MIT License.

import { describe, it, expect } from "vitest";
import {
  Builder,
  ZapMessage,
  encodeEnvelope,
  decodeEnvelope,
  encodeResponseEnvelope,
  encodeFieldObject,
  decodeFieldObject,
  frameBody,
  encodeHeartbeatResponse,
  decodeHeartbeatResponse,
} from "../src/zap";

describe("ZAP wire — byte-exact layout (parity with luxfi/zap v0.2.0)", () => {
  it("encodes a JSON envelope with the exact Go byte layout", () => {
    const body = Buffer.from("{}", "utf8");
    const opcode = 0x0060;
    const frame = encodeEnvelope(opcode, body);

    // header
    expect(frame.subarray(0, 4).toString("latin1")).toBe("ZAP\x00");
    expect(frame.readUInt16LE(4)).toBe(1); // version
    expect(frame.readUInt16LE(6)).toBe((opcode << 8) & 0xffff); // flags carry opcode
    expect(frame.readUInt32LE(8)).toBe(16); // root offset = HeaderSize
    expect(frame.readUInt32LE(12)).toBe(42); // total size = 16 header + 24 obj + 2 body

    // object: bytes field 0 → relOffset 24, length 2; payload appended at 40
    expect(frame.readInt32LE(16)).toBe(24);
    expect(frame.readUInt32LE(20)).toBe(2);
    expect(frame.subarray(40, 42).toString("utf8")).toBe("{}");
    expect(frame.length).toBe(42);
  });

  it("round-trips a response envelope with status + error detail", () => {
    const frame = encodeResponseEnvelope(0x0064, Buffer.from('{"ok":true}'), 500, "boom");
    const { status, detail, body } = decodeEnvelope(frame);
    expect(status).toBe(500);
    expect(detail).toBe("boom");
    expect(JSON.parse(body.toString("utf8"))).toEqual({ ok: true });
  });

  it("round-trips a field object (token @0, payload @8)", () => {
    const token = Buffer.from("abc123", "utf8");
    const payload = Buffer.from('{"v":1,"cmds":[]}', "utf8");
    const frame = encodeFieldObject(token, payload);
    const decoded = decodeFieldObject(frame);
    expect(decoded.token.toString("utf8")).toBe("abc123");
    expect(decoded.payload.toString("utf8")).toBe('{"v":1,"cmds":[]}');
  });

  it("frameBody re-stamps the opcode flags on an already-framed body", () => {
    const framed = encodeFieldObject(Buffer.from("t"), Buffer.from("p"));
    const restamped = frameBody(0x00a2, framed);
    expect(ZapMessage.parse(restamped).opcode()).toBe(0x00a2);
    // still decodes as a field object
    const d = decodeFieldObject(restamped);
    expect(d.token.toString()).toBe("t");
    expect(d.payload.toString()).toBe("p");
  });

  it("frameBody wraps a raw JSON body in the envelope", () => {
    const wrapped = frameBody(0x0060, Buffer.from('{"a":1}'));
    expect(decodeEnvelope(wrapped).body.toString("utf8")).toBe('{"a":1}');
  });

  it("empty bytes fields become null pointers", () => {
    const frame = encodeEnvelope(0x0090, null);
    const { body } = decodeEnvelope(frame);
    expect(body.length).toBe(0);
  });

  it("heartbeat response carries the cancelRequested bool", () => {
    expect(decodeHeartbeatResponse(encodeHeartbeatResponse(true))).toBe(true);
    expect(decodeHeartbeatResponse(encodeHeartbeatResponse(false))).toBe(false);
  });

  it("Builder/Reader round-trip scalars and text at fixed offsets", () => {
    const b = new Builder(64);
    const obj = b.startObject(24);
    obj.setUint32(8, 0xdeadbeef);
    obj.setText(0, "hello");
    obj.setBool(16, true);
    obj.finishAsRoot();
    const msg = ZapMessage.parse(b.finish());
    const root = msg.root();
    expect(root.uint32(8)).toBe(0xdeadbeef);
    expect(root.text(0)).toBe("hello");
    expect(root.bool(16)).toBe(true);
  });
});
