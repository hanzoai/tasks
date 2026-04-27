// Support — 4 cards (GitHub, Docs, Issue, Health).

import { Card, H2, Text, XStack, YStack } from 'hanzogui'
import {
  BookOpen,
  ExternalLink,
  Github,
  MessageSquare,
} from '@hanzogui/lucide-icons-2'

export function SupportPage() {
  return (
    <YStack gap="$4" maxW={760}>
      <H2 size="$7" color="$color">
        Support
      </H2>
      <XStack gap="$3" flexWrap="wrap">
        <SupportLink
          href="https://github.com/hanzoai/tasks"
          title="hanzoai/tasks on GitHub"
          hint="Source, issues, discussions"
          icon={Github}
        />
        <SupportLink
          href="https://hanzo.ai/docs/tasks"
          title="Documentation"
          hint="SDK guide, opcodes, IAM integration"
          icon={BookOpen}
        />
        <SupportLink
          href="https://github.com/hanzoai/tasks/issues/new"
          title="File an issue"
          hint="Bug reports, feature requests"
          icon={MessageSquare}
        />
        <SupportLink
          href="/healthz"
          title="Server health"
          hint="GET /healthz · GET /v1/tasks/health"
          icon={ExternalLink}
        />
      </XStack>
    </YStack>
  )
}

function SupportLink({
  href,
  title,
  hint,
  icon: Icon,
}: {
  href: string
  title: string
  hint: string
  icon: React.ComponentType<{ size?: number; color?: string }>
}) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      style={{ flex: '1 1 320px', textDecoration: 'none' }}
    >
      <Card
        p="$5"
        bg="$background"
        borderColor="$borderColor"
        borderWidth={1}
        hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
      >
        <XStack items="flex-start" gap="$3">
          <Icon size={18} color="#7e8794" />
          <YStack flex={1} minW={0}>
            <Text fontSize="$3" fontWeight="500" color="$color">
              {title}
            </Text>
            <Text fontSize="$2" color="$placeholderColor">
              {hint}
            </Text>
          </YStack>
          <ExternalLink size={14} color="#7e8794" />
        </XStack>
      </Card>
    </a>
  )
}
