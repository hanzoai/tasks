import { useEffect, useState } from 'react'
import { Clock } from 'lucide-react'

export function UTCClock() {
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    const t = setInterval(() => setNow(new Date()), 1000)
    return () => clearInterval(t)
  }, [])
  const hh = String(now.getUTCHours()).padStart(2, '0')
  const mm = String(now.getUTCMinutes()).padStart(2, '0')
  const ss = String(now.getUTCSeconds()).padStart(2, '0')
  return (
    <span
      title={now.toISOString()}
      className="inline-flex items-center gap-1.5 rounded-md border border-border bg-card/50 px-2 py-1 text-xs text-muted-foreground"
    >
      <Clock size={12} />
      UTC {hh}:{mm}:{ss}
    </span>
  )
}
