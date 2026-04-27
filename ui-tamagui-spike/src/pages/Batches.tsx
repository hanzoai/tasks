// Batches — bulk operations across many workflow executions. List
// view + Start Batch dialog (terminate / cancel / signal / reset).

import { useCallback, useState } from 'react'
import { useParams } from 'react-router-dom'
import {
  Button,
  Card,
  Dialog,
  H2,
  Input,
  Text,
  XStack,
  YStack,
} from 'hanzogui'
import { Plus } from '@hanzogui/lucide-icons-2'
import type { BatchOperation } from '../lib/api'
import { apiPost } from '../lib/api'
import { useFetch } from '../lib/useFetch'
import { useTaskEvents } from '../lib/events'
import { Alert } from '../components/Alert'
import { Badge } from '../components/Badge'
import { Empty, ErrorState, LoadingState } from '../components/Empty'

const OPS = [
  { value: 'BATCH_OPERATION_TYPE_TERMINATE', label: 'Terminate' },
  { value: 'BATCH_OPERATION_TYPE_CANCEL', label: 'Cancel' },
  { value: 'BATCH_OPERATION_TYPE_SIGNAL', label: 'Signal' },
  { value: 'BATCH_OPERATION_TYPE_RESET', label: 'Reset' },
]

export function BatchesPage() {
  const { ns } = useParams()
  const namespace = ns!
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/batches`
  const { data, error, isLoading, mutate } = useFetch<{ batches: BatchOperation[] }>(url)

  const onEvent = useCallback(() => {
    void mutate()
  }, [mutate])

  useTaskEvents(namespace, onEvent, ['batch.started'])

  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState />
  const rows = data?.batches ?? []

  return (
    <YStack gap="$4">
      <XStack items="baseline" justify="space-between">
        <H2 size="$7" color="$color">
          Batches{' '}
          <Text fontSize="$3" color="$placeholderColor" fontWeight="400">
            ({rows.length})
          </Text>
        </H2>
        <StartBatchButton ns={namespace} onCreated={() => void mutate()} />
      </XStack>

      {rows.length === 0 ? (
        <Empty
          title={`No batch operations in ${namespace}`}
          hint="Bulk terminate / cancel / signal across many workflow executions."
        />
      ) : (
        <Card overflow="hidden" bg="$background" borderColor="$borderColor" borderWidth={1}>
          <XStack
            bg={'rgba(255,255,255,0.03)' as never}
            px="$4"
            py="$2.5"
            borderBottomWidth={1}
            borderBottomColor="$borderColor"
          >
            <HeaderCell flex={2}>Batch ID</HeaderCell>
            <HeaderCell flex={1}>Operation</HeaderCell>
            <HeaderCell flex={2}>Reason</HeaderCell>
            <HeaderCell flex={1}>State</HeaderCell>
            <HeaderCell flex={1}>Progress</HeaderCell>
            <HeaderCell flex={2}>Started</HeaderCell>
          </XStack>
          {rows.map((b, i) => (
            <XStack
              key={b.batchId}
              px="$4"
              py="$2.5"
              borderBottomWidth={i === rows.length - 1 ? 0 : 1}
              borderBottomColor="$borderColor"
              items="center"
            >
              <YStack flex={2} px="$2">
                <Text
                  fontFamily={'ui-monospace, SFMono-Regular, monospace' as never}
                  fontSize="$1"
                  color="$color"
                >
                  {b.batchId}
                </Text>
              </YStack>
              <YStack flex={1} px="$2">
                <Text fontSize="$2" color="$color">
                  {b.operation.replace('BATCH_OPERATION_TYPE_', '').toLowerCase()}
                </Text>
              </YStack>
              <YStack flex={2} px="$2">
                <Text fontSize="$2" color="$placeholderColor" numberOfLines={1}>
                  {b.reason || '—'}
                </Text>
              </YStack>
              <YStack flex={1} px="$2">
                <Badge variant={b.state.endsWith('COMPLETED') ? 'success' : 'muted'}>
                  {b.state.replace('BATCH_OPERATION_STATE_', '').toLowerCase()}
                </Badge>
              </YStack>
              <YStack flex={1} px="$2">
                <Text fontSize="$2" color="$placeholderColor">
                  {b.completeOperationCount} / {b.totalOperationCount || '—'}
                </Text>
              </YStack>
              <YStack flex={2} px="$2">
                <Text fontSize="$2" color="$placeholderColor">
                  {new Date(b.startTime).toLocaleString()}
                </Text>
              </YStack>
            </XStack>
          ))}
        </Card>
      )}
    </YStack>
  )
}

function HeaderCell({ children, flex }: { children: React.ReactNode; flex: number }) {
  return (
    <YStack flex={flex} px="$2">
      <Text fontSize="$1" fontWeight="500" color="$placeholderColor">
        {children}
      </Text>
    </YStack>
  )
}

function StartBatchButton({ ns, onCreated }: { ns: string; onCreated: () => void }) {
  const [open, setOpen] = useState(false)
  const [op, setOp] = useState('BATCH_OPERATION_TYPE_TERMINATE')
  const [query, setQuery] = useState("WorkflowType='X'")
  const [reason, setReason] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  async function submit() {
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
    <Dialog modal open={open} onOpenChange={setOpen}>
      <Dialog.Trigger asChild>
        <Button size="$3" bg={'#f2f2f2' as never} hoverStyle={{ background: '#ffffff' as never }}>
          <XStack items="center" gap="$1.5">
            <Plus size={14} color="#070b13" />
            <Text fontSize="$2" fontWeight="500" color={'#070b13' as never}>
              Start Batch
            </Text>
          </XStack>
        </Button>
      </Dialog.Trigger>
      <Dialog.Portal>
        <Dialog.Overlay key="overlay" bg={'rgba(0,0,0,0.6)' as never} />
        <Dialog.Content
          bg="$background"
          borderColor="$borderColor"
          borderWidth={1}
          minW={520}
          p="$5"
          gap="$4"
        >
          <Dialog.Title fontSize="$6" fontWeight="600" color="$color">
            Start a batch operation in {ns}
          </Dialog.Title>
          <Dialog.Description fontSize="$2" color="$placeholderColor">
            Apply a terminate / cancel / signal across every execution matching the visibility query.
          </Dialog.Description>
          <YStack gap="$3">
            <Field label="Operation">
              <XStack gap="$2" flexWrap="wrap">
                {OPS.map((o) => (
                  <Button
                    key={o.value}
                    size="$2"
                    onPress={() => setOp(o.value)}
                    bg={op === o.value ? ('#f2f2f2' as never) : 'transparent'}
                    borderWidth={1}
                    borderColor={op === o.value ? ('#f2f2f2' as never) : '$borderColor'}
                  >
                    <Text
                      fontSize="$2"
                      color={op === o.value ? ('#070b13' as never) : '$color'}
                    >
                      {o.label}
                    </Text>
                  </Button>
                ))}
              </XStack>
            </Field>
            <Field label="Visibility query">
              <Input value={query} onChangeText={setQuery} />
            </Field>
            <Field label="Reason">
              <Input value={reason} onChangeText={setReason} />
            </Field>
            {err && (
              <Alert variant="destructive" title="Could not start">
                {err}
              </Alert>
            )}
          </YStack>
          <XStack gap="$2" justify="flex-end" mt="$2">
            <Button chromeless onPress={() => setOpen(false)}>
              <Text fontSize="$2">Cancel</Text>
            </Button>
            <Button
              onPress={submit}
              disabled={submitting}
              bg={'#f2f2f2' as never}
              hoverStyle={{ background: '#ffffff' as never }}
            >
              <Text fontSize="$2" fontWeight="500" color={'#070b13' as never}>
                {submitting ? 'Starting…' : 'Start'}
              </Text>
            </Button>
          </XStack>
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <YStack gap="$1.5">
      <Text fontSize="$2" color="$color">
        {label}
      </Text>
      {children}
    </YStack>
  )
}
