import useSWR from 'swr'
import { useParams, useSearchParams, Link } from 'react-router-dom'
import { ErrorState } from './Namespaces'
import { StatusPill } from '../components/StatusPill'

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
  const qs = new URLSearchParams({ 'execution.workflowId': workflowId!, 'execution.runId': runId }).toString()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/workflows/${encodeURIComponent(workflowId!)}?${qs}`
  const { data, error, isLoading } = useSWR<DescribeResp>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading || !data) return <p className="text-zinc-400">Loading workflow…</p>

  const info = data.workflowExecutionInfo
  return (
    <section>
      <div className="mb-4">
        <Link to={`/namespaces/${encodeURIComponent(ns!)}/workflows`} className="text-sm text-zinc-400 hover:text-zinc-200">
          ← workflows
        </Link>
      </div>
      <header className="mb-6">
        <h2 className="text-xl font-semibold">{info.execution.workflowId}</h2>
        <p className="text-zinc-400 text-sm mt-1 font-mono">{info.execution.runId}</p>
      </header>
      <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm">
        <Field label="Type">{info.type.name}</Field>
        <Field label="Status">
          <StatusPill kind="ok">{info.status.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()}</StatusPill>
        </Field>
        <Field label="Task queue">{info.taskQueue ?? data.executionConfig?.taskQueue?.name ?? '—'}</Field>
        <Field label="History events">{info.historyLength ?? '—'}</Field>
        <Field label="Started">{info.startTime ? new Date(info.startTime).toLocaleString() : '—'}</Field>
        <Field label="Closed">{info.closeTime ? new Date(info.closeTime).toLocaleString() : 'running'}</Field>
      </dl>
    </section>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-zinc-500">{label}</dt>
      <dd className="text-zinc-200">{children}</dd>
    </>
  )
}
