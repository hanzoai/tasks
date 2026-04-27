import { useEffect, useMemo, useState } from 'react'
import { Clock } from 'lucide-react'
import { Popover, PopoverItem } from './ui/dropdown-menu'

// LocalTimeIndicator mimics Temporal's clock toggle in the top bar.
// Stores the choice in localStorage so it persists across reloads.
// Two modes: "local" (Intl.DateTimeFormat default) and "UTC".
// Other surfaces (timestamps in tables) read from localStorage too.

const KEY = 'tasks.tz'

export type Tz = 'local' | 'utc'

export function getTz(): Tz {
  if (typeof window === 'undefined') return 'local'
  const stored = localStorage.getItem(KEY)
  return stored === 'utc' ? 'utc' : 'local'
}

export function LocalTimeIndicator() {
  const [tz, setTz] = useState<Tz>(getTz())
  const [now, setNow] = useState(() => new Date())

  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 1000)
    return () => clearInterval(t)
  }, [])

  useEffect(() => {
    localStorage.setItem(KEY, tz)
    // Notify any timestamp consumers in the page; we don't have a
    // shared store yet so this just dispatches a custom event.
    window.dispatchEvent(new CustomEvent('tasks:tz-changed', { detail: tz }))
  }, [tz])

  const label = useMemo(() => {
    if (tz === 'utc') {
      const hh = String(now.getUTCHours()).padStart(2, '0')
      const mm = String(now.getUTCMinutes()).padStart(2, '0')
      return `UTC ${hh}:${mm}`
    }
    return `local`
  }, [tz, now])

  return (
    <Popover
      align="end"
      trigger={
        <button
          type="button"
          className="inline-flex items-center gap-1.5 rounded-md border border-border bg-card/40 px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground"
          aria-label="Time zone"
        >
          <Clock size={12} />
          <span>{label}</span>
        </button>
      }
    >
      <div className="px-2 py-1.5 text-xs font-medium text-muted-foreground">Time zone</div>
      <div className="my-1 border-t border-border" />
      <PopoverItem onClick={() => setTz('local')}>
        <span className={tz === 'local' ? 'font-medium' : ''}>Local</span>
      </PopoverItem>
      <PopoverItem onClick={() => setTz('utc')}>
        <span className={tz === 'utc' ? 'font-medium' : ''}>UTC</span>
      </PopoverItem>
    </Popover>
  )
}

export function formatTimestamp(d: Date): string {
  const tz = getTz()
  if (tz === 'utc') {
    return d.toISOString().replace('T', ' ').slice(0, 23) + ' UTC'
  }
  // local — match the upstream "26 Apr 2026, 22:33:39.64 GMT-7" format
  const day = d.getDate().toString().padStart(2, '0')
  const month = d.toLocaleString('en-US', { month: 'short' })
  const year = d.getFullYear()
  const time = d.toLocaleTimeString('en-US', {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
  const ms = String(d.getMilliseconds()).padStart(3, '0').slice(0, 2)
  const tzOffset = -d.getTimezoneOffset() / 60
  const tzLabel = `GMT${tzOffset >= 0 ? '+' : ''}${tzOffset}`
  return `${day} ${month} ${year}, ${time}.${ms} ${tzLabel}`
}
