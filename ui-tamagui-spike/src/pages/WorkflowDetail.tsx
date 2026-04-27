// Workflow detail — Summary | Call stack | History tabs.
//
// Summary: type, status, task queue, history events, started, closed.
// Call stack: queries the worker via __stack_trace; shows 501 banner
//             if engine doesn't yet implement that opcode.
// History:   pointer to /history page; the actual timeline lives there.

import { useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import {
  Button,
  Card,
  H1,
  Spinner,
  Tabs,
  Text,
  XStack,
  YStack,
} from 'hanzogui'
import {
  ChevronLeft,
  History,
  RefreshCw,
} from '@hanzogui/lucide-icons-2'
import { Alert, Badge, ErrorState, LoadingState, useFetch } from '@hanzogui/admin'
import { ApiError, apiPost, shortStatus, statusVariant } from '../lib/api'
import { useTaskEvents } from '../lib/events'

interface DescribeResp {
  workflowExecutionInfo: {
    execution: { workflowId: string; runId: string }
    type: { name: string }
    startTime?: string
    closeTime?: string
    status: string
    historyLength?: string
    taskQueue?: string
  }
  executionConfig?: {
    taskQueue?: { name: string }
    workflowRunTimeout?: string
    workflowTaskTimeout?: string
  }
}

export function WorkflowDetailPage() {
  const { ns, workflowId } = useParams()
  const [sp] = useSearchParams()
  const runId = sp.get('runId') ?? ''
  const namespace = ns!
  const qs = new URLSearchParams({
    'execution.workflowId': workflowId!,
    'execution.runId': runId,
  }).toString()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}?${qs}`
  const { data, error, isLoading, mutate } = useFetch<DescribeResp>(url)

  useTaskEvents(namespace, () => void mutate(), [
    'workflow.canceled',
    'workflow.terminated',
    'workflow.signaled',
  ])

  if (error) return <ErrorState error={error as Error} />
  if (isLoading || !data) return <LoadingState />

  const info = data.workflowExecutionInfo
  const runQs = runId ? `?runId=${encodeURIComponent(runId)}` : ''

  return (
    <YStack gap="$5">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/workflows`}
        style={{ textDecoration: 'none', color: 'inherit' }}
      >
        <XStack items="center" gap="$1.5" hoverStyle={{ opacity: 0.8 }}>
          <ChevronLeft size={14} color="#7e8794" />
          <Text fontSize="$2" color="$placeholderColor">
            workflows
          </Text>
        </XStack>
      </Link>

      <XStack items="flex-start" justify="space-between" gap="$3">
        <YStack gap="$1" flex={1}>
          <H1 size="$7" color="$color" fontWeight="600">
            {info.execution.workflowId}
          </H1>
          <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$2" color="$placeholderColor">
            {info.execution.runId}
          </Text>
        </YStack>
        <Link
          to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(info.execution.workflowId)}/history${runQs}`}
          style={{ textDecoration: 'none' }}
        >
          <Button size="$2" borderWidth={1} borderColor="$borderColor">
            <XStack items="center" gap="$1.5">
              <History size={14} />
              <Text fontSize="$2">Full history</Text>
            </XStack>
          </Button>
        </Link>
      </XStack>

      <Tabs defaultValue="summary" orientation="horizontal" flexDirection="column">
        <Tabs.List
          borderBottomWidth={1}
          borderBottomColor="$borderColor"
          gap="$2"
          self="flex-start"
        >
          <TabTrigger value="summary">Summary</TabTrigger>
          <TabTrigger value="call-stack">Call stack</TabTrigger>
          <TabTrigger value="history">History</TabTrigger>
        </Tabs.List>

        <Tabs.Content value="summary" mt="$4">
          <Card p="$4" bg="$background" borderColor="$borderColor" borderWidth={1}>
            <YStack gap="$3">
              <Field label="Type">{info.type.name}</Field>
              <Field label="Status">
                <Badge variant={statusVariant(info.status)}>{shortStatus(info.status)}</Badge>
              </Field>
              <Field label="Task queue">
                {info.taskQueue ? (
                  <Link
                    to={`/namespaces/${encodeURIComponent(namespace)}/task-queues/${encodeURIComponent(info.taskQueue)}`}
                    style={{ textDecoration: 'none' }}
                  >
                    <Text fontSize="$2" color={'#86efac' as never}>
                      {info.taskQueue}
                    </Text>
                  </Link>
                ) : (
                  <Text fontSize="$2" color="$color">
                    {data.executionConfig?.taskQueue?.name ?? '—'}
                  </Text>
                )}
              </Field>
              <Field label="History events">
                <Text fontSize="$2" color="$color">
                  {info.historyLength ?? '—'}
                </Text>
              </Field>
              <Field label="Started">
                <Text fontSize="$2" color="$color">
                  {info.startTime ? new Date(info.startTime).toLocaleString() : '—'}
                </Text>
              </Field>
              <Field label="Closed">
                <Text fontSize="$2" color="$color">
                  {info.closeTime ? new Date(info.closeTime).toLocaleString() : 'running'}
                </Text>
              </Field>
            </YStack>
          </Card>
        </Tabs.Content>

        <Tabs.Content value="call-stack" mt="$4">
          <CallStackPane
            ns={namespace}
            workflowId={info.execution.workflowId}
            runId={info.execution.runId}
            running={info.status === 'WORKFLOW_EXECUTION_STATUS_RUNNING'}
          />
        </Tabs.Content>

        <Tabs.Content value="history" mt="$4">
          <Card p="$4" bg="$background" borderColor="$borderColor" borderWidth={1}>
            <YStack gap="$3">
              <Text fontSize="$2" color="$placeholderColor">
                The full event timeline lives on its own page so it can paginate
                large histories without crowding the summary.
              </Text>
              <Link
                to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(info.execution.workflowId)}/history${runQs}`}
                style={{ textDecoration: 'none' }}
              >
                <XStack items="center" gap="$1.5">
                  <History size={14} color="#86efac" />
                  <Text fontSize="$2" color={'#86efac' as never}>
                    Open full history
                  </Text>
                </XStack>
              </Link>
            </YStack>
          </Card>
        </Tabs.Content>
      </Tabs>
    </YStack>
  )
}

function TabTrigger({ value, children }: { value: string; children: React.ReactNode }) {
  return (
    <Tabs.Tab value={value} px="$3" py="$2" unstyled bg="transparent">
      <Text fontSize="$2" color="$color">
        {children}
      </Text>
    </Tabs.Tab>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <XStack items="center" gap="$3">
      <Text width={140} fontSize="$2" color="$placeholderColor">
        {label}
      </Text>
      <YStack flex={1}>{children}</YStack>
    </XStack>
  )
}

function CallStackPane({
  ns,
  workflowId,
  runId,
  running,
}: {
  ns: string
  workflowId: string
  runId: string
  running: boolean
}) {
  const [stack, setStack] = useState<string | null>(null)
  const [err, setErr] = useState<{ status: number; message: string } | null>(null)
  const [loading, setLoading] = useState(false)

  async function load() {
    setLoading(true)
    setErr(null)
    setStack(null)
    try {
      const resp = await apiPost<{ stack?: string }>(
        `/v1/tasks/namespaces/${encodeURIComponent(ns)}/workflows/${encodeURIComponent(workflowId)}/query?runId=${encodeURIComponent(runId)}`,
        { queryType: '__stack_trace' },
      )
      setStack(resp?.stack ?? '')
    } catch (e) {
      if (e instanceof ApiError) {
        setErr({ status: e.status, message: e.message })
      } else {
        setErr({ status: 0, message: e instanceof Error ? e.message : String(e) })
      }
    } finally {
      setLoading(false)
    }
  }

  if (!running) {
    return (
      <Alert title="Stack trace requires a running workflow">
        This workflow is not running, so there is no live stack trace to query.
        Re-open while the workflow is in progress to capture a snapshot.
      </Alert>
    )
  }

  return (
    <YStack gap="$3">
      <XStack items="center" justify="space-between">
        <Text fontSize="$2" color="$placeholderColor">
          Calls QueryWorkflow(__stack_trace) on the worker.
        </Text>
        <Button size="$2" chromeless onPress={load} disabled={loading}>
          <XStack items="center" gap="$1.5">
            {loading ? <Spinner size="small" /> : <RefreshCw size={14} color="#7e8794" />}
            <Text fontSize="$2">{loading ? 'Querying…' : 'Capture stack'}</Text>
          </XStack>
        </Button>
      </XStack>

      {err ? (
        err.status === 501 ? (
          <Alert title="Worker SDK runtime not yet shipped">
            Stack-trace queries land when the worker SDK runtime ships. Until then
            the engine returns 501 — that's the honest answer rather than a
            fabricated frame.
          </Alert>
        ) : (
          <Alert variant="destructive" title="Query failed">
            {err.message}
          </Alert>
        )
      ) : stack !== null ? (
        stack ? (
          <Card p="$3" bg="$background" borderColor="$borderColor" borderWidth={1}>
            <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$1" color="$color">
              {stack}
            </Text>
          </Card>
        ) : (
          <Alert title="Empty stack">
            The worker returned an empty stack — the workflow may be parked
            between activities.
          </Alert>
        )
      ) : (
        <Card p="$4" bg="$background" borderColor="$borderColor" borderWidth={1}>
          <Text fontSize="$2" color="$placeholderColor">
            Click Capture stack to query the worker.
          </Text>
        </Card>
      )}
    </YStack>
  )
}
