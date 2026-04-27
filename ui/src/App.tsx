import { NavLink, Outlet, useParams } from 'react-router-dom'
import {
  Activity,
  Archive,
  BookOpen,
  Heart,
  Layers,
  Network,
  Rocket,
  Timer,
  Upload,
  Workflow,
} from 'lucide-react'
import { cn } from './lib/utils'
import { ThemeToggle } from './components/ThemeToggle'
import { AccountChip } from './components/AccountChip'
import { NamespaceDropdown } from './components/NamespaceDropdown'
import { LocalTimeIndicator } from './components/LocalTimeIndicator'
import { ArchiveToggle } from './components/ArchiveToggle'

// App shell mirrors Temporal's holocene/navigation pattern:
//   • Top group: navigable surfaces (Namespaces, Workflows, Schedules, …)
//   • Bottom group: archive/import/docs (separated by a divider)
//   • Footer: Feedback link + version
// The Hanzo H replaces the Temporal logo. Sample SDK links remain
// pointed at github.com/temporalio per the brief.
//
// Version comes from VITE_APP_VERSION at build time (defaulted in vite.config.ts).

const APP_VERSION = (import.meta as any).env?.VITE_APP_VERSION ?? '2.45.3'

export function App() {
  const { ns } = useParams()
  return (
    <div className="min-h-screen flex bg-background text-foreground">
      <Sidebar ns={ns} />
      <div className="flex-1 flex flex-col min-w-0">
        <TopBar ns={ns} />
        <main className="flex-1 overflow-auto">
          <Outlet />
        </main>
      </div>
    </div>
  )
}

function Sidebar({ ns }: { ns?: string }) {
  const ent = ns ? `/namespaces/${encodeURIComponent(ns)}` : ''
  return (
    <aside className="flex w-56 shrink-0 flex-col border-r border-border bg-card/40">
      <div className="border-b border-border px-4 py-3">
        <div className="flex items-center gap-2">
          <HanzoMark />
          <div className="flex flex-col leading-tight">
            <span className="text-sm font-semibold tracking-tight">Hanzo Tasks</span>
            <span className="text-[10px] uppercase tracking-wider text-muted-foreground">
              Self-Hosted
            </span>
          </div>
        </div>
      </div>

      <nav className="flex-1 space-y-0.5 p-2">
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
        <NavItem to={ent ? `${ent}/nexus` : '/namespaces'} icon={Network} disabled={!ns}>
          Nexus
        </NavItem>

        <div className="my-3 border-t border-border" />

        <NavItem to="/archive" icon={Archive} disabled>
          Archive
        </NavItem>
        <NavItem to="/import" icon={Upload} disabled>
          Import
        </NavItem>
        <NavItemExternal href="https://docs.temporal.io" icon={BookOpen}>
          Docs
        </NavItemExternal>
      </nav>

      <div className="border-t border-border px-3 py-3 text-xs">
        <a
          href="https://github.com/hanzoai/tasks/issues/new"
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1.5 text-muted-foreground transition-colors hover:text-foreground"
        >
          <Heart size={12} />
          Feedback
        </a>
        <p className="mt-1 text-[11px] text-muted-foreground/60">v{APP_VERSION}</p>
      </div>
    </aside>
  )
}

function HanzoMark() {
  return (
    <span
      className="inline-flex size-7 shrink-0 items-center justify-center rounded-md bg-foreground text-background"
      aria-label="Hanzo"
    >
      <svg viewBox="0 0 24 24" className="h-4 w-4" fill="currentColor" aria-hidden="true">
        <path d="M4 3 H7 V10 H17 V3 H20 V21 H17 V13 H7 V21 H4 Z" />
      </svg>
    </span>
  )
}

function NavItem({
  to,
  icon: Icon,
  children,
  disabled,
  end,
}: {
  to: string
  icon: React.ComponentType<any>
  children: React.ReactNode
  disabled?: boolean
  end?: boolean
}) {
  if (disabled) {
    return (
      <span
        className="flex cursor-not-allowed select-none items-center gap-2.5 rounded-md px-3 py-2 text-sm text-muted-foreground/40"
        aria-disabled
      >
        <Icon size={16} />
        {children}
      </span>
    )
  }
  return (
    <NavLink
      to={to}
      end={end}
      className={({ isActive }) =>
        cn(
          'flex items-center gap-2.5 rounded-md px-3 py-2 text-sm transition-colors',
          isActive
            ? 'bg-accent text-accent-foreground'
            : 'text-muted-foreground hover:bg-accent/50 hover:text-foreground'
        )
      }
    >
      <Icon size={16} />
      {children}
    </NavLink>
  )
}

function NavItemExternal({
  href,
  icon: Icon,
  children,
}: {
  href: string
  icon: React.ComponentType<any>
  children: React.ReactNode
}) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="flex items-center gap-2.5 rounded-md px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground"
    >
      <Icon size={16} />
      {children}
    </a>
  )
}

function TopBar({ ns }: { ns?: string }) {
  return (
    <header className="flex h-14 items-center gap-4 border-b border-border bg-card/40 px-6 backdrop-blur supports-[backdrop-filter]:bg-card/30">
      <NamespaceDropdown activeNs={ns} />
      <div className="ml-auto flex items-center gap-2">
        <LocalTimeIndicator />
        <ArchiveToggle />
        <ThemeToggle />
        <AccountChip />
      </div>
    </header>
  )
}
