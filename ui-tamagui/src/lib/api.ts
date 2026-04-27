// Same fetch wrapper as v1 ui/src/lib/api.ts. Repeated rather than
// shared because v1 ships now and this v2 spike is intentionally
// stand-alone (no path back into the v1 source tree until cutover).

export class ApiError extends Error {
  constructor(
    public status: number,
    public body: unknown,
    message?: string,
  ) {
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
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(payload),
  })
  const body: unknown = await res.json().catch(() => null)
  if (!res.ok) {
    const msg =
      body && typeof body === 'object' && 'error' in body
        ? String((body as { error: unknown }).error)
        : `POST ${path} → ${res.status}`
    throw new ApiError(res.status, body, msg)
  }
  return body as T
}

// Mirror of v1 ui/src/lib/api.ts WorkflowExecution. Source of truth is
// pkg/tasks/types.go in the tasks Go module.
export interface WorkflowExecution {
  execution: { workflowId: string; runId: string }
  type: { name: string }
  startTime?: string
  closeTime?: string
  status: string
  taskQueue?: string
  historyLength?: number
}

export interface WorkflowsResponse {
  executions?: WorkflowExecution[]
}
