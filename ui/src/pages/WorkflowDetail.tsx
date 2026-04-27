import useSWR from 'swr'
import { useParams, useSearchParams, Link } from 'react-router-dom'
import { ChevronLeft } from 'lucide-react'
import { Card, CardContent } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Skeleton } from '../components/ui/skeleton'
import { ErrorState } from '../components/ErrorState'

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
  const qs = new URLSearchParams({
    'execution.workflowId': workflowId!,
    'execution.runId': runId,
  }).toString()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/workflows/${encodeURIComponent(workflowId!)}?${qs}`
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
  return (
    <section className="space-y-6">
      <Link
        to={`/namespaces/${encodeURIComponent(ns!)}/workflows`}
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ChevronLeft size={14} /> workflows
      </Link>
      <header className="space-y-1">
        <h2 className="text-xl font-semibold">{info.execution.workflowId}</h2>
        <p className="text-muted-foreground text-sm font-mono">{info.execution.runId}</p>
      </header>
      <Card>
        <CardContent>
          <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm">
            <Field label="Type">{info.type.name}</Field>
            <Field label="Status">
              <Badge variant="success">
                {info.status.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()}
              </Badge>
            </Field>
            <Field label="Task queue">{info.taskQueue ?? data.executionConfig?.taskQueue?.name ?? '—'}</Field>
            <Field label="History events">{info.historyLength ?? '—'}</Field>
            <Field label="Started">{info.startTime ? new Date(info.startTime).toLocaleString() : '—'}</Field>
            <Field label="Closed">{info.closeTime ? new Date(info.closeTime).toLocaleString() : 'running'}</Field>
          </dl>
        </CardContent>
      </Card>
    </section>
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
