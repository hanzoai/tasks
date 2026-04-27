// App shell — wires the @hanzogui/admin chrome (AdminApp + Sidebar +
// TopBar) with tasks-specific config: the nav items, the brand mark,
// and the namespace switcher backed by /v1/tasks/namespaces.
//
// The route tree lives in main.tsx so this file only owns config.

import { Outlet, useParams } from 'react-router-dom'
import {
  AccountChip,
  AdminApp,
  HanzoMark,
  LocalTimeIndicator,
  NamespaceSwitcher,
  Sidebar,
  ThemeToggle,
  TopBar,
  useFetch,
  type SidebarConfig,
} from '@hanzogui/admin'
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
import type { Namespace } from './lib/api'

const APP_VERSION = (import.meta as any).env?.VITE_APP_VERSION ?? '2.45.3'

function buildSidebarConfig(ns?: string): SidebarConfig {
  const ent = ns ? `/namespaces/${encodeURIComponent(ns)}` : ''
  return {
    brand: {
      mark: <HanzoMark />,
      title: 'Hanzo Tasks',
      subtitle: 'Self-Hosted',
    },
    sections: [
      {
        items: [
          { to: '/namespaces', icon: Layers, label: 'Namespaces', end: true },
          { to: ent ? `${ent}/workflows` : '/namespaces', icon: Workflow, label: 'Workflows', disabled: !ns },
          { to: ent ? `${ent}/schedules` : '/namespaces', icon: Timer, label: 'Schedules', disabled: !ns },
          { to: ent ? `${ent}/batches` : '/namespaces', icon: Activity, label: 'Batch', disabled: !ns },
          { to: ent ? `${ent}/deployments` : '/namespaces', icon: Rocket, label: 'Deployments', disabled: !ns },
          { to: ent ? `${ent}/task-queues` : '/namespaces', icon: ListChecks, label: 'Task Queues', disabled: !ns },
          { to: ent ? `${ent}/workers` : '/namespaces', icon: Users, label: 'Workers', disabled: !ns },
          { to: ent ? `${ent}/nexus` : '/namespaces', icon: Network, label: 'Nexus', disabled: !ns },
        ],
      },
      {
        items: [
          { to: '/archive', icon: Archive, label: 'Archive', disabled: true },
          { to: '/import', icon: Upload, label: 'Import', disabled: true },
          { href: 'https://docs.hanzo.ai/tasks', icon: BookOpen, label: 'Docs' },
        ],
      },
    ],
    footer: {
      feedback: {
        href: 'https://github.com/hanzoai/tasks/issues/new',
        label: 'Feedback',
        icon: Heart,
      },
      version: `v${APP_VERSION}`,
    },
  }
}

function TasksTopBar({ ns }: { ns?: string }) {
  const { data } = useFetch<{ namespaces: Namespace[] }>('/v1/tasks/namespaces?pageSize=200')
  const options = (data?.namespaces ?? []).map((n) => ({
    id: n.namespaceInfo.name,
    label: n.namespaceInfo.name,
  }))
  return (
    <TopBar
      left={
        <NamespaceSwitcher
          active={ns}
          options={options}
          hrefFor={(id) => `/namespaces/${encodeURIComponent(id)}/workflows`}
          allHref="/namespaces"
          docsHref="https://docs.hanzo.ai/tasks#namespaces"
          groupLabel="Switch namespace"
        />
      }
      right={
        <>
          <LocalTimeIndicator />
          <ThemeToggle storageKey="tasks.theme" />
          <AccountChip
            initials="HZ"
            name="Hanzo"
            subtitle="local · embedded"
            identityHref="/v1/tasks/health"
            onSignOut={() => alert('Sign out is wired through hanzoai/gateway IAM session.')}
          />
        </>
      }
    />
  )
}

export default function App() {
  const { ns } = useParams()
  return (
    <AdminApp sidebar={<Sidebar config={buildSidebarConfig(ns)} />} topBar={<TasksTopBar ns={ns} />}>
      <Outlet />
    </AdminApp>
  )
}
