import { useState } from 'react'
import useSWR from 'swr'
import { useParams } from 'react-router-dom'
import { Plus } from 'lucide-react'
import type { BatchOperation } from '../lib/api'
import { apiPost } from '../lib/api'
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

export function BatchesPage() {
  const { ns } = useParams()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}/batches`
  const { data, error, isLoading, mutate } = useSWR<{ batches: BatchOperation[] }>(url)

  if (error) return <ErrorState error={error} />
  if (isLoading) return <Skeleton className="h-40" />
  const rows = data?.batches ?? []

  return (
    <section className="space-y-4">
      <header className="flex items-center justify-between">
        <h2 className="text-lg font-medium">
          Batches <span className="text-muted-foreground text-sm">({rows.length})</span>
        </h2>
        <StartBatchButton ns={ns!} onCreated={mutate} />
      </header>

      {rows.length === 0 ? (
        <Empty
          title={`No batch operations in ${ns}`}
          hint="Bulk terminate / cancel / signal across many workflow executions."
          action={<StartBatchButton ns={ns!} onCreated={mutate} />}
        />
      ) : (
        <Card className="py-0 overflow-hidden">
          <table className="w-full text-sm">
            <thead className="bg-muted/40 text-muted-foreground text-left">
              <tr>
                <th className="px-4 py-2.5 font-medium">Batch ID</th>
                <th className="px-4 py-2.5 font-medium">Operation</th>
                <th className="px-4 py-2.5 font-medium">Reason</th>
                <th className="px-4 py-2.5 font-medium">State</th>
                <th className="px-4 py-2.5 font-medium">Progress</th>
                <th className="px-4 py-2.5 font-medium">Started</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-border">
              {rows.map((b) => (
                <tr key={b.batchId}>
                  <td className="px-4 py-2.5 font-mono text-xs">{b.batchId}</td>
                  <td className="px-4 py-2.5">
                    {b.operation.replace('BATCH_OPERATION_TYPE_', '').toLowerCase()}
                  </td>
                  <td className="px-4 py-2.5 text-muted-foreground">{b.reason || '—'}</td>
                  <td className="px-4 py-2.5">
                    <Badge variant={b.state.endsWith('COMPLETED') ? 'success' : 'muted'}>
                      {b.state.replace('BATCH_OPERATION_STATE_', '').toLowerCase()}
                    </Badge>
                  </td>
                  <td className="px-4 py-2.5 text-muted-foreground">
                    {b.completeOperationCount} / {b.totalOperationCount || '—'}
                  </td>
                  <td className="px-4 py-2.5 text-muted-foreground">
                    {new Date(b.startTime).toLocaleString()}
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

function StartBatchButton({ ns, onCreated }: { ns: string; onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  const [op, setOp] = useState('BATCH_OPERATION_TYPE_TERMINATE')
  const [query, setQuery] = useState("WorkflowType='X'")
  const [reason, setReason] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  async function submit(e: React.FormEvent) {
    e.preventDefault()
    setSubmitting(true)
    setErr(null)
    try {
      await apiPost(`/v1/tasks/namespaces/${encodeURIComponent(ns)}/batches`, {
        operation: op,
        query,
        reason,
      })
      setOpen(false)
      onCreated()
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm">
          <Plus />
          Start Batch
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Start a batch operation in {ns}</DialogTitle>
          <DialogDescription>
            Apply a terminate / cancel / signal across every execution matching the visibility query.
          </DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="b-op">Operation</Label>
            <select
              id="b-op"
              value={op}
              onChange={(e) => setOp(e.target.value)}
              className="flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs"
            >
              <option value="BATCH_OPERATION_TYPE_TERMINATE">Terminate</option>
              <option value="BATCH_OPERATION_TYPE_CANCEL">Cancel</option>
              <option value="BATCH_OPERATION_TYPE_SIGNAL">Signal</option>
              <option value="BATCH_OPERATION_TYPE_RESET">Reset</option>
            </select>
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="b-q">Visibility query</Label>
            <Input id="b-q" value={query} onChange={(e) => setQuery(e.target.value)} />
          </div>
          <div className="space-y-1.5">
            <Label htmlFor="b-r">Reason</Label>
            <Input id="b-r" value={reason} onChange={(e) => setReason(e.target.value)} />
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
            <Button type="submit" disabled={submitting}>
              {submitting ? 'Starting…' : 'Start'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
