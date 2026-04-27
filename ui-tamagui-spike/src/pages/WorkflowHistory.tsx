// Workflow history — synthetic event timeline. Native engine doesn't
// yet emit per-event durable history, so the server returns start +
// signal counter + (if closed) terminal transition. We label the
// response as synthetic so this page can show the right hint.

import { Link, useParams, useSearchParams } from 'react-router-dom'
import {
  Card,
  H1,
  H4,
  Text,
  XStack,
  YStack,
} from 'hanzogui'
import { ChevronLeft, Circle } from '@hanzogui/lucide-icons-2'
import {
  Alert,
  Badge,
  Empty,
  ErrorState,
  LoadingState,
  formatTimestamp,
  useFetch,
} from '@hanzogui/admin'
import { useTaskEvents } from '../lib/events'

interface HistoryEvent {
  eventId: string
  eventType: string
  eventTime?: string
  attributes?: Record<string, unknown>
}

interface HistoryResp {
  events?: HistoryEvent[]
  synthetic?: boolean
}

export function WorkflowHistoryPage() {
  const { ns, workflowId } = useParams()
  const [sp] = useSearchParams()
  const runId = sp.get('runId') ?? ''
  const namespace = ns!
  const qs = runId ? `?runId=${encodeURIComponent(runId)}` : ''
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/history${qs}`
  const { data, error, isLoading, mutate } = useFetch<HistoryResp>(url)

  useTaskEvents(namespace, () => void mutate(), [
    'workflow.canceled',
    'workflow.terminated',
    'workflow.signaled',
  ])

  if (error) return <ErrorState error={error as Error} />
  if (isLoading || !data) return <LoadingState />

  const events = data.events ?? []

  return (
    <YStack gap="$5">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}${qs}`}
        style={{ textDecoration: 'none', color: 'inherit' }}
      >
        <XStack items="center" gap="$1.5" hoverStyle={{ opacity: 0.8 }}>
          <ChevronLeft size={14} color="#7e8794" />
          <Text fontSize="$2" color="$placeholderColor">
            {workflowId}
          </Text>
        </XStack>
      </Link>

      <YStack gap="$1">
        <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
          HISTORY
        </Text>
        <H1 size="$7" color="$color" fontWeight="600">
          {workflowId}
        </H1>
        {runId && (
          <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$2" color="$placeholderColor">
            {runId}
          </Text>
        )}
      </YStack>

      {data.synthetic ? (
        <Alert title="Synthetic timeline">
          Native engine event history coming in v1.42. Today this view derives
          events from the workflow record (start, signal count, terminal
          transition) — no events are fabricated.
        </Alert>
      ) : null}

      {events.length === 0 ? (
        <Empty title="No events yet" hint="Events will appear once the workflow runs." />
      ) : (
        <YStack gap="$3" pl="$4" borderLeftWidth={1} borderLeftColor="$borderColor">
          {events.map((ev) => (
            <YStack key={ev.eventId} position="relative">
              <YStack
                position="absolute"
                l={-29}
                t={14}
                width={12}
                height={12}
                rounded={9999}
                bg="$background"
                items="center"
                justify="center"
              >
                <Circle size={10} color="#f2f2f2" fill="#f2f2f2" />
              </YStack>
              <Card p="$4" bg="$background" borderColor="$borderColor" borderWidth={1}>
                <YStack gap="$2">
                  <XStack items="center" justify="space-between" gap="$3">
                    <XStack items="center" gap="$2">
                      <Badge variant="muted">#{ev.eventId}</Badge>
                      <Link
                        to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/events/${encodeURIComponent(ev.eventId)}${qs}`}
                        style={{ textDecoration: 'none' }}
                      >
                        <H4 fontSize="$3" fontWeight="500" color={'#86efac' as never}>
                          {ev.eventType}
                        </H4>
                      </Link>
                    </XStack>
                    <Text fontSize="$1" color="$placeholderColor">
                      {ev.eventTime ? formatTimestamp(new Date(ev.eventTime)) : '—'}
                    </Text>
                  </XStack>
                  {ev.attributes && Object.keys(ev.attributes).length > 0 && (
                    <YStack
                      bg={'rgba(255,255,255,0.02)' as never}
                      p="$2"
                      rounded="$2"
                      borderWidth={1}
                      borderColor="$borderColor"
                    >
                      <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$1" color="$placeholderColor">
                        {JSON.stringify(ev.attributes, null, 2)}
                      </Text>
                    </YStack>
                  )}
                </YStack>
              </Card>
            </YStack>
          ))}
        </YStack>
      )}
    </YStack>
  )
}
