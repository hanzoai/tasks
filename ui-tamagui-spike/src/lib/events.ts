// Realtime event tail for tasks. Thin wrapper over `useEvents` from
// @hanzogui/admin that pins the SSE URL to /v1/tasks/events and adds
// the namespace-scoping filter the tasks server emits.

import { useCallback } from 'react'
import { useEvents } from '@hanzogui/admin'

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

const ALL_KINDS: EventKind[] = [
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

// useTaskEvents subscribes to /v1/tasks/events and invokes onEvent
// for any kind in kindFilter (or all kinds if omitted) whose
// namespace matches ns.
export function useTaskEvents(
  ns: string | undefined,
  onEvent: (event: TaskEvent) => void,
  kindFilter?: EventKind[],
) {
  const filter = useCallback(
    (event: TaskEvent) => {
      if (ns && event.namespace && event.namespace !== ns) return false
      if (kindFilter && !kindFilter.includes(event.kind)) return false
      return true
    },
    [ns, kindFilter],
  )
  useEvents<TaskEvent>({
    url: '/v1/tasks/events',
    kinds: kindFilter ?? ALL_KINDS,
    filter,
    onEvent,
  })
}
