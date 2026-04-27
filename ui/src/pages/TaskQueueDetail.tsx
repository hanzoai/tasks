// Task queue detail — workflows targeting one queue + summary cards
// for total / running / closed.

import { useCallback } from 'react'
import { Link, useParams } from 'react-router-dom'
import { Card, H1, H2, H3, Text, XStack, YStack } from 'hanzogui'
import { ChevronLeft } from '@hanzogui/lucide-icons-2'
import type { WorkflowExecution } from '../lib/api'
import { useFetch } from '../lib/useFetch'
import { useTaskEvents } from '../lib/events'
import { Alert } from '../components/Alert'
import { Badge } from '../components/Badge'
import { Empty, ErrorState, LoadingState } from '../components/Empty'
import { formatTimestamp, shortStatus, statusVariant } from '../lib/format'

interface DetailResp {
  name: string
  workflows: WorkflowExecution[]
  running: number
  total: number
}

export function TaskQueueDetailPage() {
  const { ns, queue } = useParams()
  const namespace = ns!
  const queueName = queue!
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/task-queues/${encodeURIComponent(queueName)}`
  const { data, error, isLoading, mutate } = useFetch<DetailResp>(url)

  const onEvent = useCallback(() => {
    void mutate()
  }, [mutate])

  useTaskEvents(namespace, onEvent, [
    'workflow.started',
    'workflow.canceled',
    'workflow.terminated',
  ])

  if (error) return <ErrorState error={error as Error} />
  if (isLoading || !data) return <LoadingState />

  const rows = data.workflows ?? []

  return (
    <YStack gap="$5">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/task-queues`}
        style={{ textDecoration: 'none', color: 'inherit' }}
      >
        <XStack items="center" gap="$1.5" hoverStyle={{ opacity: 0.8 }}>
          <ChevronLeft size={14} color="#7e8794" />
          <Text fontSize="$2" color="$placeholderColor">
            task queues
          </Text>
        </XStack>
      </Link>

      <YStack gap="$1">
        <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
          TASK QUEUE
        </Text>
        <H1 size="$7" color="$color" fontWeight="600">
          {data.name}
        </H1>
      </YStack>

      <XStack gap="$3" flexWrap="wrap">
        <SummaryCard label="Total workflows" value={data.total} />
        <SummaryCard label="Running" value={data.running} accent="success" />
        <SummaryCard
          label="Closed"
          value={Math.max(0, data.total - data.running)}
          accent="muted"
        />
      </XStack>

      <Alert title="Workers">
        Worker registration lands with the worker SDK runtime (pkg/sdk/worker). Until
        then, this view derives queue stats from listed workflows.
      </Alert>

      <XStack items="baseline" justify="space-between">
        <H3 size="$5" color="$color" fontWeight="500">
          Workflows on this queue{' '}
          <Text fontSize="$2" color="$placeholderColor" fontWeight="400">
            ({rows.length})
          </Text>
        </H3>
      </XStack>

      {rows.length === 0 ? (
        <Empty
          title={`No workflows targeting ${data.name}`}
          hint="Workflows are bucketed by their taskQueue field on start."
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
            <HeaderCell flex={1.2}>Status</HeaderCell>
            <HeaderCell flex={3}>Workflow ID</HeaderCell>
            <HeaderCell flex={2}>Type</HeaderCell>
            <HeaderCell flex={2}>Start</HeaderCell>
          </XStack>
          {rows.map((wf, i) => (
            <XStack
              key={`${wf.execution.workflowId}-${wf.execution.runId}`}
              px="$4"
              py="$2.5"
              borderBottomWidth={i === rows.length - 1 ? 0 : 1}
              borderBottomColor="$borderColor"
              hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
              items="center"
            >
              <YStack flex={1.2} px="$2">
                <Badge variant={statusVariant(wf.status)}>{shortStatus(wf.status)}</Badge>
              </YStack>
              <YStack flex={3} px="$2">
                <Link
                  to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(wf.execution.workflowId)}?runId=${encodeURIComponent(wf.execution.runId)}`}
                  style={{ textDecoration: 'none' }}
                >
                  <Text fontSize="$2" color={'#86efac' as never} numberOfLines={1}>
                    {wf.execution.workflowId}
                  </Text>
                </Link>
              </YStack>
              <YStack flex={2} px="$2">
                <Text fontSize="$2" color="$color">
                  {wf.type.name}
                </Text>
              </YStack>
              <YStack flex={2} px="$2">
                <Text fontSize="$2" color="$placeholderColor">
                  {wf.startTime ? formatTimestamp(new Date(wf.startTime)) : '—'}
                </Text>
              </YStack>
            </XStack>
          ))}
        </Card>
      )}
    </YStack>
  )
}

function SummaryCard({
  label,
  value,
  accent,
}: {
  label: string
  value: number
  accent?: 'success' | 'muted'
}) {
  const color =
    accent === 'success' ? '#86efac' : accent === 'muted' ? '#7e8794' : '#f2f2f2'
  return (
    <Card
      p="$4"
      bg="$background"
      borderColor="$borderColor"
      borderWidth={1}
      flexBasis={200}
      flexGrow={1}
    >
      <YStack gap="$1">
        <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
          {label.toUpperCase()}
        </Text>
        <H2 size="$8" fontWeight="600" color={color as never}>
          {value}
        </H2>
      </YStack>
    </Card>
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
