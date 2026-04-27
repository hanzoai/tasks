import { useEffect, useMemo, useState } from 'react'
import useSWR, { useSWRConfig } from 'swr'
import { Link, useParams } from 'react-router-dom'
import { Play, Plus, RefreshCw } from 'lucide-react'
import type { WorkflowExecution } from '../lib/api'
import { apiPost, ApiError } from '../lib/api'
import { Card } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { Label } from '../components/ui/label'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '../components/ui/dialog'
import { Alert, AlertDescription, AlertTitle } from '../components/ui/alert'
import { ErrorState } from '../components/ErrorState'
import { SavedViewsRail, SYSTEM_VIEWS, type SystemViewId } from '../components/SavedViewsRail'
import { EmptyWorkflowsHero } from '../components/EmptyWorkflowsHero'
import { FilterBar, type FilterMode } from '../components/FilterBar'
import { formatTimestamp } from '../components/LocalTimeIndicator'

// Workflows page replicates the upstream layout exactly:
//   • Big "N Workflows" header + refresh + last-fetch timestamp
//   • Start Workflow CTA on the right
//   • FilterBar (filter pill + view toggle) below the header
//   • Two-column body: SavedViewsRail | (table or empty hero)
// The empty state is the moon-and-sea hero with sample SDK links.

export function WorkflowsPage() {
  const { ns } = useParams()
  const namespace = ns!
  const { mutate } = useSWRConfig()

  const [activeView, setActiveView] = useState<SystemViewId>('all')
  const [query, setQuery] = useState('')
  const [filterMode, setFilterMode] = useState<FilterMode>('list')
  const [fetchedAt, setFetchedAt] = useState(new Date())

  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workflows?query=${encodeURIComponent(query)}&pageSize=50`
  const { data, error, isLoading, isValidating } = useSWR<{ executions?: WorkflowExecution[] }>(url)

  useEffect(() => {
    if (data) setFetchedAt(new Date())
  }, [data])

  const rows = data?.executions ?? []
  const count = rows.length

  const headerTimestamp = useMemo(() => formatTimestamp(fetchedAt), [fetchedAt])

  const refresh = () => mutate(url)

  return (
    <section className="flex h-full flex-col">
      <div className="border-b border-border bg-background/60 px-6 py-5">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-baseline gap-3">
            <h1 className="text-3xl font-semibold tracking-tight">
              {count} Workflow{count === 1 ? '' : 's'}
            </h1>
            <button
              type="button"
              onClick={refresh}
              className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
              aria-label="Refresh"
              title="Refresh"
            >
              <RefreshCw size={14} className={isValidating ? 'animate-spin' : ''} />
            </button>
            <span className="text-xs text-muted-foreground">{headerTimestamp}</span>
          </div>
          <StartWorkflowButton ns={namespace} onStarted={refresh} />
        </div>
      </div>

      <FilterBar
        mode={filterMode}
        onModeChange={setFilterMode}
        query={query}
        onQueryChange={setQuery}
      />

      <div className="flex flex-1 overflow-hidden">
        <SavedViewsRail
          activeId={activeView}
          query={query}
          onSelect={(view) => {
            setActiveView(view.id)
            setQuery(view.query)
          }}
        />

        <div className="flex-1 overflow-auto p-6">
          {error ? (
            <ErrorState error={error} />
          ) : isLoading ? (
            <ListSkeleton />
          ) : rows.length === 0 ? (
            <EmptyWorkflowsHero namespace={namespace} />
          ) : (
            <Card className="overflow-hidden py-0">
              <table className="w-full text-sm">
                <thead className="bg-muted/50 text-left text-muted-foreground">
                  <tr>
                    <th className="px-4 py-2.5 font-medium">Status</th>
                    <th className="px-4 py-2.5 font-medium">Workflow ID</th>
                    <th className="px-4 py-2.5 font-medium">Run ID</th>
                    <th className="px-4 py-2.5 font-medium">Type</th>
                    <th className="px-4 py-2.5 font-medium">Start</th>
                    <th className="px-4 py-2.5 font-medium">End</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border">
                  {rows.map((wf) => (
                    <tr
                      key={`${wf.execution.workflowId}-${wf.execution.runId}`}
                      className="hover:bg-accent/30"
                    >
                      <td className="px-4 py-2.5">
                        <Badge variant={statusVariant(wf.status)}>{shortStatus(wf.status)}</Badge>
                      </td>
                      <td className="px-4 py-2.5">
                        <Link
                          to={`/namespaces/${encodeURIComponent(namespace)}/workflows/${encodeURIComponent(wf.execution.workflowId)}?runId=${encodeURIComponent(wf.execution.runId)}`}
                          className="text-primary hover:underline"
                        >
                          {wf.execution.workflowId}
                        </Link>
                      </td>
                      <td className="px-4 py-2.5 font-mono text-xs text-muted-foreground">
                        {wf.execution.runId.slice(0, 8)}
                      </td>
                      <td className="px-4 py-2.5">{wf.type.name}</td>
                      <td className="px-4 py-2.5 text-muted-foreground">
                        {wf.startTime ? formatTimestamp(new Date(wf.startTime)) : '—'}
                      </td>
                      <td className="px-4 py-2.5 text-muted-foreground">
                        {wf.closeTime ? formatTimestamp(new Date(wf.closeTime)) : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </Card>
          )}
        </div>
      </div>

      {/* Make the SYSTEM_VIEWS const referenced so vite tree-shake keeps it.
          Saved views are wired through SavedViewsRail; this is a guard. */}
      {SYSTEM_VIEWS.length > 0 ? null : null}
    </section>
  )
}

function StartWorkflowButton({ ns, onStarted }: { ns: string; onStarted: () => void }) {
  const [open, setOpen] = useState(false)
  const [type, setType] = useState('')
  const [taskQueue, setTaskQueue] = useState('default')
  const [workflowId, setWorkflowId] = useState('')
  const [input, setInput] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setSubmitting(true)
    setErr(null)
    try {
      let parsedInput: unknown = undefined
      if (input.trim()) {
        try {
          parsedInput = JSON.parse(input)
        } catch {
          throw new Error('Input must be valid JSON.')
        }
      }
      await apiPost(`/v1/tasks/namespaces/${encodeURIComponent(ns)}/workflows`, {
        workflowId: workflowId || undefined,
        workflowType: { name: type },
        taskQueue: { name: taskQueue },
        input: parsedInput,
      })
      setOpen(false)
      onStarted()
    } catch (e) {
      if (e instanceof ApiError) {
        setErr(`${e.status === 501 ? 'Not yet implemented in native server' : 'Failed'}: ${e.message}`)
      } else {
        setErr(e instanceof Error ? e.message : String(e))
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm">
          <Plus />
          Start Workflow
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Start a workflow in {ns}</DialogTitle>
          <DialogDescription>
            Posts to <code>/v1/tasks/namespaces/{ns}/workflows</code> (opcode 0x0060).
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="wf-type">Workflow type *</Label>
            <Input
              id="wf-type"
              required
              placeholder="MyWorkflow"
              value={type}
              onChange={(e) => setType(e.target.value)}
            />
          </div>
          <div className="grid grid-cols-2 gap-4">
            <div className="space-y-1.5">
              <Label htmlFor="wf-tq">Task queue</Label>
              <Input id="wf-tq" value={taskQueue} onChange={(e) => setTaskQueue(e.target.value)} />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="wf-id">Workflow ID</Label>
              <Input
                id="wf-id"
                placeholder="auto"
                value={workflowId}
                onChange={(e) => setWorkflowId(e.target.value)}
              />
            </div>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="wf-input">Input (JSON, optional)</Label>
            <Input
              id="wf-input"
              placeholder='{"key": "value"}'
              value={input}
              onChange={(e) => setInput(e.target.value)}
            />
          </div>
          {err && (
            <Alert variant="destructive">
              <AlertTitle>Could not start</AlertTitle>
              <AlertDescription>{err}</AlertDescription>
            </Alert>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={() => setOpen(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={submitting || !type}>
              <Play />
              {submitting ? 'Starting…' : 'Start'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function ListSkeleton() {
  return (
    <div className="space-y-3">
      <div className="h-9 w-48 animate-pulse rounded bg-muted" />
      <div className="h-32 animate-pulse rounded bg-muted/50" />
    </div>
  )
}

function shortStatus(s: string) {
  return s.replace(/^WORKFLOW_EXECUTION_STATUS_/, '').toLowerCase()
}

function statusVariant(
  s: string
): 'success' | 'destructive' | 'warning' | 'muted' | 'default' {
  switch (s) {
    case 'WORKFLOW_EXECUTION_STATUS_RUNNING':
    case 'WORKFLOW_EXECUTION_STATUS_COMPLETED':
      return 'success'
    case 'WORKFLOW_EXECUTION_STATUS_FAILED':
    case 'WORKFLOW_EXECUTION_STATUS_TIMED_OUT':
    case 'WORKFLOW_EXECUTION_STATUS_TERMINATED':
      return 'destructive'
    case 'WORKFLOW_EXECUTION_STATUS_CANCELED':
      return 'warning'
    default:
      return 'muted'
  }
}
