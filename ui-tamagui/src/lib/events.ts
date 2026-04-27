// Realtime event tail. Subscribes to GET /v1/tasks/events (SSE) and
// fires a callback when relevant transitions arrive. v1's useRealtime
// is tied to SWR's mutate(); the v2 spike accepts an explicit
// onInvalidate callback so it works against our local useFetch.

import { useEffect } from 'react'

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

const KINDS: EventKind[] = [
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

// Subscribes to /v1/tasks/events and invokes `onEvent` for any kind in
// `kindFilter` (or all kinds if omitted) whose namespace matches `ns`.
export function useTaskEvents(
  ns: string | undefined,
  onEvent: (event: TaskEvent) => void,
  kindFilter?: EventKind[],
) {
  useEffect(() => {
    if (typeof window === 'undefined') return
    const es = new EventSource('/v1/tasks/events')
    const handle = (e: MessageEvent) => {
      let event: TaskEvent
      try {
        event = JSON.parse(e.data)
      } catch {
        return
      }
      if (ns && event.namespace && event.namespace !== ns) return
      if (kindFilter && !kindFilter.includes(event.kind)) return
      onEvent(event)
    }
    KINDS.forEach((k) => es.addEventListener(k, handle as EventListener))
    es.onmessage = handle
    es.onerror = () => {
      // Browser auto-reconnects. Surfacing as banner is a follow-up
      // polish task — same TODO carried over from v1.
    }
    return () => {
      KINDS.forEach((k) => es.removeEventListener(k, handle as EventListener))
      es.close()
    }
  }, [ns, onEvent, kindFilter])
}
