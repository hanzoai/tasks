import useSWR from 'swr'
import { Link } from 'react-router-dom'
import { ChevronRight } from 'lucide-react'
import type { Namespace } from '../lib/api'
import { Card } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Skeleton } from '../components/ui/skeleton'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'
import { useRealtime } from '../lib/events'

export function NamespacesPage() {
  useRealtime()
  const { data, error, isLoading } = useSWR<{ namespaces: Namespace[] }>(
    '/v1/tasks/namespaces?pageSize=200'
  )
  if (error) return <ErrorState error={error} />
  if (isLoading) return <NamespacesSkeleton />
  const nss = data?.namespaces ?? []
  if (nss.length === 0) return <Empty title="No namespaces" hint="Create one with the SDK or hanzo-tasks CLI." />

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Namespaces <span className="text-muted-foreground text-sm">({nss.length})</span>
        </h2>
      </header>
      <Card className="py-0 overflow-hidden divide-y divide-border">
        {nss.map((ns) => {
          const name = ns.namespaceInfo.name
          const ok = ns.namespaceInfo.state === 'NAMESPACE_STATE_REGISTERED'
          return (
            <Link
              key={name}
              to={`/namespaces/${encodeURIComponent(name)}`}
              className="flex items-center gap-4 px-5 py-3.5 hover:bg-accent/40 transition-colors"
            >
              <span className="font-medium">{name}</span>
              <Badge variant={ok ? 'success' : 'muted'}>{shortState(ns.namespaceInfo.state)}</Badge>
              {ns.namespaceInfo.description && (
                <span className="text-muted-foreground text-sm truncate">{ns.namespaceInfo.description}</span>
              )}
              <span className="ml-auto text-xs text-muted-foreground">
                retention {humanTTL(ns.config?.workflowExecutionRetentionTtl)}
              </span>
              <ChevronRight size={16} className="text-muted-foreground" />
            </Link>
          )
        })}
      </Card>
    </section>
  )
}

function NamespacesSkeleton() {
  return (
    <section className="space-y-4">
      <Skeleton className="h-7 w-48" />
      <Card className="py-0 divide-y divide-border">
        {[0, 1, 2].map((i) => (
          <div key={i} className="flex items-center gap-4 px-5 py-3.5">
            <Skeleton className="h-4 w-32" />
            <Skeleton className="h-5 w-20" />
            <Skeleton className="ml-auto h-3 w-24" />
          </div>
        ))}
      </Card>
    </section>
  )
}

function shortState(s: string) {
  return s.replace(/^NAMESPACE_STATE_/, '').toLowerCase()
}

function humanTTL(raw?: string) {
  if (!raw) return '—'
  const m = /^(\d+)([sm]|h|d)?$/.exec(raw) ?? /^(\d+)h(\d+)?m?(\d+)?s?$/.exec(raw)
  if (!m) return raw
  // Common case: "720h" → "30d", "604800s" → "7d"
  const num = Number(m[1])
  if (raw.endsWith('h')) {
    const days = Math.round(num / 24)
    return days >= 1 ? `${days}d` : `${num}h`
  }
  if (raw.endsWith('s')) {
    const days = Math.round(num / 86400)
    return days >= 1 ? `${days}d` : `${Math.round(num / 3600)}h`
  }
  return raw
}
