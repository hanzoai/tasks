// Thin fetch wrapper over the hanzoai/tasks HTTP API at /v1/tasks/*.
// The browser is the only HTTP caller; every other client speaks the
// canonical ZAP binary transport on _tasks._tcp:9999. There is no
// /api/ prefix and no v2 — append-only opcode evolution behind /v1.

export class ApiError extends Error {
  constructor(public status: number, public body: unknown, message?: string) {
    super(message ?? `HTTP ${status}`)
    this.name = 'ApiError'
  }
}

export async function fetcher<T = unknown>(path: string): Promise<T> {
  const res = await fetch(path, { headers: { Accept: 'application/json' } })
  const contentType = res.headers.get('content-type') || ''
  const body: unknown = contentType.includes('application/json')
    ? await res.json().catch(() => null)
    : await res.text().catch(() => null)
  if (!res.ok) {
    const msg =
      body && typeof body === 'object' && 'error' in body
        ? String((body as { error: unknown }).error)
        : `${path} → ${res.status}`
    throw new ApiError(res.status, body, msg)
  }
  return body as T
}

export async function apiPost<T = unknown>(path: string, payload: unknown): Promise<T> {
  return apiSend('POST', path, payload)
}

export async function apiDelete<T = unknown>(path: string): Promise<T> {
  return apiSend('DELETE', path, undefined)
}

async function apiSend<T>(method: string, path: string, payload: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers:
      payload === undefined
        ? { Accept: 'application/json' }
        : { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: payload === undefined ? undefined : JSON.stringify(payload),
  })
  const body: unknown = await res.json().catch(() => null)
  if (!res.ok) {
    const msg =
      body && typeof body === 'object' && 'error' in body
        ? String((body as { error: unknown }).error)
        : `${method} ${path} → ${res.status}`
    throw new ApiError(res.status, body, msg)
  }
  return body as T
}

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
