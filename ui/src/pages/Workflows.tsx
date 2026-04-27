import { useState } from 'react'
import useSWR from 'swr'
import { Link, useParams } from 'react-router-dom'
import { Play, Plus } from 'lucide-react'
import type { WorkflowExecution } from '../lib/api'
import { apiPost, ApiError } from '../lib/api'
import { Card } from '../components/ui/card'
import { Badge } from '../components/ui/badge'
import { Button } from '../components/ui/button'
import { Input } from '../components/ui/input'
import { Label } from '../components/ui/label'
import { Skeleton } from '../components/ui/skeleton'
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
import { Empty } from '../components/Empty'

export function WorkflowsPage() {
  const { ns } = useParams()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/workflows?query=${encodeURIComponent('')}&pageSize=50`
  const { data, error, isLoading } = useSWR<{ executions?: WorkflowExecution[] }>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading) return <ListSkeleton title="Workflows" />
  const rows = data?.executions ?? []

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Workflows <span className="text-muted-foreground text-sm">({rows.length})</span>
        </h2>
        <StartWorkflowButton ns={ns!} />
      </header>

      {rows.length === 0 ? (
        <Empty
          title={`No workflows in ${ns}`}
          hint="Start one from your code with the Hanzo Tasks SDK, or click Start Workflow."
          action={<StartWorkflowButton ns={ns!} />}
        />
      ) : (
        <Card className="py-0 overflow-hidden">
          <table className="w-full text-sm">
            <thead className="bg-muted/50 text-muted-foreground text-left">
              <tr>
                <th className="px-4 py-2.5 font-medium">Workflow ID</th>
                <th className="px-4 py-2.5 font-medium">Type</th>
                <th className="px-4 py-2.5 font-medium">Status</th>
                <th className="px-4 py-2.5 font-medium">Task queue</th>
                <th className="px-4 py-2.5 font-medium">Started</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {rows.map((wf) => (
                <tr key={`${wf.execution.workflowId}-${wf.execution.runId}`} className="hover:bg-accent/30">
                  <td className="px-4 py-2.5">
                    <Link
                      to={`/namespaces/${encodeURIComponent(ns!)}/workflows/${encodeURIComponent(wf.execution.workflowId)}?runId=${encodeURIComponent(wf.execution.runId)}`}
                      className="text-primary hover:underline"
                    >
                      {wf.execution.workflowId}
                    </Link>
                  </td>
                  <td className="px-4 py-2.5">{wf.type.name}</td>
                  <td className="px-4 py-2.5">
                    <Badge variant={statusVariant(wf.status)}>{shortStatus(wf.status)}</Badge>
                  </td>
                  <td className="px-4 py-2.5 text-muted-foreground">{wf.taskQueue ?? '—'}</td>
                  <td className="px-4 py-2.5 text-muted-foreground">
                    {wf.startTime ? new Date(wf.startTime).toLocaleString() : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </section>
  )
}

function StartWorkflowButton({ ns }: { ns: string }) {
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

function ListSkeleton({ title }: { title: string }) {
  return (
    <section className="space-y-4">
      <Skeleton className="h-7 w-48" />
      <Card className="py-6 space-y-4">
        {[0, 1, 2].map((i) => (
          <Skeleton key={i} className="h-5 w-full" />
        ))}
      </Card>
      <span className="sr-only">{title}</span>
    </section>
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
