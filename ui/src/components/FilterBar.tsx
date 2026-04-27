import { Code2, List, Plus, SlidersHorizontal } from 'lucide-react'
import { cn } from '../lib/utils'

// FilterBar mimics Temporal's workflow-filters.svelte: filter icon,
// "+ Add Filter" pill, and a code/list view toggle on the right.
// The filter chips themselves arrive when the predicate engine ships;
// for now this is a structural shell that records intent.

export type FilterMode = 'list' | 'code'

export function FilterBar({
  mode,
  onModeChange,
  query,
  onQueryChange,
}: {
  mode: FilterMode
  onModeChange: (m: FilterMode) => void
  query: string
  onQueryChange: (q: string) => void
}) {
  return (
    <div className="flex items-center gap-3 border-b border-border bg-card/30 px-4 py-2.5">
      <button
        type="button"
        className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
        aria-label="Filters"
        title="Filters"
      >
        <SlidersHorizontal size={16} />
      </button>

      {mode === 'list' ? (
        <button
          type="button"
          className="inline-flex items-center gap-1.5 rounded-md border border-dashed border-border bg-transparent px-3 py-1 text-xs text-muted-foreground transition-colors hover:border-foreground/40 hover:text-foreground"
          onClick={() => onModeChange('code')}
        >
          <Plus size={12} />
          Add Filter
        </button>
      ) : (
        <input
          type="text"
          value={query}
          onChange={(e) => onQueryChange(e.target.value)}
          placeholder='ExecutionStatus = "Running"'
          className="flex-1 rounded-md border border-border bg-background px-3 py-1.5 font-mono text-xs text-foreground outline-none placeholder:text-muted-foreground focus:border-ring"
          spellCheck={false}
        />
      )}

      <div className="ml-auto inline-flex rounded-md border border-border bg-background p-0.5">
        <button
          type="button"
          onClick={() => onModeChange('list')}
          className={cn(
            'rounded px-2 py-1 text-xs transition-colors',
            mode === 'list' ? 'bg-accent text-accent-foreground' : 'text-muted-foreground hover:text-foreground'
          )}
          aria-label="List filter mode"
          title="List"
        >
          <List size={13} />
        </button>
        <button
          type="button"
          onClick={() => onModeChange('code')}
          className={cn(
            'rounded px-2 py-1 text-xs transition-colors',
            mode === 'code' ? 'bg-accent text-accent-foreground' : 'text-muted-foreground hover:text-foreground'
          )}
          aria-label="Code filter mode"
          title="Code"
        >
          <Code2 size={13} />
        </button>
      </div>
    </div>
  )
}
