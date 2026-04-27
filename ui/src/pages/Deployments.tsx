import useSWR from 'swr'
import { useParams } from 'react-router-dom'
import type { Deployment } from '../lib/api'
import { Card } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Skeleton } from '../components/ui/skeleton'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'

export function DeploymentsPage() {
  const { ns } = useParams()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/deployments`
  const { data, error, isLoading } = useSWR<{ deployments: Deployment[] }>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading) return <Skeleton className="h-40" />
  const rows = data?.deployments ?? []

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Deployments <span className="text-muted-foreground text-sm">({rows.length})</span>
        </h2>
      </header>

      {rows.length === 0 ? (
        <Empty
          title={`No worker deployments in ${ns}`}
          hint="Workers register a series + buildId on connect. Routing rules promote a default and ramp new versions."
        />
      ) : (
        <div className="grid gap-4 md:grid-cols-2">
          {rows.map((d) => (
            <Card key={d.seriesName} className="space-y-3 p-6">
              <div>
                <p className="text-sm text-muted-foreground">Series</p>
                <p className="font-medium">{d.seriesName}</p>
              </div>
              <div>
                <p className="text-sm text-muted-foreground">Default build</p>
                <p className="font-mono text-sm">{d.defaultBuildId || '—'}</p>
              </div>
              <div>
                <p className="text-sm text-muted-foreground mb-1">Versions</p>
                <ul className="space-y-1">
                  {d.buildIds.map((b) => (
                    <li key={b.buildId} className="flex items-center gap-2 text-sm">
                      <code className="font-mono">{b.buildId}</code>
                      <Badge
                        variant={b.state === 'DEPLOYMENT_STATE_CURRENT' ? 'success' : 'muted'}
                      >
                        {b.state.replace('DEPLOYMENT_STATE_', '').toLowerCase()}
                      </Badge>
                    </li>
                  ))}
                </ul>
              </div>
            </Card>
          ))}
        </div>
      )}
    </section>
  )
}
