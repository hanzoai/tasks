// Workflows — list view for a namespace. Full-bleed (no PageShell).
//
// Header band:  "N Workflows" + refresh + last-fetched stamp + Start CTA.
// Filter band:  query input.
// Body:         table when populated, empty state when not.

import { useCallback, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import {
  Button,
  Card,
  Dialog,
  H1,
  Input,
  Spinner,
  Text,
  XStack,
  YStack,
} from 'hanzogui'
import {
  Play,
  Plus,
  RefreshCw,
} from '@hanzogui/lucide-icons-2'
import type { WorkflowExecution } from '../lib/api'
import { ApiError, apiPost } from '../lib/api'
import { useFetch } from '../lib/useFetch'
import { useTaskEvents } from '../lib/events'
import { Alert } from '../components/Alert'
import { Badge } from '../components/Badge'
import { Empty, ErrorState } from '../components/Empty'
import { formatTimestamp, shortStatus, statusVariant } from '../lib/format'

interface WorkflowsResp {
  executions?: WorkflowExecution[]
}

export function WorkflowsPage() {
  const { ns } = useParams()
  const namespace = ns!
  const [query, setQuery] = useState('')
  const [fetchedAt, setFetchedAt] = useState<Date>(new Date())

  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows?query=${encodeURIComponent(query)}&pageSize=50`
  const { data, error, isLoading, isValidating, mutate } = useFetch<WorkflowsResp>(url)

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
    <YStack flex={1} bg="$background" minH="100%">
      {/* Header band */}
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
          <Button
            size="$2"
            chromeless
            onPress={refresh}
            disabled={isValidating}
            aria-label="Refresh"
          >
            {isValidating ? (
              <Spinner size="small" />
            ) : (
              <RefreshCw size={14} color="#7e8794" />
            )}
          </Button>
          <Text fontSize="$1" color="$placeholderColor">
            {formatTimestamp(fetchedAt)}
          </Text>
        </XStack>
        <StartWorkflowButton ns={namespace} onStarted={refresh} />
      </XStack>

      {/* Filter band */}
      <XStack
        px="$6"
        py="$3"
        borderBottomWidth={1}
        borderBottomColor="$borderColor"
        gap="$2"
        items="center"
      >
        <Input
          size="$3"
          flex={1}
          maxW={520}
          placeholder='WorkflowType="MyWorkflow" OR WorkflowId STARTS_WITH "abc"'
          value={query}
          onChangeText={setQuery}
        />
        <Text fontSize="$1" color="$placeholderColor">
          List view
        </Text>
      </XStack>

      {/* Body */}
      <YStack flex={1} p="$6" gap="$4">
        {error ? (
          <ErrorState error={error as Error} />
        ) : isLoading ? (
          <YStack gap="$3">
            <YStack height={36} bg="$borderColor" rounded="$2" opacity={0.5} />
            <YStack height={120} bg="$borderColor" rounded="$2" opacity={0.3} />
          </YStack>
        ) : rows.length === 0 ? (
          <Empty
            title={`No workflows in ${namespace}`}
            hint="Start one with the button above, or run a worker that registers a workflow type."
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
              <HeaderCell flex={1.5}>Run ID</HeaderCell>
              <HeaderCell flex={2}>Type</HeaderCell>
              <HeaderCell flex={2}>Start</HeaderCell>
              <HeaderCell flex={2}>End</HeaderCell>
            </XStack>
            {rows.map((wf, i) => (
              <WorkflowRow
                key={`${wf.execution.workflowId}-${wf.execution.runId}`}
                wf={wf}
                ns={namespace}
                last={i === rows.length - 1}
              />
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
      <Text fontSize="$1" fontWeight="500" color="$placeholderColor">
        {children}
      </Text>
    </YStack>
  )
}

function WorkflowRow({
  wf,
  ns,
  last,
}: {
  wf: WorkflowExecution
  ns: string
  last: boolean
}) {
  const href = `/namespaces/${encodeURIComponent(ns)}/workflows/${encodeURIComponent(wf.execution.workflowId)}?runId=${encodeURIComponent(wf.execution.runId)}`
  return (
    <XStack
      px="$4"
      py="$2.5"
      borderBottomWidth={last ? 0 : 1}
      borderBottomColor="$borderColor"
      hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
      items="center"
    >
      <YStack flex={1.2} px="$2">
        <Badge variant={statusVariant(wf.status)}>{shortStatus(wf.status)}</Badge>
      </YStack>
      <YStack flex={3} px="$2">
        <Link to={href} style={{ textDecoration: 'none' }}>
          <Text fontSize="$2" color={'#86efac' as never} numberOfLines={1}>
            {wf.execution.workflowId}
          </Text>
        </Link>
      </YStack>
      <YStack flex={1.5} px="$2">
        <Text
          fontFamily={'ui-monospace, SFMono-Regular, monospace' as never}
          fontSize="$1"
          color="$placeholderColor"
        >
          {wf.execution.runId.slice(0, 8)}
        </Text>
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
      <YStack flex={2} px="$2">
        <Text fontSize="$2" color="$placeholderColor">
          {wf.closeTime ? formatTimestamp(new Date(wf.closeTime)) : '—'}
        </Text>
      </YStack>
    </XStack>
  )
}

function StartWorkflowButton({ ns, onStarted }: { ns: string; onStarted: () => void }) {
  const [open, setOpen] = useState(false)
  const [type, setType] = useState('')
  const [taskQueue, setTaskQueue] = useState('default')
  const [workflowId, setWorkflowId] = useState('')
  const [input, setInput] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  async function submit() {
    setSubmitting(true)
    setErr(null)
    try {
      let parsedInput: unknown = undefined
      if (input.trim()) {
        try {
          parsedInput = JSON.parse(input)
        } catch {
          throw new Error('Input must be valid JSON.')
        }
      }
      await apiPost(`/v1/tasks/namespaces/${encodeURIComponent(ns)}/workflows`, {
        workflowId: workflowId || undefined,
        workflowType: { name: type },
        taskQueue: { name: taskQueue },
        input: parsedInput,
      })
      setOpen(false)
      onStarted()
    } catch (e) {
      if (e instanceof ApiError) {
        setErr(`${e.status === 501 ? 'Not yet implemented in native server' : 'Failed'}: ${e.message}`)
      } else {
        setErr(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog modal open={open} onOpenChange={setOpen}>
      <Dialog.Trigger asChild>
        <Button size="$3" bg={'#f2f2f2' as never} hoverStyle={{ background: '#ffffff' as never }}>
          <XStack items="center" gap="$1.5">
            <Plus size={14} color="#070b13" />
            <Text fontSize="$2" fontWeight="500" color={'#070b13' as never}>
              Start Workflow
            </Text>
          </XStack>
        </Button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay key="overlay" bg={'rgba(0,0,0,0.6)' as never} />
        <Dialog.Content
          bg="$background"
          borderColor="$borderColor"
          borderWidth={1}
          minW={520}
          p="$5"
          gap="$4"
        >
          <Dialog.Title fontSize="$6" fontWeight="600" color="$color">
            Start a workflow in {ns}
          </Dialog.Title>
          <Dialog.Description fontSize="$2" color="$placeholderColor">
            Posts to /v1/tasks/namespaces/{ns}/workflows (opcode 0x0060).
          </Dialog.Description>
          <YStack gap="$3">
            <Field label="Workflow type *">
              <Input value={type} onChangeText={setType} placeholder="MyWorkflow" />
            </Field>
            <XStack gap="$3">
              <Field label="Task queue" flex={1}>
                <Input value={taskQueue} onChangeText={setTaskQueue} />
              </Field>
              <Field label="Workflow ID" flex={1}>
                <Input value={workflowId} onChangeText={setWorkflowId} placeholder="auto" />
              </Field>
            </XStack>
            <Field label="Input (JSON, optional)">
              <Input value={input} onChangeText={setInput} placeholder='{"key":"value"}' />
            </Field>
            {err && <Alert variant="destructive" title="Could not start">{err}</Alert>}
          </YStack>
          <XStack gap="$2" justify="flex-end" mt="$2">
            <Button chromeless onPress={() => setOpen(false)}>
              <Text fontSize="$2">Cancel</Text>
            </Button>
            <Button
              onPress={submit}
              disabled={submitting || !type}
              bg={'#f2f2f2' as never}
              hoverStyle={{ background: '#ffffff' as never }}
            >
              <XStack items="center" gap="$1.5">
                <Play size={14} color="#070b13" />
                <Text fontSize="$2" fontWeight="500" color={'#070b13' as never}>
                  {submitting ? 'Starting…' : 'Start'}
                </Text>
              </XStack>
            </Button>
          </XStack>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog>
  )
}

function Field({
  label,
  flex,
  children,
}: {
  label: string
  flex?: number
  children: React.ReactNode
}) {
  return (
    <YStack gap="$1.5" flex={flex}>
      <Text fontSize="$2" color="$color">
        {label}
      </Text>
      {children}
    </YStack>
  )
}

