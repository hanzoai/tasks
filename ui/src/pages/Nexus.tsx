import useSWR from 'swr'
import { useParams } from 'react-router-dom'
import { Network } from 'lucide-react'
import type { NexusEndpoint } from '../lib/api'
import { Card } from '../components/ui/card'
import { Skeleton } from '../components/ui/skeleton'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'

export function NexusPage() {
  const { ns } = useParams()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/nexus`
  const { data, error, isLoading } = useSWR<{ endpoints: NexusEndpoint[] }>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading) return <Skeleton className="h-40" />
  const rows = data?.endpoints ?? []

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Nexus <span className="text-muted-foreground text-sm">({rows.length})</span>
        </h2>
      </header>

      {rows.length === 0 ? (
        <Empty
          title={`No Nexus endpoints in ${ns}`}
          hint="Cross-namespace operation bridges. A workflow in this namespace calls a handler in another."
        />
      ) : (
        <Card className="py-0 overflow-hidden divide-y divide-border">
          {rows.map((e) => (
            <div key={e.name} className="flex items-center gap-3 px-5 py-3.5">
              <Network size={16} className="text-muted-foreground" />
              <div className="flex-1 min-w-0">
                <p className="font-medium">{e.name}</p>
                {e.description && (
                  <p className="text-xs text-muted-foreground truncate">{e.description}</p>
                )}
              </div>
              <code className="text-xs text-muted-foreground">{e.target}</code>
            </div>
          ))}
        </Card>
      )}
    </section>
  )
}
