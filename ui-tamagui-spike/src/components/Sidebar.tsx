// Left sidebar — top group (Namespaces, Workflows, Schedules, Batch,
// Deployments, Task Queues, Workers, Nexus), divider, bottom group
// (Archive disabled, Import disabled, Docs external), footer
// (Feedback link + version).
//
// The brief says match v1 1:1 with Hanzo branding swapped in. This
// is the Tamagui rewrite — primitives only, NavLink from
// react-router-dom for active state highlighting.

import { NavLink } from 'react-router-dom'
import { Text, XStack, YStack } from 'hanzogui'
import {
  Activity,
  Archive,
  BookOpen,
  Heart,
  Layers,
  ListChecks,
  Network,
  Rocket,
  Timer,
  Upload,
  Users,
  Workflow,
} from '@hanzogui/lucide-icons-2'
import { HanzoMark } from './HanzoMark'

// Version comes from VITE_APP_VERSION at build time (default in vite.config.ts).
const APP_VERSION = (import.meta as any).env?.VITE_APP_VERSION ?? '2.45.3'

export function Sidebar({ ns }: { ns?: string }) {
  const ent = ns ? `/namespaces/${encodeURIComponent(ns)}` : ''
  return (
    <YStack
      width={224}
      height="100vh"
      borderRightWidth={1}
      borderRightColor="$borderColor"
      bg={'rgba(7,11,19,0.6)' as never}
    >
      {/* Brand row */}
      <XStack
        px="$4"
        py="$3"
        borderBottomWidth={1}
        borderBottomColor="$borderColor"
        items="center"
        gap="$2"
      >
        <HanzoMark />
        <YStack>
          <Text fontSize="$3" fontWeight="600" color="$color" letterSpacing={-0.2}>
            Hanzo Tasks
          </Text>
          <Text
            fontSize={9}
            color="$placeholderColor"
            letterSpacing={1.2}
            textTransform={'uppercase' as any}
          >
            Self-Hosted
          </Text>
        </YStack>
      </XStack>

      {/* Top group */}
      <YStack flex={1} p="$2" gap="$1">
        <NavItem to="/namespaces" icon={Layers} end>
          Namespaces
        </NavItem>
        <NavItem to={ent ? `${ent}/workflows` : '/namespaces'} icon={Workflow} disabled={!ns}>
          Workflows
        </NavItem>
        <NavItem to={ent ? `${ent}/schedules` : '/namespaces'} icon={Timer} disabled={!ns}>
          Schedules
        </NavItem>
        <NavItem to={ent ? `${ent}/batches` : '/namespaces'} icon={Activity} disabled={!ns}>
          Batch
        </NavItem>
        <NavItem to={ent ? `${ent}/deployments` : '/namespaces'} icon={Rocket} disabled={!ns}>
          Deployments
        </NavItem>
        <NavItem to={ent ? `${ent}/task-queues` : '/namespaces'} icon={ListChecks} disabled={!ns}>
          Task Queues
        </NavItem>
        <NavItem to={ent ? `${ent}/workers` : '/namespaces'} icon={Users} disabled={!ns}>
          Workers
        </NavItem>
        <NavItem to={ent ? `${ent}/nexus` : '/namespaces'} icon={Network} disabled={!ns}>
          Nexus
        </NavItem>

        <YStack my="$3" borderTopWidth={1} borderTopColor="$borderColor" />

        <NavItem to="/archive" icon={Archive} disabled>
          Archive
        </NavItem>
        <NavItem to="/import" icon={Upload} disabled>
          Import
        </NavItem>
        <NavItemExternal href="https://docs.hanzo.ai/tasks" icon={BookOpen}>
          Docs
        </NavItemExternal>
      </YStack>

      {/* Footer */}
      <YStack
        px="$3"
        py="$3"
        borderTopWidth={1}
        borderTopColor="$borderColor"
        gap="$1"
      >
        <a
          href="https://github.com/hanzoai/tasks/issues/new"
          target="_blank"
          rel="noopener noreferrer"
          style={{ textDecoration: 'none' }}
        >
          <XStack items="center" gap="$1.5">
            <Heart size={12} color="#7e8794" />
            <Text fontSize="$1" color="$placeholderColor">
              Feedback
            </Text>
          </XStack>
        </a>
        <Text fontSize={10} color="$placeholderColor" opacity={0.6}>
          v{APP_VERSION}
        </Text>
      </YStack>
    </YStack>
  )
}

interface NavItemProps {
  to: string
  icon: React.ComponentType<{ size?: number; color?: string }>
  children: React.ReactNode
  disabled?: boolean
  end?: boolean
}

function NavItem({ to, icon: Icon, children, disabled, end }: NavItemProps) {
  if (disabled) {
    return (
      <XStack
        items="center"
        gap="$2"
        px="$3"
        py="$2"
        rounded="$3"
        opacity={0.4}
        cursor="not-allowed"
      >
        <Icon size={16} color="#7e8794" />
        <Text fontSize="$2" color="$placeholderColor">
          {children}
        </Text>
      </XStack>
    )
  }
  return (
    <NavLink to={to} end={end} style={{ textDecoration: 'none' }}>
      {({ isActive }) => (
        <XStack
          items="center"
          gap="$2"
          px="$3"
          py="$2"
          rounded="$3"
          bg={isActive ? ('rgba(255,255,255,0.06)' as never) : 'transparent'}
          hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
        >
          <Icon size={16} color={isActive ? '#f2f2f2' : '#7e8794'} />
          <Text fontSize="$2" color={isActive ? '$color' : '$placeholderColor'}>
            {children}
          </Text>
        </XStack>
      )}
    </NavLink>
  )
}

function NavItemExternal({
  href,
  icon: Icon,
  children,
}: {
  href: string
  icon: React.ComponentType<{ size?: number; color?: string }>
  children: React.ReactNode
}) {
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" style={{ textDecoration: 'none' }}>
      <XStack
        items="center"
        gap="$2"
        px="$3"
        py="$2"
        rounded="$3"
        hoverStyle={{ background: 'rgba(255,255,255,0.04)' as never }}
      >
        <Icon size={16} color="#7e8794" />
        <Text fontSize="$2" color="$placeholderColor">
          {children}
        </Text>
      </XStack>
    </a>
  )
}
