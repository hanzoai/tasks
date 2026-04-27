// Schedules — list of recurring workflow specs. Read-only for now;
// the upstream UI surfaces a create dialog but the native engine
// ships writes via the SDK first.

import { useCallback } from 'react'
import { useParams } from 'react-router-dom'
import { Card, H2, Text, XStack, YStack } from 'hanzogui'
import { Empty, ErrorState, LoadingState, useFetch } from '@hanzogui/admin'
import type { Schedule } from '../lib/api'
import { useTaskEvents } from '../lib/events'

interface ListResp {
  schedules?: Schedule[]
}

export function SchedulesPage() {
  const { ns } = useParams()
  const namespace = ns!
  const url = `/v1/tasks/namespaces/${encodeURIComponent(namespace)}/schedules`
  const { data, error, isLoading, mutate } = useFetch<ListResp>(url)

  const onEvent = useCallback(() => {
    void mutate()
  }, [mutate])

  useTaskEvents(namespace, onEvent, [
    'schedule.created',
    'schedule.paused',
    'schedule.resumed',
    'schedule.deleted',
  ])

  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState />
  const rows = data?.schedules ?? []

  return (
    <YStack gap="$4">
      <XStack items="baseline" justify="space-between">
        <H2 size="$7" color="$color">
          Schedules{' '}
          <Text fontSize="$3" color="$placeholderColor" fontWeight="400">
            ({rows.length})
          </Text>
        </H2>
      </XStack>

      {rows.length === 0 ? (
        <Empty
          title={`No schedules in ${namespace}`}
          hint="Create one with the Hanzo Tasks SDK; the UI surface is read-only for now."
        />
      ) : (
        <Card overflow="hidden" bg="$background" borderColor="$borderColor" borderWidth={1}>
          {rows.map((s, i) => (
            <YStack
              key={s.scheduleId}
              px="$5"
              py="$3.5"
              borderTopWidth={i === 0 ? 0 : 1}
              borderTopColor="$borderColor"
            >
              <Text fontSize="$3" fontWeight="500" color="$color">
                {s.scheduleId}
              </Text>
              <Text mt="$1" fontSize="$1" color="$placeholderColor">
                {describeSpec(s)}
              </Text>
            </YStack>
          ))}
        </Card>
      )}
    </YStack>
  )
}

function describeSpec(s: Schedule): string {
  const spec = s.spec
  if (!spec) return 'no spec'
  if (spec.cronString?.length) return `cron: ${spec.cronString.join(', ')}`
  if (spec.interval?.length)
    return `every ${spec.interval.map((i) => i.interval).join(', ')}`
  return 'custom'
}
