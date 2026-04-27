import useSWR from 'swr'
import { useParams } from 'react-router-dom'
import type { Schedule } from '../lib/api'
import { Card } from '../components/ui/card'
import { Skeleton } from '../components/ui/skeleton'
import { ErrorState } from '../components/ErrorState'
import { Empty } from '../components/Empty'

interface ListResp {
  schedules?: Schedule[]
}

export function SchedulesPage() {
  const { ns } = useParams()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/schedules`
  const { data, error, isLoading } = useSWR<ListResp>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading) {
    return (
      <section className="space-y-4">
        <Skeleton className="h-7 w-48" />
        <Card className="py-6">
          <Skeleton className="h-5 w-3/4 mx-6" />
        </Card>
      </section>
    )
  }
  const rows = data?.schedules ?? []

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Schedules <span className="text-muted-foreground text-sm">({rows.length})</span>
        </h2>
      </header>

      {rows.length === 0 ? (
        <Empty
          title={`No schedules in ${ns}`}
          hint="Create one with the Hanzo Tasks SDK; the UI surface is read-only for now."
        />
      ) : (
        <Card className="py-0 overflow-hidden divide-y divide-border">
          {rows.map((s) => (
            <div key={s.scheduleId} className="px-5 py-3.5">
              <p className="font-medium">{s.scheduleId}</p>
              <p className="text-xs text-muted-foreground mt-1">{describeSpec(s)}</p>
            </div>
          ))}
        </Card>
      )}
    </section>
  )
}

function describeSpec(s: Schedule): string {
  const spec = s.spec
  if (!spec) return 'no spec'
  if (spec.cronString?.length) return `cron: ${spec.cronString.join(', ')}`
  if (spec.interval?.length) return `every ${spec.interval.map((i: { interval: string }) => i.interval).join(', ')}`
  return 'custom'
}
