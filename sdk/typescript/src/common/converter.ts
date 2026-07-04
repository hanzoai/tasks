// Copyright © 2026 Hanzo AI. MIT License.
//
// Payload conversion. The v1 Hanzo Tasks wire encodes activity/workflow
// arguments and results as JSON. Arguments always travel as a JSON array
// (json.Marshal(args) in the Go SDK); a single result travels as the bare
// JSON of the returned value.

/** JSON-encode an argument list into the array form the server records. */
export function encodeArgs(args: unknown[]): Buffer {
  return Buffer.from(JSON.stringify(args ?? []), "utf8");
}

/** Decode a JSON args array. Empty/whitespace decodes to []. */
export function decodeArgs(bytes: Buffer | Uint8Array | null | undefined): unknown[] {
  if (!bytes || bytes.length === 0) return [];
  const s = Buffer.from(bytes).toString("utf8").trim();
  if (s.length === 0) return [];
  const v = JSON.parse(s);
  return Array.isArray(v) ? v : [v];
}

/** JSON-encode a single value (activity/workflow result). */
export function encodeValue(v: unknown): Buffer {
  return Buffer.from(JSON.stringify(v ?? null), "utf8");
}

/** Decode a single JSON value. Empty decodes to undefined. */
export function decodeValue<T = unknown>(bytes: Buffer | Uint8Array | null | undefined): T | undefined {
  if (!bytes || bytes.length === 0) return undefined;
  const s = Buffer.from(bytes).toString("utf8").trim();
  if (s.length === 0) return undefined;
  return JSON.parse(s) as T;
}
