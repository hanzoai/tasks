// Realtime event tail. Subscribes to GET /v1/tasks/events (SSE) and
// invalidates SWR caches on relevant transitions so list views update
// without polling. The same stream is reachable over the canonical
// ZAP wire from Go callers via pkg/sdk/client.SubscribeEvents.

import { useEffect } from 'react'
import { useSWRConfig } from 'swr'

export type EventKind =
  | 'workflow.started'
  | 'workflow.canceled'
  | 'workflow.terminated'
  | 'workflow.signaled'
  | 'schedule.created'
  | 'schedule.paused'
  | 'schedule.resumed'
  | 'schedule.deleted'
  | 'namespace.registered'
  | 'batch.started'

export interface TaskEvent {
  kind: EventKind
  namespace?: string
  workflow_id?: string
  run_id?: string
  schedule_id?: string
  batch_id?: string
  at: string
  data?: unknown
}

// useRealtime opens a single shared EventSource and invalidates SWR
// caches when events affect the listed surfaces. React StrictMode
// double-mounts in dev — the cleanup closes the previous handle so
// we never run two streams against the same backend.
export function useRealtime(namespace?: string) {
  const { mutate } = useSWRConfig()
  useEffect(() => {
    if (typeof window === 'undefined') return
    const es = new EventSource('/v1/tasks/events')
    const handler = (e: MessageEvent) => {
      let ev: TaskEvent
      try {
        ev = JSON.parse(e.data)
      } catch {
        return
      }
      const ns = ev.namespace ?? namespace
      if (!ns) return
      switch (ev.kind) {
        case 'workflow.started':
        case 'workflow.canceled':
        case 'workflow.terminated':
        case 'workflow.signaled':
          mutate(
            (key) =>
              typeof key === 'string' &&
              key.startsWith(`/v1/tasks/namespaces/${encodeURIComponent(ns)}/workflows`)
          )
          break
        case 'schedule.created':
        case 'schedule.paused':
        case 'schedule.resumed':
        case 'schedule.deleted':
          mutate(`/v1/tasks/namespaces/${encodeURIComponent(ns)}/schedules`)
          break
        case 'namespace.registered':
          mutate('/v1/tasks/namespaces?pageSize=200')
          break
        case 'batch.started':
          mutate(`/v1/tasks/namespaces/${encodeURIComponent(ns)}/batches`)
          break
      }
    }
    // The server emits "event: <kind>\ndata: {...}\n\n" so each kind is
    // also a named event. Listen on each so this hook works whether the
    // server uses generic 'message' or named events.
    const kinds: EventKind[] = [
      'workflow.started',
      'workflow.canceled',
      'workflow.terminated',
      'workflow.signaled',
      'schedule.created',
      'schedule.paused',
      'schedule.resumed',
      'schedule.deleted',
      'namespace.registered',
      'batch.started',
    ]
    kinds.forEach((k) => es.addEventListener(k, handler as EventListener))
    es.onmessage = handler
    es.onerror = () => {
      // Browser auto-reconnects; nothing to do. Suppressing the error is
      // intentional — surfacing it as a banner is a v1.42 polish task.
    }
    return () => {
      kinds.forEach((k) => es.removeEventListener(k, handler as EventListener))
      es.close()
    }
  }, [namespace, mutate])
}
