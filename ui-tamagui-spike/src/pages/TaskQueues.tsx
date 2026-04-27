// Task queues — list of queues with running/total counts. Server
// aggregates queues from the workflow list so every row maps to at
// least one workflow we know about.

import { useCallback } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Card, H2, Text, XStack, YStack } from 'hanzogui'
import { Layers } from '@hanzogui/lucide-icons-2'
import { useFetch } from '../lib/useFetch'
import { useTaskEvents } from '../lib/events'
import { Badge } from '../components/Badge'
import { Empty, ErrorState, LoadingState } from '../components/Empty'
import { formatTimestamp } from '../lib/format'

interface TaskQueueRow {
  name: string
  workflows: number
  running: number
  latestStart?: string
}

export function TaskQueuesPage() {
  const { ns } = useParams()
  const namespace = ns!
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/task-queues`
  const { data, error, isLoading, mutate } = useFetch<{ taskQueues?: TaskQueueRow[] }>(url)

  const onEvent = useCallback(() => {
    void mutate()
  }, [mutate])

  useTaskEvents(namespace, onEvent, [
    'workflow.started',
    'workflow.canceled',
    'workflow.terminated',
  ])

  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState />
  const rows = data?.taskQueues ?? []

  return (
    <YStack gap="$4">
      <XStack items="baseline" justify="space-between">
        <H2 size="$7" color="$color">
          Task queues{' '}
          <Text fontSize="$3" color="$placeholderColor" fontWeight="400">
            ({rows.length})
          </Text>
        </H2>
      </XStack>

      {rows.length === 0 ? (
        <Empty
          title={`No task queues in ${namespace}`}
          hint="A task queue is created the first time a workflow targets it. Start a workflow to see queues here."
        />
      ) : (
        <Card overflow="hidden" bg="$background" borderColor="$borderColor" borderWidth={1}>
          <XStack
            bg={'rgba(255,255,255,0.03)' as never}
            px="$4"
            py="$2.5"
            borderBottomWidth={1}
            borderBottomColor="$borderColor"
          >
            <HeaderCell flex={3}>Name</HeaderCell>
            <HeaderCell flex={1}>Workflows</HeaderCell>
            <HeaderCell flex={1}>Running</HeaderCell>
            <HeaderCell flex={2}>Latest start</HeaderCell>
          </XStack>
          {rows.map((q, i) => (
            <XStack
              key={q.name}
              px="$4"
              py="$2.5"
              borderBottomWidth={i === rows.length - 1 ? 0 : 1}
              borderBottomColor="$borderColor"
              hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
              items="center"
            >
              <YStack flex={3} px="$2">
                <Link
                  to={`/namespaces/${encodeURIComponent(namespace)}/task-queues/${encodeURIComponent(q.name)}`}
                  style={{ textDecoration: 'none' }}
                >
                  <XStack items="center" gap="$2">
                    <Layers size={14} color="#86efac" />
                    <Text fontSize="$2" color={'#86efac' as never}>
                      {q.name}
                    </Text>
                  </XStack>
                </Link>
              </YStack>
              <YStack flex={1} px="$2">
                <Text fontSize="$2" color="$color">
                  {q.workflows}
                </Text>
              </YStack>
              <YStack flex={1} px="$2">
                {q.running > 0 ? (
                  <Badge variant="success">{q.running}</Badge>
                ) : (
                  <Text fontSize="$2" color="$placeholderColor">
                    0
                  </Text>
                )}
              </YStack>
              <YStack flex={2} px="$2">
                <Text fontSize="$2" color="$placeholderColor">
                  {q.latestStart ? formatTimestamp(new Date(q.latestStart)) : '—'}
                </Text>
              </YStack>
            </XStack>
          ))}
        </Card>
      )}
    </YStack>
  )
}

function HeaderCell({ children, flex }: { children: React.ReactNode; flex: number }) {
  return (
    <YStack flex={flex} px="$2">
      <Text fontSize="$1" fontWeight="500" color="$placeholderColor">
        {children}
      </Text>
    </YStack>
  )
}
