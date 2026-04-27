import useSWR from 'swr'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { ChevronLeft } from 'lucide-react'
import { Card, CardContent } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Skeleton } from '../components/ui/skeleton'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'
import { useRealtime } from '../lib/events'
import { formatTimestamp } from '../components/LocalTimeIndicator'

interface HistoryEvent {
  eventId: string
  eventType: string
  eventTime?: string
  attributes?: Record<string, unknown>
}

interface HistoryResp {
  events?: HistoryEvent[]
  synthetic?: boolean
}

export function EventDetailPage() {
  const { ns, workflowId, eventId } = useParams()
  const [sp] = useSearchParams()
  const runId = sp.get('runId') ?? ''
  const namespace = ns!
  useRealtime(namespace)
  const qs = runId ? `?runId=${encodeURIComponent(runId)}` : ''
  // Event detail rides on the same /history endpoint and picks the row.
  // No new opcode — keeps the wire surface honest.
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/history${qs}`
  const { data, error, isLoading } = useSWR<HistoryResp>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading || !data) {
    return (
      <section className="space-y-4">
        <Skeleton className="h-4 w-24" />
        <Skeleton className="h-7 w-64" />
        <Card><CardContent><Skeleton className="h-32" /></CardContent></Card>
      </section>
    )
  }

  const ev = data.events?.find((e) => e.eventId === eventId)

  return (
    <section className="space-y-6">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/history${qs}`}
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ChevronLeft size={14} /> history
      </Link>
      <header className="space-y-1">
        <p className="text-xs uppercase tracking-wider text-muted-foreground">Event</p>
        <h2 className="text-xl font-semibold">
          {ev ? ev.eventType : `Event ${eventId}`}
        </h2>
        <p className="font-mono text-xs text-muted-foreground">workflow {workflowId}</p>
      </header>

      {!ev ? (
        <Empty title="Event not found" hint={`No event with id ${eventId} on this workflow run.`} />
      ) : (
        <Card>
          <CardContent>
            <dl className="grid grid-cols-2 gap-x-6 gap-y-3 text-sm">
              <Field label="Event ID">
                <Badge variant="muted">#{ev.eventId}</Badge>
              </Field>
              <Field label="Type">{ev.eventType}</Field>
              <Field label="Time">
                {ev.eventTime ? formatTimestamp(new Date(ev.eventTime)) : '—'}
              </Field>
            </dl>
            <div className="mt-5 space-y-2">
              <p className="text-xs uppercase tracking-wider text-muted-foreground">Attributes</p>
              <pre className="overflow-auto rounded border border-border bg-muted/30 p-3 text-xs">
                {JSON.stringify(ev.attributes ?? {}, null, 2)}
              </pre>
            </div>
          </CardContent>
        </Card>
      )}
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
