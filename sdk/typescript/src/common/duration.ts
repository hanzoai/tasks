// Copyright © 2026 Hanzo AI. MIT License.
//
// Duration parsing. Accepts a number of milliseconds, or a human string in
// the `ms`-package style used by @temporalio ("1 minute", "30s", "2 hours",
// "500ms"). Returns milliseconds.

export type Duration = number | string;

const UNIT_MS: Record<string, number> = {
  ms: 1,
  msec: 1,
  msecs: 1,
  millisecond: 1,
  milliseconds: 1,
  s: 1000,
  sec: 1000,
  secs: 1000,
  second: 1000,
  seconds: 1000,
  m: 60_000,
  min: 60_000,
  mins: 60_000,
  minute: 60_000,
  minutes: 60_000,
  h: 3_600_000,
  hr: 3_600_000,
  hrs: 3_600_000,
  hour: 3_600_000,
  hours: 3_600_000,
  d: 86_400_000,
  day: 86_400_000,
  days: 86_400_000,
};

/** Convert a Duration to milliseconds. Undefined → 0. */
export function toMs(d: Duration | undefined): number {
  if (d === undefined || d === null) return 0;
  if (typeof d === "number") return Math.round(d);
  const s = d.trim();
  const m = /^(-?\d*\.?\d+)\s*([a-zA-Z]+)?$/.exec(s);
  if (!m) throw new Error(`hanzo/tasks: invalid duration "${d}"`);
  const value = parseFloat(m[1]);
  const unit = (m[2] ?? "ms").toLowerCase();
  const mult = UNIT_MS[unit];
  if (mult === undefined) throw new Error(`hanzo/tasks: unknown duration unit "${unit}" in "${d}"`);
  return Math.round(value * mult);
}
