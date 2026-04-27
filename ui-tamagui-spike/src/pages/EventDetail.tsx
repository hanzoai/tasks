// Event detail — JSON tree of a single event payload. Rides on the
// /history endpoint and picks the matching row, so no new opcode.

import { Link, useParams, useSearchParams } from 'react-router-dom'
import { Card, H1, Text, XStack, YStack } from 'hanzogui'
import { ChevronLeft } from '@hanzogui/lucide-icons-2'
import { Badge, Empty, ErrorState, LoadingState, formatTimestamp, useFetch } from '@hanzogui/admin'

interface HistoryEvent {
  eventId: string
  eventType: string
  eventTime?: string
  attributes?: Record<string, unknown>
}

interface HistoryResp {
  events?: HistoryEvent[]
}

export function EventDetailPage() {
  const { ns, workflowId, eventId } = useParams()
  const [sp] = useSearchParams()
  const runId = sp.get('runId') ?? ''
  const namespace = ns!
  const qs = runId ? `?runId=${encodeURIComponent(runId)}` : ''
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/history${qs}`
  const { data, error, isLoading } = useFetch<HistoryResp>(url)

  if (error) return <ErrorState error={error as Error} />
  if (isLoading || !data) return <LoadingState />

  const ev = data.events?.find((e) => e.eventId === eventId)

  return (
    <YStack gap="$5">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/history${qs}`}
        style={{ textDecoration: 'none', color: 'inherit' }}
      >
        <XStack items="center" gap="$1.5" hoverStyle={{ opacity: 0.8 }}>
          <ChevronLeft size={14} color="#7e8794" />
          <Text fontSize="$2" color="$placeholderColor">
            history
          </Text>
        </XStack>
      </Link>

      <YStack gap="$1">
        <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
          EVENT
        </Text>
        <H1 size="$7" color="$color" fontWeight="600">
          {ev ? ev.eventType : `Event ${eventId}`}
        </H1>
        <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$1" color="$placeholderColor">
          workflow {workflowId}
        </Text>
      </YStack>

      {!ev ? (
        <Empty
          title="Event not found"
          hint={`No event with id ${eventId} on this workflow run.`}
        />
      ) : (
        <Card p="$4" bg="$background" borderColor="$borderColor" borderWidth={1}>
          <YStack gap="$3">
            <Field label="Event ID">
              <Badge variant="muted">#{ev.eventId}</Badge>
            </Field>
            <Field label="Type">
              <Text fontSize="$2" color="$color">
                {ev.eventType}
              </Text>
            </Field>
            <Field label="Time">
              <Text fontSize="$2" color="$color">
                {ev.eventTime ? formatTimestamp(new Date(ev.eventTime)) : '—'}
              </Text>
            </Field>
            <YStack gap="$1.5" mt="$2">
              <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
                ATTRIBUTES
              </Text>
              <YStack
                bg={'rgba(255,255,255,0.02)' as never}
                p="$3"
                rounded="$2"
                borderWidth={1}
                borderColor="$borderColor"
              >
                <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$1" color="$color">
                  {JSON.stringify(ev.attributes ?? {}, null, 2)}
                </Text>
              </YStack>
            </YStack>
          </YStack>
        </Card>
      )}
    </YStack>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <XStack items="center" gap="$3">
      <Text width={120} fontSize="$2" color="$placeholderColor">
        {label}
      </Text>
      <YStack flex={1}>{children}</YStack>
    </XStack>
  )
}
