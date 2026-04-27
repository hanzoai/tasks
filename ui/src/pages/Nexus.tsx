// Nexus — list of cross-namespace operation bridges.

import { useParams } from 'react-router-dom'
import { Card, H2, Text, XStack, YStack } from 'hanzogui'
import { Network } from '@hanzogui/lucide-icons-2'
import type { NexusEndpoint } from '../lib/api'
import { useFetch } from '../lib/useFetch'
import { Empty, ErrorState, LoadingState } from '../components/Empty'

export function NexusPage() {
  const { ns } = useParams()
  const namespace = ns!
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/nexus`
  const { data, error, isLoading } = useFetch<{ endpoints: NexusEndpoint[] }>(url)

  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState />
  const rows = data?.endpoints ?? []

  return (
    <YStack gap="$4">
      <XStack items="baseline" justify="space-between">
        <H2 size="$7" color="$color">
          Nexus{' '}
          <Text fontSize="$3" color="$placeholderColor" fontWeight="400">
            ({rows.length})
          </Text>
        </H2>
      </XStack>

      {rows.length === 0 ? (
        <Empty
          title={`No Nexus endpoints in ${namespace}`}
          hint="Cross-namespace operation bridges. A workflow in this namespace calls a handler in another."
        />
      ) : (
        <Card overflow="hidden" bg="$background" borderColor="$borderColor" borderWidth={1}>
          {rows.map((e, i) => (
            <XStack
              key={e.name}
              items="center"
              gap="$3"
              px="$5"
              py="$3.5"
              borderTopWidth={i === 0 ? 0 : 1}
              borderTopColor="$borderColor"
            >
              <Network size={16} color="#7e8794" />
              <YStack flex={1} minW={0}>
                <Text fontSize="$3" fontWeight="500" color="$color">
                  {e.name}
                </Text>
                {e.description ? (
                  <Text fontSize="$1" color="$placeholderColor" numberOfLines={1}>
                    {e.description}
                  </Text>
                ) : null}
              </YStack>
              <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$1" color="$placeholderColor">
                {e.target}
              </Text>
            </XStack>
          ))}
        </Card>
      )}
    </YStack>
  )
}
