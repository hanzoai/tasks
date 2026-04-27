import useSWR from 'swr'
import { Link, useParams } from 'react-router-dom'
import { Layers } from 'lucide-react'
import { Card } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Skeleton } from '../components/ui/skeleton'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'
import { useRealtime } from '../lib/events'
import { formatTimestamp } from '../components/LocalTimeIndicator'

// TaskQueues mirrors the upstream Temporal route at
//   /namespaces/[namespace]/task-queues
// Server aggregates queues from the workflow list so the UI is honest:
// every queue here has at least one workflow we know about.

interface TaskQueueRow {
  name: string
  workflows: number
  running: number
  latestStart?: string
}

export function TaskQueuesPage() {
  const { ns } = useParams()
  const namespace = ns!
  useRealtime(namespace)
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/task-queues`
  const { data, error, isLoading } = useSWR<{ taskQueues?: TaskQueueRow[] }>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading) {
    return (
      <section className="space-y-4">
        <Skeleton className="h-7 w-48" />
        <Card className="py-6">
          <Skeleton className="mx-6 h-5 w-3/4" />
        </Card>
      </section>
    )
  }
  const rows = data?.taskQueues ?? []

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Task queues <span className="text-sm text-muted-foreground">({rows.length})</span>
        </h2>
      </header>

      {rows.length === 0 ? (
        <Empty
          title={`No task queues in ${namespace}`}
          hint="A task queue is created the first time a workflow targets it. Start a workflow to see queues here."
        />
      ) : (
        <Card className="overflow-hidden py-0">
          <table className="w-full text-sm">
            <thead className="bg-muted/50 text-left text-muted-foreground">
              <tr>
                <th className="px-4 py-2.5 font-medium">Name</th>
                <th className="px-4 py-2.5 font-medium">Workflows</th>
                <th className="px-4 py-2.5 font-medium">Running</th>
                <th className="px-4 py-2.5 font-medium">Latest start</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {rows.map((q) => (
                <tr key={q.name} className="hover:bg-accent/30">
                  <td className="px-4 py-2.5">
                    <Link
                      to={`/namespaces/${encodeURIComponent(namespace)}/task-queues/${encodeURIComponent(q.name)}`}
                      className="inline-flex items-center gap-2 text-primary hover:underline"
                    >
                      <Layers size={14} />
                      {q.name}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5">{q.workflows}</td>
                  <td className="px-4 py-2.5">
                    {q.running > 0 ? (
                      <Badge variant="success">{q.running}</Badge>
                    ) : (
                      <span className="text-muted-foreground">0</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-muted-foreground">
                    {q.latestStart ? formatTimestamp(new Date(q.latestStart)) : '—'}
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
