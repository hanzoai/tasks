import { Bookmark, ChevronLeft, ChevronRight, Clock, GitBranch, History, Layers, ListTree, Smile, X } from 'lucide-react'
import { useState } from 'react'
import { cn } from '../lib/utils'

// SavedViewsRail mimics Temporal's left "Saved Views" panel (slide.svelte).
// The rail is structural only — wired filters land when the canonical
// query DSL ships server-side. Each system view sets a query string the
// page reads via the onSelect callback.
//
// Two sections:
//   1. System views (All Workflows, Task Failures, Running, …)
//   2. "Custom Views N/MAX" with a per-namespace list (empty for now).

export type SystemViewId =
  | 'all'
  | 'task-failures'
  | 'running'
  | 'parents'
  | 'today'
  | 'last-hour'

export interface SystemView {
  id: SystemViewId
  name: string
  query: string
  icon: React.ComponentType<any>
  badge?: { content: string | number; tone: 'muted' | 'happy' }
}

const SYSTEM_VIEWS: SystemView[] = [
  { id: 'all', name: 'All Workflows', query: '', icon: ListTree },
  {
    id: 'task-failures',
    name: 'Task Failures',
    // Mimics Temporal's TASK_FAILURES_QUERY shape; the server treats unknown
    // queries as no-op until the predicate engine lands.
    query: 'WorkflowTaskFailureCount > 0',
    icon: History,
    badge: { content: 0, tone: 'happy' },
  },
  {
    id: 'running',
    name: 'Running',
    query: 'ExecutionStatus = "Running"',
    icon: Clock,
  },
  {
    id: 'parents',
    name: 'Parent Workflows',
    query: 'ParentWorkflowId is null',
    icon: GitBranch,
  },
  { id: 'today', name: 'Today', query: 'StartTime > "today"', icon: Layers },
  { id: 'last-hour', name: 'Last Hour', query: 'StartTime > "1h"', icon: Clock },
]

const MAX_VIEWS = 20

export function SavedViewsRail({
  activeId,
  query,
  onSelect,
}: {
  activeId: SystemViewId
  query: string
  onSelect: (view: SystemView) => void
}) {
  const [open, setOpen] = useState(true)
  // No persistence yet — Temporal stores per-namespace in localStorage.
  // Stub for now; lands with the saved-query store.
  const customViews: { id: string; name: string; query: string }[] = []

  return (
    <div
      className={cn(
        'shrink-0 border-r border-border bg-card/30 transition-all duration-200',
        open ? 'w-56' : 'w-12'
      )}
    >
      <div className="flex items-center justify-between border-b border-border px-3 py-2">
        {open ? <p className="text-sm font-medium">Saved Views</p> : <span className="sr-only">Saved Views</span>}
        <button
          type="button"
          onClick={() => setOpen((o) => !o)}
          className="rounded p-0.5 text-muted-foreground hover:bg-accent hover:text-foreground"
          aria-label={open ? 'Collapse saved views' : 'Expand saved views'}
        >
          {open ? <ChevronLeft size={14} /> : <ChevronRight size={14} />}
        </button>
      </div>

      <div className="space-y-1 p-1.5">
        {SYSTEM_VIEWS.map((view) => {
          const active = view.id === activeId
          const Icon = view.icon
          return (
            <button
              key={view.id}
              type="button"
              onClick={() => onSelect(view)}
              className={cn(
                'flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-sm transition-colors',
                active
                  ? 'bg-accent text-accent-foreground'
                  : 'text-muted-foreground hover:bg-accent/50 hover:text-foreground'
              )}
              title={view.name}
            >
              <Icon size={14} className="shrink-0" />
              {open && (
                <>
                  <span className="truncate text-left">{view.name}</span>
                  {view.badge && (
                    <span className="ml-auto inline-flex items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-xs font-medium text-muted-foreground">
                      {view.badge.content}
                      {view.badge.tone === 'happy' && (
                        <Smile size={11} className="text-muted-foreground" />
                      )}
                    </span>
                  )}
                </>
              )}
            </button>
          )
        })}
      </div>

      {open && (
        <>
          <div className="flex items-center justify-between px-3 py-2">
            <p className="text-sm font-medium">Custom Views</p>
            <span className="rounded-full bg-muted px-2 py-0.5 font-mono text-xs text-muted-foreground">
              {customViews.length}/{MAX_VIEWS}
            </span>
          </div>
          <div className="border-t border-border" />
          <div className="px-4 py-3">
            {customViews.length === 0 ? (
              <p className="text-sm text-muted-foreground">No Views</p>
            ) : (
              <ul className="space-y-1">
                {customViews.map((v) => (
                  <li
                    key={v.id}
                    className="flex items-center gap-2 rounded-md px-2 py-1 text-sm text-muted-foreground hover:bg-accent/40 hover:text-foreground"
                  >
                    <Bookmark size={14} />
                    <span className="truncate">{v.name}</span>
                    <X size={12} className="ml-auto opacity-0 group-hover:opacity-100" />
                  </li>
                ))}
              </ul>
            )}
            {query && activeId === 'all' && (
              <p className="mt-2 text-xs text-muted-foreground/70">
                Unsaved query active.
              </p>
            )}
          </div>
        </>
      )}
    </div>
  )
}

export { SYSTEM_VIEWS }
