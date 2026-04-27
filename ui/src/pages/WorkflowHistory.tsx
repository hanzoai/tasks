import useSWR from 'swr'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { ChevronLeft, Circle } from 'lucide-react'
import { Card } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Skeleton } from '../components/ui/skeleton'
import { Alert, AlertDescription, AlertTitle } from '../components/ui/alert'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'
import { useRealtime } from '../lib/events'
import { formatTimestamp } from '../components/LocalTimeIndicator'

// WorkflowHistory mirrors the upstream
//   /namespaces/[ns]/workflows/[wf]/[run]/history route.
// The native engine doesn't yet emit per-event durable history, so the
// server returns a synthetic timeline: start event, signal counter, and
// (if closed) the terminal transition. We label the response as
// synthetic so this page can show the right hint.

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

export function WorkflowHistoryPage() {
  const { ns, workflowId } = useParams()
  const [sp] = useSearchParams()
  const runId = sp.get('runId') ?? ''
  const namespace = ns!
  useRealtime(namespace)
  const qs = runId ? `?runId=${encodeURIComponent(runId)}` : ''
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/history${qs}`
  const { data, error, isLoading } = useSWR<HistoryResp>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading || !data) {
    return (
      <section className="space-y-4">
        <Skeleton className="h-4 w-24" />
        <Skeleton className="h-7 w-72" />
        <Card className="py-6"><Skeleton className="mx-6 h-32" /></Card>
      </section>
    )
  }

  const events = data.events ?? []

  return (
    <section className="space-y-6">
      <Link
        to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}${qs}`}
        className="inline-flex items-center gap-1 text-sm text-muted-foreground hover:text-foreground"
      >
        <ChevronLeft size={14} /> {workflowId}
      </Link>
      <header className="space-y-1">
        <p className="text-xs uppercase tracking-wider text-muted-foreground">History</p>
        <h2 className="text-xl font-semibold">{workflowId}</h2>
        {runId && <p className="font-mono text-sm text-muted-foreground">{runId}</p>}
      </header>

      {data.synthetic ? (
        <Alert>
          <AlertTitle>Synthetic timeline</AlertTitle>
          <AlertDescription>
            Native engine event history coming in v1.42. Today this view derives
            events from the workflow record (start, signal count, terminal
            transition) — no events are fabricated.
          </AlertDescription>
        </Alert>
      ) : null}

      {events.length === 0 ? (
        <Empty title="No events yet" hint="Events will appear once the workflow runs." />
      ) : (
        <ol className="relative space-y-3 border-l border-border pl-6">
          {events.map((ev) => (
            <li key={ev.eventId} className="relative">
              <span className="absolute -left-[27px] top-2 inline-flex size-3 items-center justify-center rounded-full bg-background text-muted-foreground">
                <Circle size={10} className="fill-foreground stroke-none" />
              </span>
              <Card className="py-4">
                <div className="space-y-2 px-5">
                  <div className="flex items-center justify-between gap-3">
                    <div className="flex items-center gap-2">
                      <Badge variant="muted">#{ev.eventId}</Badge>
                      <Link
                        to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(workflowId!)}/events/${encodeURIComponent(ev.eventId)}${qs}`}
                        className="text-sm font-medium text-primary hover:underline"
                      >
                        {ev.eventType}
                      </Link>
                    </div>
                    <span className="text-xs text-muted-foreground">
                      {ev.eventTime ? formatTimestamp(new Date(ev.eventTime)) : '—'}
                    </span>
                  </div>
                  {ev.attributes && Object.keys(ev.attributes).length > 0 && (
                    <pre className="overflow-auto rounded bg-muted/30 p-2 text-xs text-muted-foreground">
                      {JSON.stringify(ev.attributes, null, 2)}
                    </pre>
                  )}
                </div>
              </Card>
            </li>
          ))}
        </ol>
      )}
    </section>
  )
}
