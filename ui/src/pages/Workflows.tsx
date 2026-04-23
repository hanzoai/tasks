import useSWR from 'swr'
import { Link, useParams } from 'react-router-dom'
import type { WorkflowExecution } from '../lib/api'
import { StatusPill } from '../components/StatusPill'
import { ErrorState } from './Namespaces'

export function WorkflowsPage() {
  const { ns } = useParams()
  const url = `/api/v1/namespaces/${encodeURIComponent(ns!)}/workflows?query=${encodeURIComponent('')}&pageSize=50`
  const { data, error, isLoading } = useSWR<{ executions?: WorkflowExecution[] }>(url)
  if (error) return <ErrorState error={error} />
  if (isLoading) return <p className="text-zinc-400">Loading workflows…</p>
  const rows = data?.executions ?? []
  return (
    <section>
      <h2 className="text-lg font-medium mb-4">Workflows <span className="text-zinc-500 text-sm">({rows.length})</span></h2>
      {rows.length === 0 ? (
        <p className="text-zinc-400">No running or recent workflows in <code>{ns}</code>.</p>
      ) : (
        <table className="w-full text-sm border border-zinc-800 rounded-lg overflow-hidden">
          <thead className="bg-zinc-900/50 text-zinc-400 text-left">
            <tr>
              <th className="px-4 py-2 font-medium">Workflow ID</th>
              <th className="px-4 py-2 font-medium">Type</th>
              <th className="px-4 py-2 font-medium">Status</th>
              <th className="px-4 py-2 font-medium">Task queue</th>
              <th className="px-4 py-2 font-medium">Started</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-zinc-800">
            {rows.map((wf) => {
              const workflowId = wf.execution.workflowId
              return (
                <tr key={`${workflowId}-${wf.execution.runId}`} className="hover:bg-zinc-800/30">
                  <td className="px-4 py-2">
                    <Link
                      to={`/namespaces/${encodeURIComponent(ns!)}/workflows/${encodeURIComponent(workflowId)}?runId=${encodeURIComponent(wf.execution.runId)}`}
                      className="text-sky-400 hover:underline"
                    >
                      {workflowId}
                    </Link>
                  </td>
                  <td className="px-4 py-2 text-zinc-300">{wf.type.name}</td>
                  <td className="px-4 py-2"><StatusPill kind={statusKind(wf.status)}>{shortStatus(wf.status)}</StatusPill></td>
                  <td className="px-4 py-2 text-zinc-400">{wf.taskQueue ?? '—'}</td>
                  <td className="px-4 py-2 text-zinc-500">{wf.startTime ? new Date(wf.startTime).toLocaleString() : '—'}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}
    </section>
  )
}

function shortStatus(s: string) {
  return s.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()
}

function statusKind(s: string): 'ok' | 'warn' | 'error' | 'muted' {
  switch (s) {
    case 'WORKFLOW_EXECUTION_STATUS_COMPLETED':
      return 'ok'
    case 'WORKFLOW_EXECUTION_STATUS_RUNNING':
      return 'ok'
    case 'WORKFLOW_EXECUTION_STATUS_FAILED':
    case 'WORKFLOW_EXECUTION_STATUS_TIMED_OUT':
    case 'WORKFLOW_EXECUTION_STATUS_TERMINATED':
      return 'error'
    case 'WORKFLOW_EXECUTION_STATUS_CANCELED':
      return 'warn'
    default:
      return 'muted'
  }
}
