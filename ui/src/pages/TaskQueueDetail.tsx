import useSWR from 'swr'
import { Link, useParams } from 'react-router-dom'
import { ChevronLeft } from 'lucide-react'
import type { WorkflowExecution } from '../lib/api'
import { Card, CardContent } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Skeleton } from '../components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '../components/ui/alert'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'
import { useRealtime } from '../lib/events'
import { formatTimestamp } from '../components/LocalTimeIndicator'

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
  useRealtime(namespace)
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/task-queues/${encodeURIComponent(queueName)}`
  const { data, error, isLoading } = useSWR<DetailResp>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading || !data) {
    return (
      <section className="space-y-4">
        <Skeleton className="h-4 w-24" />
        <Skeleton className="h-7 w-72" />
        <Card><CardContent><Skeleton className="h-32" /></CardContent></Card>
      </section>
    )
  }

  const rows = data.workflows ?? []

  return (
    <section className="space-y-6">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/task-queues`}
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ChevronLeft size={14} /> task queues
      </Link>
      <header className="space-y-1">
        <p className="text-xs uppercase tracking-wider text-muted-foreground">Task queue</p>
        <h2 className="text-xl font-semibold">{data.name}</h2>
      </header>

      <div className="grid grid-cols-2 gap-4 md:grid-cols-3">
        <SummaryCard label="Total workflows" value={data.total} />
        <SummaryCard label="Running" value={data.running} accent="success" />
        <SummaryCard label="Closed" value={Math.max(0, data.total - data.running)} accent="muted" />
      </div>

      <Alert>
        <AlertTitle>Workers</AlertTitle>
        <AlertDescription>
          Worker registration lands with the worker SDK runtime
          (<code className="font-mono text-xs">pkg/sdk/worker</code>). Until then,
          this view derives queue stats from listed workflows.
        </AlertDescription>
      </Alert>

      <header className="flex items-center justify-between">
        <h3 className="text-base font-medium">
          Workflows on this queue <span className="text-sm text-muted-foreground">({rows.length})</span>
        </h3>
      </header>

      {rows.length === 0 ? (
        <Empty
          title={`No workflows targeting ${data.name}`}
          hint="Workflows are bucketed by their taskQueue field on start."
        />
      ) : (
        <Card className="overflow-hidden py-0">
          <table className="w-full text-sm">
            <thead className="bg-muted/50 text-left text-muted-foreground">
              <tr>
                <th className="px-4 py-2.5 font-medium">Status</th>
                <th className="px-4 py-2.5 font-medium">Workflow ID</th>
                <th className="px-4 py-2.5 font-medium">Type</th>
                <th className="px-4 py-2.5 font-medium">Start</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {rows.map((wf) => (
                <tr
                  key={`${wf.execution.workflowId}-${wf.execution.runId}`}
                  className="hover:bg-accent/30"
                >
                  <td className="px-4 py-2.5">
                    <Badge variant={statusVariant(wf.status)}>{shortStatus(wf.status)}</Badge>
                  </td>
                  <td className="px-4 py-2.5">
                    <Link
                      to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(wf.execution.workflowId)}?runId=${encodeURIComponent(wf.execution.runId)}`}
                      className="text-primary hover:underline"
                    >
                      {wf.execution.workflowId}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5">{wf.type.name}</td>
                  <td className="px-4 py-2.5 text-muted-foreground">
                    {wf.startTime ? formatTimestamp(new Date(wf.startTime)) : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </section>
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
  return (
    <Card className="py-4">
      <CardContent className="space-y-1">
        <p className="text-xs uppercase tracking-wider text-muted-foreground">{label}</p>
        <p
          className={
            accent === 'success'
              ? 'text-2xl font-semibold text-emerald-400 dark:text-emerald-300'
              : accent === 'muted'
              ? 'text-2xl font-semibold text-muted-foreground'
              : 'text-2xl font-semibold'
          }
        >
          {value}
        </p>
      </CardContent>
    </Card>
  )
}

function shortStatus(s: string) {
  return s.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()
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
