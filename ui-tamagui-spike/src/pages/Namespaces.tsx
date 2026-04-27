// Namespaces list — clicking a row navigates to the namespace detail.

import { useCallback } from 'react'
import { Link } from 'react-router-dom'
import { Card, H2, Text, XStack, YStack } from 'hanzogui'
import { ChevronRight } from '@hanzogui/lucide-icons-2'
import { Badge, Empty, ErrorState, LoadingState, humanTTL, useFetch } from '@hanzogui/admin'
import type { Namespace } from '../lib/api'
import { useTaskEvents } from '../lib/events'

export function NamespacesPage() {
  const url = '/v1/tasks/namespaces?pageSize=200'
  const { data, error, isLoading, mutate } = useFetch<{ namespaces: Namespace[] }>(url)

  const onEvent = useCallback(() => {
    void mutate()
  }, [mutate])

  useTaskEvents(undefined, onEvent, ['namespace.registered'])

  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState />
  const nss = data?.namespaces ?? []
  if (nss.length === 0) {
    return <Empty title="No namespaces" hint="Create one with the SDK or hanzo-tasks CLI." />
  }

  return (
    <YStack gap="$4">
      <XStack items="baseline" justify="space-between">
        <H2 size="$7" color="$color">
          Namespaces{' '}
          <Text fontSize="$3" color="$placeholderColor" fontWeight="400">
            ({nss.length})
          </Text>
        </H2>
      </XStack>
      <Card overflow="hidden" bg="$background" borderColor="$borderColor" borderWidth={1}>
        {nss.map((ns, i) => {
          const name = ns.namespaceInfo.name
          const ok = ns.namespaceInfo.state === 'NAMESPACE_STATE_REGISTERED'
          const state = ns.namespaceInfo.state.replace(/^NAMESPACE_STATE_/, '').toLowerCase()
          return (
            <Link
              key={name}
              to={`/namespaces/${encodeURIComponent(name)}`}
              style={{ textDecoration: 'none', color: 'inherit' }}
            >
              <XStack
                items="center"
                gap="$4"
                px="$5"
                py="$3.5"
                borderTopWidth={i === 0 ? 0 : 1}
                borderTopColor="$borderColor"
                hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
              >
                <Text fontSize="$3" fontWeight="500" color="$color">
                  {name}
                </Text>
                <Badge variant={ok ? 'success' : 'muted'}>{state}</Badge>
                {ns.namespaceInfo.description ? (
                  <Text fontSize="$2" color="$placeholderColor" numberOfLines={1} flex={1}>
                    {ns.namespaceInfo.description}
                  </Text>
                ) : (
                  <YStack flex={1} />
                )}
                <Text fontSize="$1" color="$placeholderColor">
                  retention {humanTTL(ns.config?.workflowExecutionRetentionTtl)}
                </Text>
                <ChevronRight size={16} color="#7e8794" />
              </XStack>
            </Link>
          )
        })}
      </Card>
    </YStack>
  )
}
