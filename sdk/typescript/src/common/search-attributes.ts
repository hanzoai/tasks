// Copyright © 2026 Hanzo AI. MIT License.
//
// Typed search attributes — the @temporalio/common surface social-orchestrator
// imports (defineSearchAttributeKey / SearchAttributeType / TypedSearchAttributes).
// On the Hanzo Tasks wire a workflow's search attributes are a flat
// name → value record carried on StartWorkflowRequest.search_attributes and
// stored on the execution (visibility-queryable via ListWorkflows). This module
// is the typed builder that flattens to that record.

/** Attribute value types. Mirrors @temporalio/common's SearchAttributeType. */
export enum SearchAttributeType {
  TEXT = "Text",
  KEYWORD = "Keyword",
  INT = "Int",
  DOUBLE = "Double",
  BOOL = "Bool",
  DATETIME = "Datetime",
  KEYWORD_LIST = "KeywordList",
}

/** A typed key handle returned by defineSearchAttributeKey. */
export interface SearchAttributeKey<_T = unknown> {
  readonly name: string;
  readonly type: SearchAttributeType;
}

/** One (key,value) pair used to construct TypedSearchAttributes. */
export interface SearchAttributePair<T = unknown> {
  key: SearchAttributeKey<T>;
  value: T;
}

/** Define a typed search-attribute key (name + value type). */
export function defineSearchAttributeKey<T = unknown>(
  name: string,
  type: SearchAttributeType,
): SearchAttributeKey<T> {
  return { name, type };
}

/**
 * An immutable typed collection of search attributes. Constructed from
 * (key,value) pairs; `toRecord()` flattens to the name→value record the wire
 * carries. Mirrors the minimal @temporalio/common TypedSearchAttributes
 * surface social depends on.
 */
export class TypedSearchAttributes {
  private readonly pairs: SearchAttributePair[];

  constructor(pairs: SearchAttributePair[] = []) {
    this.pairs = pairs.slice();
  }

  /** All (key,value) pairs. */
  getAll(): SearchAttributePair[] {
    return this.pairs.slice();
  }

  /** Value for a key, or undefined. */
  get<T>(key: SearchAttributeKey<T>): T | undefined {
    const hit = this.pairs.find((p) => p.key.name === key.name);
    return hit ? (hit.value as T) : undefined;
  }

  /** Flatten to the wire record { name: value }. */
  toRecord(): Record<string, unknown> {
    const out: Record<string, unknown> = {};
    for (const p of this.pairs) out[p.key.name] = p.value;
    return out;
  }
}

/**
 * Coerce a start-option search-attribute input (either a plain record or a
 * TypedSearchAttributes) to the flat wire record. undefined passes through.
 */
export function toSearchAttributeRecord(
  input: Record<string, unknown> | TypedSearchAttributes | undefined,
): Record<string, unknown> | undefined {
  if (input === undefined) return undefined;
  if (input instanceof TypedSearchAttributes) return input.toRecord();
  return input;
}
