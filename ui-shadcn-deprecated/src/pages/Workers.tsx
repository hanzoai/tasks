import useSWR from 'swr'
import { useParams } from 'react-router-dom'
import { Card } from '../components/ui/card'
import { Skeleton } from '../components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '../components/ui/alert'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'
import { useRealtime } from '../lib/events'

interface WorkersResp {
  workers?: unknown[]
}

export function WorkersPage() {
  const { ns } = useParams()
  const namespace = ns!
  useRealtime(namespace)
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workers`
  const { data, error, isLoading } = useSWR<WorkersResp>(url)

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
  const rows = data?.workers ?? []

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Workers <span className="text-sm text-muted-foreground">({rows.length})</span>
        </h2>
      </header>

      <Alert>
        <AlertTitle>Worker registration not yet wired</AlertTitle>
        <AlertDescription>
          Worker registration lands with the worker SDK runtime
          (<code className="font-mono text-xs">pkg/sdk/worker</code>) — task #10
          follow-up. Until then, this surface stays empty by design rather than
          showing fake data.
        </AlertDescription>
      </Alert>

      {rows.length === 0 ? (
        <Empty
          title={`No workers polling ${namespace}`}
          hint="Use the Hanzo Tasks SDK at pkg/sdk/worker to register a worker. The next release lands worker heartbeats and live polling status here."
        />
      ) : null}
    </section>
  )
}
