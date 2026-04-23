import useSWR from 'swr'
import { useParams } from 'react-router-dom'
import type { Schedule } from '../lib/api'
import { ErrorState } from './Namespaces'

interface ListResp {
  schedules?: Schedule[]
}

export function SchedulesPage() {
  const { ns } = useParams()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/schedules`
  const { data, error, isLoading } = useSWR<ListResp>(url)
  if (error) return <ErrorState error={error} />
  if (isLoading) return <p className="text-zinc-400">Loading schedules…</p>
  const rows = data?.schedules ?? []
  return (
    <section>
      <h2 className="text-lg font-medium mb-4">Schedules <span className="text-zinc-500 text-sm">({rows.length})</span></h2>
      {rows.length === 0 ? (
        <p className="text-zinc-400">No schedules defined in <code>{ns}</code>.</p>
      ) : (
        <div className="border border-zinc-800 rounded-lg divide-y divide-zinc-800 bg-zinc-900/50">
          {rows.map((s) => (
            <div key={s.scheduleId} className="px-4 py-3">
              <p className="font-medium">{s.scheduleId}</p>
              <p className="text-xs text-zinc-500 mt-1">{describeSpec(s)}</p>
            </div>
          ))}
        </div>
      )}
    </section>
  )
}

function describeSpec(s: Schedule): string {
  const spec = s.schedule?.spec
  if (!spec) return 'no spec'
  if (spec.cronString && spec.cronString.length) return `cron: ${spec.cronString.join(', ')}`
  if (spec.interval && spec.interval.length) return `every ${spec.interval.map((i) => i.interval).join(', ')}`
  if (spec.calendar && spec.calendar.length) return 'calendar'
  return 'custom'
}
