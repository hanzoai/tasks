// Tasks-specific API surface. Transport (apiPost / apiDelete /
// ApiError / useFetch) lives in @hanzogui/admin. This file only owns
// the wire-shape types that match pkg/tasks/types.go JSON tags.
//
// There is no /api/ prefix and no v2 — append-only opcode evolution
// behind /v1.

export { ApiError, apiPost, apiDelete } from '@hanzogui/admin'

// ── shape types — match pkg/tasks/types.go JSON tags exactly ────────

export interface Namespace {
  namespaceInfo: {
    name: string
    state: string
    description?: string
    ownerEmail?: string
    region?: string
    createTime?: string
  }
  config: {
    workflowExecutionRetentionTtl: string
    apsLimit: number
  }
  isActive: boolean
}

export interface WorkflowExecution {
  execution: { workflowId: string; runId: string }
  type: { name: string }
  startTime?: string
  closeTime?: string
  status: string
  taskQueue?: string
  historyLength?: number
  input?: unknown
  result?: unknown
  memo?: unknown
}

export interface WorkflowsResponse {
  executions?: WorkflowExecution[]
}

export interface Schedule {
  scheduleId: string
  namespace: string
  spec: {
    cronString?: string[]
    interval?: Array<{ interval: string; phase?: string }>
  }
  action: {
    workflowType: { name: string }
    taskQueue: string
    input?: unknown
  }
  state: { paused: boolean; note?: string }
  info: { createTime: string; updateTime?: string; actionCount: number }
}

export interface BatchOperation {
  batchId: string
  namespace: string
  operation: string
  reason: string
  query: string
  state: string
  startTime: string
  closeTime?: string
  totalOperationCount: number
  completeOperationCount: number
}

export interface Deployment {
  seriesName: string
  namespace: string
  buildIds: Array<{ buildId: string; state: string; createTime: string }>
  defaultBuildId: string
  createTime: string
}

export interface NexusEndpoint {
  name: string
  namespace: string
  description?: string
  target: string
  createTime: string
}

export interface Identity {
  email: string
  namespace: string
  role: string
  grantTime: string
}

// statusVariant maps WORKFLOW_EXECUTION_STATUS_* constants to the
// admin Badge variant enum. Tasks-specific because Workflow status
// strings are tasks-specific.
import type { StatusVariant } from '@hanzogui/admin'

export function statusVariant(s: string): StatusVariant {
  switch (s) {
    case 'WORKFLOW_EXECUTION_STATUS_RUNNING':
    case 'WORKFLOW_EXECUTION_STATUS_COMPLETED':
      return 'success'
    case 'WORKFLOW_EXECUTION_STATUS_FAILED':
    case 'WORKFLOW_EXECUTION_STATUS_TIMED_OUT':
    case 'WORKFLOW_EXECUTION_STATUS_TERMINATED':
      return 'destructive'
    case 'WORKFLOW_EXECUTION_STATUS_CANCELED':
      return 'warning'
    default:
      return 'muted'
  }
}

export function shortStatus(s: string) {
  return s.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()
}
