import { Eye, EyeOff } from 'lucide-react'
import { useEffect, useState } from 'react'

// ArchiveToggle is the eye icon in Temporal's top bar that switches
// between live and archived (closed) executions. Stored in
// localStorage and surfaced via a window event so the workflows page
// can re-fetch with the right query.

const KEY = 'tasks.showArchived'

export function getShowArchived(): boolean {
  if (typeof window === 'undefined') return false
  return localStorage.getItem(KEY) === '1'
}

export function ArchiveToggle() {
  const [on, setOn] = useState(getShowArchived())

  useEffect(() => {
    localStorage.setItem(KEY, on ? '1' : '0')
    window.dispatchEvent(new CustomEvent('tasks:archive-toggle', { detail: on }))
  }, [on])

  return (
    <button
      type="button"
      onClick={() => setOn((o) => !o)}
      className="inline-flex items-center justify-center rounded-md border border-border bg-card/40 p-1.5 text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground"
      aria-pressed={on}
      aria-label={on ? 'Hide archived' : 'Show archived'}
      title={on ? 'Showing closed runs' : 'Showing live runs'}
    >
      {on ? <Eye size={14} /> : <EyeOff size={14} />}
    </button>
  )
}
