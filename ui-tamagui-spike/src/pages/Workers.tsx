// Workers — empty state with worker SDK hint. Worker heartbeats land
// with pkg/sdk/worker; for now this surface stays honest rather than
// inventing fake polling data.

import { useParams } from 'react-router-dom'
import { H2, Text, XStack, YStack } from 'hanzogui'
import { useFetch } from '../lib/useFetch'
import { Alert } from '../components/Alert'
import { Empty, ErrorState, LoadingState } from '../components/Empty'

interface WorkersResp {
  workers?: unknown[]
}

export function WorkersPage() {
  const { ns } = useParams()
  const namespace = ns!
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/workers`
  const { data, error, isLoading } = useFetch<WorkersResp>(url)

  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState />
  const rows = data?.workers ?? []

  return (
    <YStack gap="$4">
      <XStack items="baseline" justify="space-between">
        <H2 size="$7" color="$color">
          Workers{' '}
          <Text fontSize="$3" color="$placeholderColor" fontWeight="400">
            ({rows.length})
          </Text>
        </H2>
      </XStack>

      <Alert title="Worker registration not yet wired">
        Worker registration lands with the worker SDK runtime (pkg/sdk/worker).
        Until then, this surface stays empty by design rather than showing fake
        data.
      </Alert>

      {rows.length === 0 ? (
        <Empty
          title={`No workers polling ${namespace}`}
          hint="Use the Hanzo Tasks SDK at pkg/sdk/worker to register a worker. The next release lands worker heartbeats and live polling status here."
        />
      ) : null}
    </YStack>
  )
}
