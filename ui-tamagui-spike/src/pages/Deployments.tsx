// Deployments — worker version series. Cards mirror v1 layout.

import { useParams } from 'react-router-dom'
import { Card, H2, Text, XStack, YStack } from 'hanzogui'
import { Badge, Empty, ErrorState, LoadingState, useFetch } from '@hanzogui/admin'
import type { Deployment } from '../lib/api'

export function DeploymentsPage() {
  const { ns } = useParams()
  const namespace = ns!
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/deployments`
  const { data, error, isLoading } = useFetch<{ deployments: Deployment[] }>(url)

  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState />
  const rows = data?.deployments ?? []

  return (
    <YStack gap="$4">
      <XStack items="baseline" justify="space-between">
        <H2 size="$7" color="$color">
          Deployments{' '}
          <Text fontSize="$3" color="$placeholderColor" fontWeight="400">
            ({rows.length})
          </Text>
        </H2>
      </XStack>

      {rows.length === 0 ? (
        <Empty
          title={`No worker deployments in ${namespace}`}
          hint="Workers register a series + buildId on connect. Routing rules promote a default and ramp new versions."
        />
      ) : (
        <XStack gap="$4" flexWrap="wrap">
          {rows.map((d) => (
            <Card
              key={d.seriesName}
              p="$5"
              bg="$background"
              borderColor="$borderColor"
              borderWidth={1}
              flexBasis={320}
              flexGrow={1}
              gap="$3"
            >
              <YStack gap="$1">
                <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
                  SERIES
                </Text>
                <Text fontSize="$3" fontWeight="500" color="$color">
                  {d.seriesName}
                </Text>
              </YStack>
              <YStack gap="$1">
                <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
                  DEFAULT BUILD
                </Text>
                <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$2" color="$color">
                  {d.defaultBuildId || '—'}
                </Text>
              </YStack>
              <YStack gap="$1.5">
                <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
                  VERSIONS
                </Text>
                <YStack gap="$1.5">
                  {d.buildIds.map((b) => (
                    <XStack key={b.buildId} items="center" gap="$2">
                      <Text fontFamily={'ui-monospace, SFMono-Regular, monospace' as never} fontSize="$2" color="$color">
                        {b.buildId}
                      </Text>
                      <Badge
                        variant={b.state === 'DEPLOYMENT_STATE_CURRENT' ? 'success' : 'muted'}
                      >
                        {b.state.replace('DEPLOYMENT_STATE_', '').toLowerCase()}
                      </Badge>
                    </XStack>
                  ))}
                </YStack>
              </YStack>
            </Card>
          ))}
        </XStack>
      )}
    </YStack>
  )
}
