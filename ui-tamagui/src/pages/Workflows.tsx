// Tamagui port of v1 ui/src/pages/Workflows.tsx.
//
// Layout intent (matches v1):
//   • Header row: "N Workflows" + refresh button + last-fetched stamp on the
//     left, "Start Workflow" CTA on the right
//   • Body: table of executions (status pill, workflow id, short run id,
//     type, start time, end time)
//   • Empty state: simple text fallback (the moon-and-sea hero is v1.42 polish)
//
// Uses Tamagui's v5 shorthand props throughout (`p`, `px`, `py`, `bg`,
// `items`, `justify`, `self`, `rounded`) — the published v102 prop
// types reject the long-form names through their `Omit<...>` chain
// even though the runtime accepts both. Short forms typecheck.
//
// Notes:
//   • `flex` keeps its long form; there's no `f` in v5 shorthands.
//   • Hex/rgba colors are passed via plain string casts to bypass the
//     ColorTokens generic — Tamagui accepts arbitrary CSS color values
//     at runtime; the type system is just stricter than the runtime.
//
// The v1 page uses SavedViewsRail + FilterBar and a Dialog for "Start
// Workflow". This spike only proves the table renders. The remaining
// affordances are listed in README.md.

import { useCallback, useState } from 'react'
import { Button, Card, H1, Paragraph, Spinner, Text, XStack, YStack } from 'hanzogui'
import type { WorkflowExecution, WorkflowsResponse } from '../lib/api'
import { useFetch } from '../lib/useFetch'
import { useTaskEvents } from '../lib/events'

export interface WorkflowsPageProps {
  namespace: string
}

export function WorkflowsPage({ namespace }: WorkflowsPageProps) {
  const [query] = useState('')
  const [fetchedAt, setFetchedAt] = useState<Date>(new Date())

  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows?query=${encodeURIComponent(query)}&pageSize=50`
  const { data, error, isLoading, isValidating, mutate } = useFetch<WorkflowsResponse>(url)

  // Refresh the table on any workflow lifecycle event for this namespace.
  // Stamp the "fetched at" once data lands.
  const onEvent = useCallback(() => {
    mutate().then(() => setFetchedAt(new Date()))
  }, [mutate])

  useTaskEvents(namespace, onEvent, [
    'workflow.started',
    'workflow.canceled',
    'workflow.terminated',
    'workflow.signaled',
  ])

  const rows = data?.executions ?? []
  const count = rows.length

  const refresh = useCallback(() => {
    mutate().then(() => setFetchedAt(new Date()))
  }, [mutate])

  return (
    <YStack flex={1} bg="$background" height="100vh">
      {/* Header */}
      <XStack
        px="$6"
        py="$5"
        borderBottomWidth={1}
        borderBottomColor="$borderColor"
        justify="space-between"
        items="center"
      >
        <XStack items="baseline" gap="$3">
          <H1 size="$9" fontWeight="600" color="$color">
            {count} Workflow{count === 1 ? '' : 's'}
          </H1>
          <Button size="$2" chromeless onPress={refresh} disabled={isValidating} aria-label="Refresh">
            {isValidating ? <Spinner size="small" /> : <Text>↻</Text>}
          </Button>
          <Text fontSize="$1" color="$placeholderColor">
            {formatTimestamp(fetchedAt)}
          </Text>
        </XStack>
        <Button size="$3">+ Start Workflow</Button>
      </XStack>

      {/* Body */}
      <YStack flex={1} p="$6" gap="$4">
        {error ? (
          <Card p="$4" bg="$background" borderColor="$borderColor">
            <Paragraph color="#f87171">Failed to load workflows: {error.message}</Paragraph>
          </Card>
        ) : isLoading ? (
          <YStack gap="$3">
            <YStack height={36} bg="$borderColor" rounded="$2" opacity={0.5} />
            <YStack height={120} bg="$borderColor" rounded="$2" opacity={0.3} />
          </YStack>
        ) : rows.length === 0 ? (
          <Card p="$6" bg="$background" borderColor="$borderColor">
            <YStack gap="$2" items="center">
              <H1 size="$6">No workflows yet</H1>
              <Paragraph color="$placeholderColor">
                Start one with the button above, or run a worker that registers a workflow type.
              </Paragraph>
            </YStack>
          </Card>
        ) : (
          <Card overflow="hidden" bg="$background" borderColor="$borderColor">
            {/* Header row */}
            <XStack bg="$borderColor" opacity={0.6} px="$4" py="$2">
              <HeaderCell flex={1.2}>Status</HeaderCell>
              <HeaderCell flex={3}>Workflow ID</HeaderCell>
              <HeaderCell flex={1.5}>Run ID</HeaderCell>
              <HeaderCell flex={2}>Type</HeaderCell>
              <HeaderCell flex={2}>Start</HeaderCell>
              <HeaderCell flex={2}>End</HeaderCell>
            </XStack>
            {/* Data rows */}
            {rows.map((wf, i) => (
              <WorkflowRow key={`${wf.execution.workflowId}-${wf.execution.runId}`} wf={wf} odd={i % 2 === 1} />
            ))}
          </Card>
        )}
      </YStack>
    </YStack>
  )
}

function HeaderCell({ children, flex }: { children: React.ReactNode; flex: number }) {
  return (
    <YStack flex={flex} px="$2">
      <Text fontSize="$2" fontWeight="500" color="$placeholderColor">
        {children}
      </Text>
    </YStack>
  )
}

function WorkflowRow({ wf, odd }: { wf: WorkflowExecution; odd: boolean }) {
  return (
    <XStack
      px="$4"
      py="$2"
      borderTopWidth={1}
      borderTopColor="$borderColor"
      bg={odd ? ('rgba(255,255,255,0.015)' as never) : ('transparent' as never)}
      hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
      items="center"
    >
      <YStack flex={1.2} px="$2">
        <StatusBadge status={wf.status} />
      </YStack>
      <YStack flex={3} px="$2">
        <Text color="$color" fontSize="$2" numberOfLines={1}>
          {wf.execution.workflowId}
        </Text>
      </YStack>
      <YStack flex={1.5} px="$2">
        <Text fontSize="$1" color="$placeholderColor">
          {wf.execution.runId.slice(0, 8)}
        </Text>
      </YStack>
      <YStack flex={2} px="$2">
        <Text color="$color" fontSize="$2">
          {wf.type.name}
        </Text>
      </YStack>
      <YStack flex={2} px="$2">
        <Text color="$placeholderColor" fontSize="$2">
          {wf.startTime ? formatTimestamp(new Date(wf.startTime)) : '—'}
        </Text>
      </YStack>
      <YStack flex={2} px="$2">
        <Text color="$placeholderColor" fontSize="$2">
          {wf.closeTime ? formatTimestamp(new Date(wf.closeTime)) : '—'}
        </Text>
      </YStack>
    </XStack>
  )
}

function StatusBadge({ status }: { status: string }) {
  const short = status.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()
  const c = badgeColor(status)
  return (
    <XStack px="$2" py="$1" rounded="$2" bg={c.bg as never} self="flex-start">
      <Text fontSize="$1" color={c.fg as never} fontWeight="500">
        {short}
      </Text>
    </XStack>
  )
}

function badgeColor(status: string): { bg: string; fg: string } {
  switch (status) {
    case 'WORKFLOW_EXECUTION_STATUS_RUNNING':
    case 'WORKFLOW_EXECUTION_STATUS_COMPLETED':
      return { bg: 'rgba(34,197,94,0.15)', fg: '#86efac' }
    case 'WORKFLOW_EXECUTION_STATUS_FAILED':
    case 'WORKFLOW_EXECUTION_STATUS_TIMED_OUT':
    case 'WORKFLOW_EXECUTION_STATUS_TERMINATED':
      return { bg: 'rgba(239,68,68,0.15)', fg: '#fca5a5' }
    case 'WORKFLOW_EXECUTION_STATUS_CANCELED':
      return { bg: 'rgba(234,179,8,0.15)', fg: '#fde68a' }
    default:
      return { bg: 'rgba(148,163,184,0.15)', fg: '#cbd5e1' }
  }
}

function formatTimestamp(d: Date): string {
  // Match v1's LocalTimeIndicator format roughly: "HH:MM:SS · Apr 27".
  const time = d.toLocaleTimeString(undefined, { hour12: false })
  const day = d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
  return `${time} · ${day}`
}
