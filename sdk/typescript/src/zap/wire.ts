// Copyright © 2026 Hanzo AI. MIT License.
//
// Byte-exact TypeScript port of the luxfi/zap (v0.2.0) message wire format —
// the "Zero-copy Application Protocol". Every byte this module emits is
// identical to what github.com/luxfi/zap's Builder/Object produce in Go, so a
// Hanzo Tasks server (tasksd) cannot tell a Go client from this one.
//
// Wire format:
//
//   Header (16 bytes):
//     Magic       [0:4]   "ZAP\0"
//     Version     [4:6]   u16 LE   (== 1)
//     Flags       [6:8]   u16 LE   (opcode is carried in the high byte)
//     RootOffset  [8:12]  u32 LE   offset to the root object
//     Size        [12:16] u32 LE   total message size including header
//   Data segment (8-byte aligned): Cap'n-Proto-style structs / bytes.
//
// A struct ("object") is a fixed region of `dataSize` bytes. Scalar fields
// live inline at their byte offset. A bytes/text field is a {relOffset u32,
// length u32} pair; relOffset is measured from the field's own position and
// points at the payload appended after the fixed section.

export const ZAP_MAGIC = Uint8Array.of(0x5a, 0x41, 0x50, 0x00); // "ZAP\0"
// Emit version 1 — accepted by every luxfi/zap (v0.2.0 and v1.2.0's
// backward-compatible Parse). Accept BOTH 1 and 2 on read: luxfi/zap v1.2.0's
// NewBuilder stamps version 2 in the header, but the generic object encoding
// (magic, version, flags, rootOffset, size + the field table) is byte-identical
// to v1 — only the platformvm TxKind schema, which this SDK never uses, differs.
// Cloud's embedded engine links zap v1.2.0 (MVS) and therefore emits version 2.
export const ZAP_VERSION = 1;
export const ZAP_VERSIONS_ACCEPTED = new Set([1, 2]);
export const HEADER_SIZE = 16;
const ALIGNMENT = 8;

/** Builder constructs a single ZAP message. Mirrors zap.Builder. */
export class Builder {
  private buf: Buffer;
  private pos: number;
  private rootOffset = 0;

  constructor(capacity = 256) {
    if (capacity < HEADER_SIZE) capacity = 256;
    this.buf = Buffer.alloc(capacity);
    this.pos = HEADER_SIZE;
    this.buf[0] = ZAP_MAGIC[0];
    this.buf[1] = ZAP_MAGIC[1];
    this.buf[2] = ZAP_MAGIC[2];
    this.buf[3] = ZAP_MAGIC[3];
    this.buf.writeUInt16LE(ZAP_VERSION, 4);
  }

  /** Start an object with a fixed section of `dataSize` bytes. */
  startObject(dataSize: number): ObjectBuilder {
    this.align(ALIGNMENT);
    return new ObjectBuilder(this, this.pos, dataSize);
  }

  finish(): Buffer {
    this.buf.writeUInt32LE(this.rootOffset >>> 0, 8);
    this.buf.writeUInt32LE(this.pos >>> 0, 12);
    return this.buf.subarray(0, this.pos);
  }

  finishWithFlags(flags: number): Buffer {
    this.buf.writeUInt16LE(flags & 0xffff, 6);
    return this.finish();
  }

  // ── internals shared with ObjectBuilder ──

  /** Current backing buffer. Re-fetch after any growth. */
  raw(): Buffer {
    return this.buf;
  }

  position(): number {
    return this.pos;
  }

  setPosition(p: number): void {
    this.pos = p;
  }

  setRoot(off: number): void {
    this.rootOffset = off;
  }

  /** Ensure capacity for `n` more bytes past the current position. */
  grow(n: number): void {
    if (this.pos + n <= this.buf.length) return;
    let newCap = this.buf.length * 2;
    if (newCap < this.pos + n) newCap = this.pos + n;
    const nb = Buffer.alloc(newCap);
    this.buf.copy(nb, 0, 0, this.pos);
    this.buf = nb;
  }

  /** Grow + zero-fill so the buffer covers up to absolute `needed`. */
  ensureAbs(needed: number): void {
    if (needed > this.pos) {
      this.grow(needed - this.pos);
      this.buf.fill(0, this.pos, needed);
      this.pos = needed;
    }
  }

  private align(a: number): void {
    const padding = (a - (this.pos % a)) % a;
    this.grow(padding);
    for (let i = 0; i < padding; i++) {
      this.buf[this.pos] = 0;
      this.pos++;
    }
  }
}

interface DeferredBytes {
  fieldOffset: number;
  data: Buffer;
}

/** ObjectBuilder writes fields into a struct region. Mirrors zap.ObjectBuilder. */
export class ObjectBuilder {
  private deferred: DeferredBytes[] = [];

  constructor(
    private readonly b: Builder,
    public readonly startPos: number,
    private readonly dataSize: number,
  ) {}

  private ensureField(endOffset: number): void {
    this.b.ensureAbs(this.startPos + endOffset);
  }

  setUint8(fieldOffset: number, v: number): void {
    this.ensureField(fieldOffset + 1);
    this.b.raw()[this.startPos + fieldOffset] = v & 0xff;
  }

  setUint16(fieldOffset: number, v: number): void {
    this.ensureField(fieldOffset + 2);
    this.b.raw().writeUInt16LE(v & 0xffff, this.startPos + fieldOffset);
  }

  setUint32(fieldOffset: number, v: number): void {
    this.ensureField(fieldOffset + 4);
    this.b.raw().writeUInt32LE(v >>> 0, this.startPos + fieldOffset);
  }

  setInt8(fieldOffset: number, v: number): void {
    this.setUint8(fieldOffset, v & 0xff);
  }

  setBool(fieldOffset: number, v: boolean): void {
    this.setUint8(fieldOffset, v ? 1 : 0);
  }

  /** Set a bytes field. The payload is appended during finish(). */
  setBytes(fieldOffset: number, v: Buffer | Uint8Array | null | undefined): void {
    this.ensureField(fieldOffset + 8);
    const raw = this.b.raw();
    if (!v || v.length === 0) {
      raw.writeUInt32LE(0, this.startPos + fieldOffset);
      raw.writeUInt32LE(0, this.startPos + fieldOffset + 4);
      return;
    }
    const data = Buffer.from(v);
    this.deferred.push({ fieldOffset, data });
    raw.writeUInt32LE(data.length >>> 0, this.startPos + fieldOffset + 4);
  }

  setText(fieldOffset: number, s: string): void {
    this.setBytes(fieldOffset, Buffer.from(s, "utf8"));
  }

  finish(): number {
    this.ensureField(this.dataSize);
    for (const e of this.deferred) {
      const dataPos = this.b.position();
      this.b.grow(e.data.length);
      e.data.copy(this.b.raw(), this.b.position());
      this.b.setPosition(this.b.position() + e.data.length);
      const fieldAbsPos = this.startPos + e.fieldOffset;
      const relOffset = dataPos - fieldAbsPos;
      this.b.raw().writeInt32LE(relOffset, fieldAbsPos);
    }
    return this.startPos;
  }

  finishAsRoot(): number {
    const off = this.finish();
    this.b.setRoot(off);
    return off;
  }
}

/** ZapMessage is a zero-copy reader over a complete frame. Mirrors zap.Message. */
export class ZapMessage {
  private constructor(public readonly data: Buffer) {}

  static parse(input: Buffer | Uint8Array): ZapMessage {
    const data = Buffer.isBuffer(input) ? input : Buffer.from(input);
    if (data.length < HEADER_SIZE) throw new Error("zap: buffer too small");
    if (data[0] !== 0x5a || data[1] !== 0x41 || data[2] !== 0x50 || data[3] !== 0x00) {
      throw new Error("zap: invalid magic");
    }
    const version = data.readUInt16LE(4);
    if (!ZAP_VERSIONS_ACCEPTED.has(version)) throw new Error(`zap: unsupported version ${version}`);
    const size = data.readUInt32LE(12);
    if (size > data.length) throw new Error("zap: declared size exceeds buffer");
    return new ZapMessage(data.subarray(0, size));
  }

  bytes(): Buffer {
    return this.data;
  }

  flags(): number {
    return this.data.readUInt16LE(6);
  }

  /** Opcode carried in the flags high byte. */
  opcode(): number {
    return this.flags() >> 8;
  }

  root(): ZapObject {
    const offset = this.data.readUInt32LE(8);
    return new ZapObject(this.data, offset);
  }
}

/** ZapObject is a zero-copy view into a struct. Mirrors zap.Object. */
export class ZapObject {
  constructor(
    private readonly data: Buffer,
    public readonly offset: number,
  ) {}

  isNull(): boolean {
    return this.offset === 0;
  }

  uint8(fieldOffset: number): number {
    const pos = this.offset + fieldOffset;
    return pos >= this.data.length ? 0 : this.data[pos];
  }

  uint16(fieldOffset: number): number {
    const pos = this.offset + fieldOffset;
    return pos + 2 > this.data.length ? 0 : this.data.readUInt16LE(pos);
  }

  uint32(fieldOffset: number): number {
    const pos = this.offset + fieldOffset;
    return pos + 4 > this.data.length ? 0 : this.data.readUInt32LE(pos);
  }

  bool(fieldOffset: number): boolean {
    return this.uint8(fieldOffset) !== 0;
  }

  bytes(fieldOffset: number): Buffer {
    const pos = this.offset + fieldOffset;
    if (pos + 4 > this.data.length) return Buffer.alloc(0);
    const relOffset = this.data.readInt32LE(pos);
    if (relOffset === 0) return Buffer.alloc(0);
    const lenPos = pos + 4;
    if (lenPos + 4 > this.data.length) return Buffer.alloc(0);
    const length = this.data.readUInt32LE(lenPos);
    const absPos = pos + relOffset;
    if (absPos < 0 || absPos + length > this.data.length) return Buffer.alloc(0);
    return this.data.subarray(absPos, absPos + length);
  }

  text(fieldOffset: number): string {
    return this.bytes(fieldOffset).toString("utf8");
  }
}
