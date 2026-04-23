import useSWR from 'swr'
import { Link } from 'react-router-dom'
import type { Namespace } from '../lib/api'
import { StatusPill } from '../components/StatusPill'

export function NamespacesPage() {
  const { data, error, isLoading } = useSWR<{ namespaces: Namespace[] }>('/api/v1/namespaces?pageSize=200')
  if (error) return <ErrorState error={error} />
  if (isLoading) return <p className="text-zinc-400">Loading namespaces…</p>
  const nss = data?.namespaces ?? []
  if (nss.length === 0)
    return <Empty title="No namespaces" hint="Create one with `tctl namespace register`" />
  return (
    <section>
      <h2 className="text-lg font-medium mb-4">Namespaces <span className="text-zinc-500 text-sm">({nss.length})</span></h2>
      <div className="border border-zinc-800 rounded-lg divide-y divide-zinc-800 bg-zinc-900/50">
        {nss.map((ns) => {
          const name = ns.namespaceInfo.name
          return (
            <Link
              key={name}
              to={`/namespaces/${encodeURIComponent(name)}/workflows`}
              className="flex items-center gap-4 px-4 py-3 hover:bg-zinc-800/50 transition"
            >
              <span className="font-medium">{name}</span>
              <StatusPill kind={ns.namespaceInfo.state === 'NAMESPACE_STATE_REGISTERED' ? 'ok' : 'muted'}>
                {shortState(ns.namespaceInfo.state)}
              </StatusPill>
              {ns.namespaceInfo.description && (
                <span className="text-zinc-400 text-sm truncate">{ns.namespaceInfo.description}</span>
              )}
              <span className="ml-auto text-xs text-zinc-500">
                retention {humanTTL(ns.config?.workflowExecutionRetentionTtl)}
              </span>
            </Link>
          )
        })}
      </div>
    </section>
  )
}

function shortState(s: string) {
  return s.replace(/^NAMESPACE_STATE_/, '').toLowerCase()
}

function humanTTL(raw?: string) {
  if (!raw) return '—'
  // Server returns "604800s" style. Drop trailing s for display.
  const m = /^(\d+)s$/.exec(raw)
  if (!m) return raw
  const s = Number(m[1])
  const days = Math.round(s / 86400)
  return days >= 1 ? `${days}d` : `${Math.round(s / 3600)}h`
}

export function ErrorState({ error }: { error: unknown }) {
  const msg = error instanceof Error ? error.message : String(error)
  return (
    <div className="border border-red-900/50 bg-red-950/30 rounded-lg p-4">
      <p className="text-red-300 font-medium">Failed to load</p>
      <p className="text-red-400/80 text-sm mt-1">{msg}</p>
    </div>
  )
}

function Empty({ title, hint }: { title: string; hint?: string }) {
  return (
    <div className="border border-zinc-800 border-dashed rounded-lg p-10 text-center">
      <p className="text-zinc-300">{title}</p>
      {hint && <p className="text-zinc-500 text-sm mt-1">{hint}</p>}
    </div>
  )
}
