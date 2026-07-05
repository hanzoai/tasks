// Copyright © 2026 Hanzo AI. MIT License.
//
// Failure serde — byte-compatible with pkg/sdk/temporal/serde.go. The wire
// shape carried in the `failure` bytes of RespondActivityTaskFailed and the
// ACTIVITY_TASK_FAILED history attribute is:
//
//   { "v": 1, "p": { message, code, nonRetryable?, details?, cause? } }
//
// A version other than 1 decodes to a non-retryable DecodeError. Encoding
// never throws — a workflow/activity must always be able to report failure.

export const FailureCode = {
  Application: "ApplicationError",
  Canceled: "CanceledError",
  Timeout: "TimeoutError",
  Decode: "DecodeError",
} as const;

const ENVELOPE_VERSION = 1;

/** TaskError is the single concrete error type this SDK emits and decodes. */
export class TaskError extends Error {
  readonly code: string;
  readonly nonRetryable: boolean;
  readonly details: unknown[];
  readonly cause?: TaskError;

  constructor(
    message: string,
    code: string = FailureCode.Application,
    nonRetryable = false,
    details: unknown[] = [],
    cause?: TaskError,
  ) {
    super(message);
    this.name = "TaskError";
    this.code = code || FailureCode.Application;
    this.nonRetryable = nonRetryable;
    this.details = details;
    this.cause = cause;
  }

  /** True when this is (or wraps) a cancellation. */
  get isCanceled(): boolean {
    return this.code === FailureCode.Canceled;
  }

  /** True when this is (or wraps) a timeout. */
  get isTimeout(): boolean {
    return this.code === FailureCode.Timeout;
  }
}

/**
 * ApplicationFailure — a business error a workflow or activity raises. Mirrors
 * @temporalio/common's ApplicationFailure: `type` is the error classifier
 * (e.g. "refresh_token", "bad_body") that workflow code switches on. On the
 * wire it is the failure `code`, so `type` is a view over `code`.
 */
export class ApplicationFailure extends TaskError {
  constructor(message: string, type: string = FailureCode.Application, nonRetryable = false, details: unknown[] = [], cause?: TaskError) {
    super(message, type, nonRetryable, details, cause);
    this.name = "ApplicationFailure";
  }

  /** The error classifier — the field @temporalio workflow code reads. */
  get type(): string {
    return this.code;
  }

  static create(opts: { message: string; type?: string; nonRetryable?: boolean; details?: unknown[] }): ApplicationFailure {
    return new ApplicationFailure(opts.message, opts.type ?? FailureCode.Application, opts.nonRetryable ?? false, opts.details ?? []);
  }

  static nonRetryable(message: string, type?: string, ...details: unknown[]): ApplicationFailure {
    return new ApplicationFailure(message, type ?? FailureCode.Application, true, details);
  }

  static retryable(message: string, type?: string, ...details: unknown[]): ApplicationFailure {
    return new ApplicationFailure(message, type ?? FailureCode.Application, false, details);
  }
}

/**
 * ActivityFailure — wraps the cause raised by a failed activity, mirroring
 * @temporalio's ActivityFailure. Workflow code pattern-matches
 * `err instanceof ActivityFailure && err.cause instanceof ApplicationFailure`,
 * so a proxied activity that fails rejects with an ActivityFailure whose
 * `cause` is the decoded ApplicationFailure.
 */
export class ActivityFailure extends TaskError {
  readonly activityType: string;

  constructor(activityType: string, cause: TaskError) {
    super(`activity ${activityType} failed: ${cause.message}`, cause.code, cause.nonRetryable, cause.details, cause);
    this.name = "ActivityFailure";
    this.activityType = activityType;
  }
}

/** Coerce a decoded TaskError to an ApplicationFailure (preserving cause). */
export function asApplicationFailure(err: TaskError): ApplicationFailure {
  if (err instanceof ApplicationFailure) return err;
  return new ApplicationFailure(err.message, err.code, err.nonRetryable, err.details, err.cause);
}

interface FailurePayload {
  message: string;
  code: string;
  nonRetryable?: boolean;
  details?: unknown[];
  cause?: FailurePayload;
}

interface FailureEnvelope {
  v: number;
  p: FailurePayload;
}

function toPayload(err: unknown): FailurePayload {
  if (err instanceof TaskError) {
    const p: FailurePayload = {
      message: err.message,
      code: err.code || FailureCode.Application,
    };
    if (err.nonRetryable) p.nonRetryable = true;
    if (err.details && err.details.length > 0) p.details = err.details;
    if (err.cause) p.cause = toPayload(err.cause);
    return p;
  }
  if (err instanceof Error) {
    return { message: err.message, code: FailureCode.Application };
  }
  return { message: String(err), code: FailureCode.Application };
}

function fromPayload(p: FailurePayload | undefined): TaskError | undefined {
  if (!p) return undefined;
  return new TaskError(
    p.message,
    p.code || FailureCode.Application,
    p.nonRetryable ?? false,
    Array.isArray(p.details) ? p.details : [],
    fromPayload(p.cause),
  );
}

/** Serialise any error into the on-wire failure bytes. Never throws. */
export function encodeFailure(err: unknown): Buffer {
  let env: FailureEnvelope;
  try {
    env = { v: ENVELOPE_VERSION, p: toPayload(err) };
    return Buffer.from(JSON.stringify(env), "utf8");
  } catch {
    env = {
      v: ENVELOPE_VERSION,
      p: { message: "failed to encode failure", code: FailureCode.Decode, nonRetryable: true },
    };
    return Buffer.from(JSON.stringify(env), "utf8");
  }
}

/** Parse failure bytes back into a TaskError. Fail-secure on hostile input. */
export function decodeFailure(bytes: Buffer | Uint8Array | null | undefined): TaskError {
  if (!bytes || bytes.length === 0) {
    return new TaskError("empty failure payload", FailureCode.Decode, true);
  }
  let env: FailureEnvelope;
  try {
    env = JSON.parse(Buffer.from(bytes).toString("utf8"));
  } catch (e) {
    return new TaskError(`failure envelope unmarshal: ${(e as Error).message}`, FailureCode.Decode, true);
  }
  if (env.v !== ENVELOPE_VERSION) {
    return new TaskError(`unsupported failure envelope version ${env.v}`, FailureCode.Decode, true);
  }
  const decoded = fromPayload(env.p);
  return decoded ?? new TaskError("nil failure payload", FailureCode.Decode, true);
}
