import { useState } from 'react'
import useSWR from 'swr'
import { Link, useParams } from 'react-router-dom'
import { ChevronLeft, Copy, Check, ChevronDown, Plug, Globe } from 'lucide-react'
import type { Namespace, Identity } from '../lib/api'
import { Card, CardContent } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Skeleton } from '../components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '../components/ui/tabs'
import { Popover } from '../components/ui/dropdown-menu'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'

export function NamespaceDetailPage() {
  const { ns } = useParams()
  const { data, error, isLoading } = useSWR<Namespace>(`/v1/tasks/namespaces/${encodeURIComponent(ns!)}`)

  if (error) return <ErrorState error={error} />
  if (isLoading || !data) return <DetailSkeleton />

  const { namespaceInfo, config } = data
  const active = namespaceInfo.state === 'NAMESPACE_STATE_REGISTERED'

  return (
    <section className="space-y-6">
      <Link
        to="/namespaces"
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ChevronLeft size={14} /> Back to Namespaces <span className="px-2 text-border">|</span>
        <Link to={`/namespaces/${encodeURIComponent(ns!)}/workflows`} className="hover:text-foreground">
          Go to Workflows
        </Link>
      </Link>

      <header className="flex items-start gap-3">
        <div className="space-y-1">
          <div className="flex items-center gap-3">
            <h1 className="text-2xl font-semibold tracking-tight">{namespaceInfo.name}</h1>
            <Badge variant={active ? 'success' : 'muted'}>{active ? 'Active' : 'Inactive'}</Badge>
          </div>
          {namespaceInfo.description && (
            <p className="text-sm text-muted-foreground">{namespaceInfo.description}</p>
          )}
        </div>
        <div className="ml-auto">
          <ConnectDropdown ns={namespaceInfo.name} />
        </div>
      </header>

      <Tabs defaultValue="overview">
        <TabsList>
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="identities">Identities</TabsTrigger>
        </TabsList>

        <TabsContent value="overview" className="mt-4">
          <div className="grid grid-cols-2 sm:grid-cols-3 gap-4">
            <StatCard label="Region">
              <div className="flex items-center gap-2">
                <Globe size={14} className="text-muted-foreground" />
                <span>{namespaceInfo.region || 'embedded'}</span>
              </div>
            </StatCard>
            <StatCard label="Retention Policy" hint={config.workflowExecutionRetentionTtl}>
              {humanTTL(config.workflowExecutionRetentionTtl)}
            </StatCard>
            <StatCard label="APS Limit" hint="actions per second">
              {config.apsLimit.toLocaleString()}
            </StatCard>
          </div>
        </TabsContent>

        <TabsContent value="identities" className="mt-4">
          <IdentitiesPanel ns={ns!} />
        </TabsContent>
      </Tabs>
    </section>
  )
}

function ConnectDropdown({ ns }: { ns: string }) {
  const host = typeof window === 'undefined' ? 'tasks.local' : window.location.hostname
  const zap = `${host}:9999`
  const http = `${window.location.protocol}//${window.location.host}/v1/tasks/namespaces/${encodeURIComponent(ns)}`

  return (
    <Popover
      align="end"
      className="w-[28rem] p-3"
      trigger={
        <Button variant="outline" size="sm">
          <Plug />
          Connect
          <ChevronDown size={14} />
        </Button>
      }
    >
      <div className="space-y-3">
        <div className="space-y-1">
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            ZAP Endpoint
          </p>
          <CopyField value={zap} />
        </div>
        <div className="space-y-1">
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            HTTP Endpoint
          </p>
          <CopyField value={http} />
        </div>
        <p className="text-xs text-muted-foreground">
          ZAP is the canonical native binary transport. HTTP/JSON is browser-only.
        </p>
      </div>
    </Popover>
  )
}

function CopyField({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  const onCopy = () => {
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1200)
    })
  }
  return (
    <div className="flex items-center gap-1 rounded-md border border-border bg-muted/40 px-2 py-1">
      <code className="flex-1 truncate text-sm font-mono">{value}</code>
      <Button variant="ghost" size="icon" onClick={onCopy} aria-label="Copy">
        {copied ? <Check size={14} className="text-emerald-400" /> : <Copy size={14} />}
      </Button>
    </div>
  )
}

function StatCard({
  label,
  children,
  hint,
}: {
  label: string
  children: React.ReactNode
  hint?: string
}) {
  return (
    <Card>
      <CardContent className="space-y-1">
        <p className="text-xs uppercase tracking-wide text-muted-foreground">{label}</p>
        <div className="text-lg font-medium">{children}</div>
        {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
      </CardContent>
    </Card>
  )
}

function IdentitiesPanel({ ns }: { ns: string }) {
  const { data, error, isLoading } = useSWR<{ identities: Identity[] }>(
    `/v1/tasks/namespaces/${encodeURIComponent(ns)}/identities`
  )
  if (error) return <ErrorState error={error} />
  if (isLoading) return <Skeleton className="h-32" />
  const rows = data?.identities ?? []
  if (rows.length === 0) {
    return (
      <Empty
        title="No identities granted"
        hint="Identities are sourced from hanzo.id IAM. Grant access via the Hanzo Tasks SDK or POST /identities."
      />
    )
  }
  return (
    <Card className="py-0 overflow-hidden">
      <table className="w-full text-sm">
        <thead className="bg-muted/40 text-muted-foreground text-left">
          <tr>
            <th className="px-4 py-2.5 font-medium">Email</th>
            <th className="px-4 py-2.5 font-medium">Role</th>
            <th className="px-4 py-2.5 font-medium">Granted</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {rows.map((id) => (
            <tr key={id.email}>
              <td className="px-4 py-2.5">{id.email}</td>
              <td className="px-4 py-2.5">
                <Badge variant="muted">{id.role}</Badge>
              </td>
              <td className="px-4 py-2.5 text-muted-foreground">
                {new Date(id.grantTime).toLocaleString()}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </Card>
  )
}

function DetailSkeleton() {
  return (
    <section className="space-y-6">
      <Skeleton className="h-4 w-48" />
      <Skeleton className="h-9 w-72" />
      <div className="grid grid-cols-3 gap-4">
        <Skeleton className="h-24" />
        <Skeleton className="h-24" />
        <Skeleton className="h-24" />
      </div>
    </section>
  )
}

function humanTTL(raw: string) {
  if (!raw) return '—'
  if (raw.endsWith('h')) {
    const h = parseInt(raw, 10)
    const days = Math.round(h / 24)
    return days >= 1 ? `${days} days` : `${h}h`
  }
  if (raw.endsWith('s')) {
    const s = parseInt(raw, 10)
    const days = Math.round(s / 86400)
    return days >= 1 ? `${days} days` : `${Math.round(s / 3600)}h`
  }
  return raw
}
