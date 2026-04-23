// Thin fetch wrapper over the hanzoai/tasks HTTP API at /v1/tasks/*.
// Browsers can only speak HTTP/JSON so this surface is JSON; every
// non-browser caller uses the ZAP binary transport on port 9652
// (_tasks._tcp) which is the canonical, fast RPC path. There is
// no /api/ prefix — that was the legacy gRPC-Gateway mount and it's
// gone. One way, one path.

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
  if (!res.ok) throw new ApiError(res.status, body, `GET ${path} → ${res.status}`)
  return body as T
}

export async function apiPost<T = unknown>(path: string, payload: unknown): Promise<T> {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(payload),
  })
  const body: unknown = await res.json().catch(() => null)
  if (!res.ok) throw new ApiError(res.status, body, `POST ${path} → ${res.status}`)
  return body as T
}

// ── Shape types (subset of Temporal protos we actually render) ─────

export interface Namespace {
  namespaceInfo: { name: string; state: string; description?: string; ownerEmail?: string }
  config?: { workflowExecutionRetentionTtl?: string }
}

export interface WorkflowExecution {
  execution: { workflowId: string; runId: string }
  type: { name: string }
  startTime?: string
  closeTime?: string
  status: string
  taskQueue?: string
}

export interface Schedule {
  scheduleId: string
  info?: {
    runningWorkflows?: unknown[]
    createTime?: string
    actionCount?: string
    missedCatchupWindow?: string
  }
  schedule?: {
    spec?: {
      interval?: Array<{ interval: string; phase?: string }>
      calendar?: Array<Record<string, string>>
      cronString?: string[]
    }
    action?: unknown
  }
}
