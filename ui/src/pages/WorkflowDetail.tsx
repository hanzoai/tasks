import { useState } from 'react'
import useSWR from 'swr'
import { useParams, useSearchParams, Link } from 'react-router-dom'
import { ChevronLeft, History, RefreshCw } from 'lucide-react'
import { apiPost, ApiError } from '../lib/api'
import { Card, CardContent } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '../components/ui/tabs'
import { Alert, AlertDescription, AlertTitle } from '../components/ui/alert'
import { ErrorState } from '../components/ErrorState'
import { useRealtime } from '../lib/events'

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
  useRealtime(namespace)
  const qs = new URLSearchParams({
    'execution.workflowId': workflowId!,
    'execution.runId': runId,
  }).toString()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}?${qs}`
  const { data, error, isLoading } = useSWR<DescribeResp>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading || !data) {
    return (
      <section className="space-y-4">
        <Skeleton className="h-4 w-24" />
        <Skeleton className="h-7 w-64" />
        <Skeleton className="h-4 w-96" />
        <Card><CardContent><Skeleton className="h-32" /></CardContent></Card>
      </section>
    )
  }

  const info = data.workflowExecutionInfo
  const runQs = runId ? `?runId=${encodeURIComponent(runId)}` : ''

  return (
    <section className="space-y-6">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/workflows`}
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ChevronLeft size={14} /> workflows
      </Link>
      <header className="flex items-start justify-between gap-4">
        <div className="space-y-1">
          <h2 className="text-xl font-semibold">{info.execution.workflowId}</h2>
          <p className="font-mono text-sm text-muted-foreground">{info.execution.runId}</p>
        </div>
        <Link
          to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(info.execution.workflowId)}/history${runQs}`}
          className="inline-flex items-center gap-1.5 rounded-md border border-border px-3 py-1.5 text-sm text-muted-foreground hover:bg-accent hover:text-foreground"
        >
          <History size={14} /> Full history
        </Link>
      </header>

      <Tabs defaultValue="summary">
        <TabsList>
          <TabsTrigger value="summary">Summary</TabsTrigger>
          <TabsTrigger value="call-stack">Call stack</TabsTrigger>
          <TabsTrigger value="history">History</TabsTrigger>
        </TabsList>

        <TabsContent value="summary">
          <Card>
            <CardContent>
              <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm">
                <Field label="Type">{info.type.name}</Field>
                <Field label="Status">
                  <Badge variant={statusVariant(info.status)}>
                    {info.status.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()}
                  </Badge>
                </Field>
                <Field label="Task queue">
                  {info.taskQueue ? (
                    <Link
                      to={`/namespaces/${encodeURIComponent(namespace)}/task-queues/${encodeURIComponent(info.taskQueue)}`}
                      className="text-primary hover:underline"
                    >
                      {info.taskQueue}
                    </Link>
                  ) : (
                    data.executionConfig?.taskQueue?.name ?? '—'
                  )}
                </Field>
                <Field label="History events">{info.historyLength ?? '—'}</Field>
                <Field label="Started">
                  {info.startTime ? new Date(info.startTime).toLocaleString() : '—'}
                </Field>
                <Field label="Closed">
                  {info.closeTime ? new Date(info.closeTime).toLocaleString() : 'running'}
                </Field>
              </dl>
            </CardContent>
          </Card>
        </TabsContent>

        <TabsContent value="call-stack">
          <CallStackPane
            ns={namespace}
            workflowId={info.execution.workflowId}
            runId={info.execution.runId}
            running={info.status === 'WORKFLOW_EXECUTION_STATUS_RUNNING'}
          />
        </TabsContent>

        <TabsContent value="history">
          <HistoryPreview ns={namespace} workflowId={info.execution.workflowId} runId={info.execution.runId} />
        </TabsContent>
      </Tabs>
    </section>
  )
}

interface CallStackPaneProps {
  ns: string
  workflowId: string
  runId: string
  running: boolean
}

function CallStackPane({ ns, workflowId, runId, running }: CallStackPaneProps) {
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
        { queryType: '__stack_trace' }
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
      <Alert>
        <AlertTitle>Stack trace requires a running workflow</AlertTitle>
        <AlertDescription>
          This workflow is not running, so there is no live stack trace to query.
          Re-open while the workflow is in progress to capture a snapshot.
        </AlertDescription>
      </Alert>
    )
  }

  return (
    <div className="space-y-3">
      <div className="flex items-center justify-between gap-3">
        <p className="text-sm text-muted-foreground">
          Calls <code className="font-mono text-xs">QueryWorkflow(__stack_trace)</code> on the
          worker.
        </p>
        <Button size="sm" variant="ghost" onClick={load} disabled={loading}>
          <RefreshCw size={14} className={loading ? 'animate-spin' : ''} />
          {loading ? 'Querying…' : 'Capture stack'}
        </Button>
      </div>

      {err ? (
        err.status === 501 ? (
          <Alert>
            <AlertTitle>Worker SDK runtime not yet shipped</AlertTitle>
            <AlertDescription>
              Stack-trace queries land when the worker SDK runtime ships. Until
              then the engine returns 501 — that's the honest answer rather than
              a fabricated frame.
            </AlertDescription>
          </Alert>
        ) : (
          <Alert variant="destructive">
            <AlertTitle>Query failed</AlertTitle>
            <AlertDescription>{err.message}</AlertDescription>
          </Alert>
        )
      ) : stack !== null ? (
        stack ? (
          <Card>
            <CardContent>
              <pre className="overflow-auto rounded bg-muted/30 p-3 text-xs">{stack}</pre>
            </CardContent>
          </Card>
        ) : (
          <Alert>
            <AlertTitle>Empty stack</AlertTitle>
            <AlertDescription>
              The worker returned an empty stack — the workflow may be parked
              between activities.
            </AlertDescription>
          </Alert>
        )
      ) : (
        <Card>
          <CardContent className="text-sm text-muted-foreground">
            Click <em>Capture stack</em> to query the worker.
          </CardContent>
        </Card>
      )}
    </div>
  )
}

interface HistoryPreviewProps {
  ns: string
  workflowId: string
  runId: string
}

function HistoryPreview({ ns, workflowId, runId }: HistoryPreviewProps) {
  const qs = runId ? `?runId=${encodeURIComponent(runId)}` : ''
  return (
    <Card>
      <CardContent className="space-y-3 text-sm">
        <p className="text-muted-foreground">
          The full event timeline lives on its own page so it can paginate
          large histories without crowding the summary.
        </p>
        <Link
          to={`/namespaces/${encodeURIComponent(ns)}/workflows/${encodeURIComponent(workflowId)}/history${qs}`}
          className="inline-flex items-center gap-1.5 text-primary hover:underline"
        >
          <History size={14} /> Open full history
        </Link>
      </CardContent>
    </Card>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </>
  )
}

function statusVariant(
  s: string
): 'success' | 'destructive' | 'warning' | 'muted' | 'default' {
  switch (s) {
    case 'WORKFLOW_EXECUTION_STATUS_RUNNING':
    case 'WORKFLOW_EXECUTION_STATUS_COMPLETED':
      return 'success'
    case 'WORKFLOW_EXECUTION_STATUS_FAILED':
    case 'WORKFLOW_EXECUTION_STATUS_TIMED_OUT':
    case 'WORKFLOW_EXECUTION_STATUS_TERMINATED':
      return 'destructive'
    case 'WORKFLOW_EXECUTION_STATUS_CANCELED':
      return 'warning'
    default:
      return 'muted'
  }
}
