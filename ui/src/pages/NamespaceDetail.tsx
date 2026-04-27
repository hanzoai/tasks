// Namespace detail — Overview/Identities tabs + Region/Retention/APS
// cards + Connect dropdown showing ZAP and HTTP endpoints with copy
// buttons. Mirrors the deprecated React/shadcn page 1:1.

import { Link, useParams } from 'react-router-dom'
import {
  Button,
  Card,
  H1,
  H4,
  Paragraph,
  Popover,
  Tabs,
  Text,
  XStack,
  YStack,
} from 'hanzogui'
import {
  ChevronDown,
  ChevronLeft,
  Globe,
  Plug,
} from '@hanzogui/lucide-icons-2'
import type { Identity, Namespace } from '../lib/api'
import { useFetch } from '../lib/useFetch'
import { Badge } from '../components/Badge'
import { CopyField } from '../components/CopyField'
import { Empty, ErrorState, LoadingState } from '../components/Empty'
import { humanTTL } from '../lib/format'

export function NamespaceDetailPage() {
  const { ns } = useParams()
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns!)}`
  const { data, error, isLoading } = useFetch<Namespace>(url)

  if (error) return <ErrorState error={error as Error} />
  if (isLoading || !data) return <LoadingState />

  const { namespaceInfo, config } = data
  const active = namespaceInfo.state === 'NAMESPACE_STATE_REGISTERED'

  return (
    <YStack gap="$5">
      <Link to="/namespaces" style={{ textDecoration: 'none', color: 'inherit' }}>
        <XStack items="center" gap="$1.5" hoverStyle={{ opacity: 0.8 }}>
          <ChevronLeft size={14} color="#7e8794" />
          <Text fontSize="$2" color="$placeholderColor">
            Back to Namespaces
          </Text>
          <Text fontSize="$2" color="$placeholderColor" px="$2">
            |
          </Text>
          <Link
            to={`/namespaces/${encodeURIComponent(ns!)}/workflows`}
            style={{ textDecoration: 'none' }}
          >
            <Text fontSize="$2" color="$placeholderColor" hoverStyle={{ color: '$color' as any }}>
              Go to Workflows
            </Text>
          </Link>
        </XStack>
      </Link>

      <XStack items="flex-start" gap="$3">
        <YStack gap="$1" flex={1}>
          <XStack items="center" gap="$3">
            <H1 size="$8" color="$color" fontWeight="600">
              {namespaceInfo.name}
            </H1>
            <Badge variant={active ? 'success' : 'muted'}>{active ? 'Active' : 'Inactive'}</Badge>
          </XStack>
          {namespaceInfo.description && (
            <Paragraph color="$placeholderColor" fontSize="$2">
              {namespaceInfo.description}
            </Paragraph>
          )}
        </YStack>
        <ConnectDropdown ns={namespaceInfo.name} />
      </XStack>

      <Tabs defaultValue="overview" orientation="horizontal" flexDirection="column">
        <Tabs.List
          borderBottomWidth={1}
          borderBottomColor="$borderColor"
          gap="$2"
          self="flex-start"
        >
          <TabTrigger value="overview">Overview</TabTrigger>
          <TabTrigger value="identities">Identities</TabTrigger>
        </Tabs.List>

        <Tabs.Content value="overview" mt="$4">
          <XStack gap="$3" flexWrap="wrap">
            <StatCard label="Region" flexBasis={220}>
              <XStack items="center" gap="$2">
                <Globe size={14} color="#7e8794" />
                <Text fontSize="$5" fontWeight="500" color="$color">
                  {namespaceInfo.region || 'embedded'}
                </Text>
              </XStack>
            </StatCard>
            <StatCard
              label="Retention Policy"
              hint={config.workflowExecutionRetentionTtl}
              flexBasis={220}
            >
              <Text fontSize="$5" fontWeight="500" color="$color">
                {humanTTL(config.workflowExecutionRetentionTtl)}
              </Text>
            </StatCard>
            <StatCard label="APS Limit" hint="actions per second" flexBasis={220}>
              <Text fontSize="$5" fontWeight="500" color="$color">
                {config.apsLimit.toLocaleString()}
              </Text>
            </StatCard>
          </XStack>
        </Tabs.Content>

        <Tabs.Content value="identities" mt="$4">
          <IdentitiesPanel ns={ns!} />
        </Tabs.Content>
      </Tabs>
    </YStack>
  )
}

function TabTrigger({ value, children }: { value: string; children: React.ReactNode }) {
  return (
    <Tabs.Tab value={value} px="$3" py="$2" unstyled bg="transparent">
      <Text fontSize="$2" color="$color">
        {children}
      </Text>
    </Tabs.Tab>
  )
}

function ConnectDropdown({ ns }: { ns: string }) {
  const host = typeof window === 'undefined' ? 'tasks.local' : window.location.hostname
  const zap = `${host}:9999`
  const http =
    typeof window === 'undefined'
      ? `https://tasks.local/v1/tasks/namespaces/${encodeURIComponent(ns)}`
      : `${window.location.protocol}//${window.location.host}/v1/tasks/namespaces/${encodeURIComponent(ns)}`

  return (
    <Popover placement="bottom-end">
      <Popover.Trigger asChild>
        <Button size="$2" borderWidth={1} borderColor="$borderColor">
          <XStack items="center" gap="$1.5">
            <Plug size={14} />
            <Text fontSize="$2">Connect</Text>
            <ChevronDown size={14} />
          </XStack>
        </Button>
      </Popover.Trigger>
      <Popover.Content
        bg="$background"
        borderWidth={1}
        borderColor="$borderColor"
        p="$3"
        minW={420}
        elevate
      >
        <YStack gap="$3">
          <YStack gap="$1">
            <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
              ZAP ENDPOINT
            </Text>
            <CopyField value={zap} />
          </YStack>
          <YStack gap="$1">
            <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
              HTTP ENDPOINT
            </Text>
            <CopyField value={http} />
          </YStack>
          <Text fontSize="$1" color="$placeholderColor">
            ZAP is the canonical native binary transport. HTTP/JSON is browser-only.
          </Text>
        </YStack>
      </Popover.Content>
    </Popover>
  )
}

function StatCard({
  label,
  hint,
  children,
  flexBasis,
}: {
  label: string
  hint?: string
  children: React.ReactNode
  flexBasis?: number
}) {
  return (
    <Card
      p="$4"
      bg="$background"
      borderColor="$borderColor"
      borderWidth={1}
      flexBasis={flexBasis}
      flexGrow={1}
    >
      <YStack gap="$1">
        <Text fontSize="$1" color="$placeholderColor" fontWeight="600" letterSpacing={0.4}>
          {label.toUpperCase()}
        </Text>
        {children}
        {hint && (
          <Text fontSize="$1" color="$placeholderColor">
            {hint}
          </Text>
        )}
      </YStack>
    </Card>
  )
}

function IdentitiesPanel({ ns }: { ns: string }) {
  const url = `/v1/tasks/namespaces/${encodeURIComponent(ns)}/identities`
  const { data, error, isLoading } = useFetch<{ identities: Identity[] }>(url)
  if (error) return <ErrorState error={error as Error} />
  if (isLoading) return <LoadingState rows={2} />
  const rows = data?.identities ?? []
  if (rows.length === 0) {
    return (
      <Empty
        title="No identities granted"
        hint="Identities are sourced from hanzo.id IAM. Grant access via the Hanzo Tasks SDK or POST /identities."
      />
    )
  }
  return (
    <Card overflow="hidden" bg="$background" borderColor="$borderColor" borderWidth={1}>
      <XStack
        bg={'rgba(255,255,255,0.03)' as never}
        px="$4"
        py="$2"
        borderBottomWidth={1}
        borderBottomColor="$borderColor"
      >
        <H4 flex={2} fontSize="$2" color="$placeholderColor" fontWeight="500">
          Email
        </H4>
        <H4 flex={1} fontSize="$2" color="$placeholderColor" fontWeight="500">
          Role
        </H4>
        <H4 flex={1} fontSize="$2" color="$placeholderColor" fontWeight="500">
          Granted
        </H4>
      </XStack>
      {rows.map((id, i) => (
        <XStack
          key={`${id.email}-${i}`}
          px="$4"
          py="$2.5"
          borderTopWidth={i === 0 ? 0 : 1}
          borderTopColor="$borderColor"
          items="center"
        >
          <Text flex={2} fontSize="$2" color="$color">
            {id.email}
          </Text>
          <YStack flex={1}>
            <Badge variant="muted">{id.role}</Badge>
          </YStack>
          <Text flex={1} fontSize="$2" color="$placeholderColor">
            {new Date(id.grantTime).toLocaleString()}
          </Text>
        </XStack>
      ))}
    </Card>
  )
}
