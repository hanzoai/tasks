import { NavLink, Outlet, useParams } from 'react-router-dom'
import {
  Activity,
  Boxes,
  Clock,
  Layers,
  LifeBuoy,
  Network,
  Rocket,
  Workflow,
} from 'lucide-react'
import { cn } from './lib/utils'
import { UTCClock } from './components/UTCClock'
import { ThemeToggle } from './components/ThemeToggle'
import { AccountChip } from './components/AccountChip'
import { Badge } from './components/ui/badge'

export function App() {
  const { ns } = useParams()
  return (
    <div className="min-h-screen flex bg-background text-foreground">
      <Sidebar ns={ns} />
      <div className="flex-1 flex flex-col min-w-0">
        <TopBar ns={ns} />
        <main className="flex-1 px-8 py-6 overflow-auto">
          <Outlet />
        </main>
      </div>
    </div>
  )
}

function Sidebar({ ns }: { ns?: string }) {
  const ent = ns ? `/namespaces/${encodeURIComponent(ns)}` : ''
  return (
    <aside className="w-56 shrink-0 border-r border-border bg-card/40 flex flex-col">
      <div className="px-5 py-4 border-b border-border">
        <div className="flex items-center gap-2">
          <div className="size-7 rounded-md bg-primary text-primary-foreground inline-flex items-center justify-center font-bold text-sm">
            H
          </div>
          <span className="font-semibold tracking-tight">Tasks</span>
        </div>
      </div>
      <nav className="flex-1 p-2 space-y-0.5">
        <NavItem to={ent ? `${ent}/workflows` : '/namespaces'} icon={Workflow} disabled={!ns}>
          Workflows
        </NavItem>
        <NavItem to={ent ? `${ent}/schedules` : '/namespaces'} icon={Clock} disabled={!ns}>
          Schedules
        </NavItem>
        <NavItem to={ent ? `${ent}/batches` : '/namespaces'} icon={Activity} disabled={!ns}>
          Batches
        </NavItem>
        <NavItem to={ent ? `${ent}/deployments` : '/namespaces'} icon={Rocket} disabled={!ns}>
          Deployments
        </NavItem>
        <NavItem to="/namespaces" icon={Layers}>
          Namespaces
        </NavItem>
        <NavItem to={ent ? `${ent}/nexus` : '/namespaces'} icon={Network} disabled={!ns}>
          Nexus
        </NavItem>
        <div className="my-2 border-t border-border" />
        <NavItem to="/support" icon={LifeBuoy}>
          Support
        </NavItem>
      </nav>
      <div className="px-3 py-3 border-t border-border text-xs text-muted-foreground space-y-1">
        <div className="flex items-center gap-1.5">
          <Boxes size={12} />
          <span>Native ZAP · :9999</span>
        </div>
        <div className="opacity-70">v1.38</div>
      </div>
    </aside>
  )
}

function NavItem({
  to,
  icon: Icon,
  children,
  disabled,
}: {
  to: string
  icon: React.ComponentType<any>
  children: React.ReactNode
  disabled?: boolean
}) {
  if (disabled) {
    return (
      <span
        className="flex items-center gap-2.5 px-3 py-2 rounded-md text-sm text-muted-foreground/40 cursor-not-allowed select-none"
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
      end={to === '/namespaces' || to === '/support'}
      className={({ isActive }) =>
        cn(
          'flex items-center gap-2.5 px-3 py-2 rounded-md text-sm transition-colors',
          isActive
            ? 'bg-accent text-accent-foreground'
            : 'text-muted-foreground hover:text-foreground hover:bg-accent/50'
        )
      }
    >
      <Icon size={16} />
      {children}
    </NavLink>
  )
}

function TopBar({ ns }: { ns?: string }) {
  return (
    <header className="border-b border-border bg-card/40 backdrop-blur supports-[backdrop-filter]:bg-card/30 px-6 h-14 flex items-center gap-4">
      {ns ? (
        <span className="text-sm">
          <span className="text-muted-foreground">ns:</span>{' '}
          <code className="text-foreground font-medium">{ns}</code>
        </span>
      ) : (
        <span className="text-sm text-muted-foreground">All namespaces</span>
      )}
      <Badge variant="muted" className="font-normal">embedded</Badge>
      <div className="ml-auto flex items-center gap-3">
        <UTCClock />
        <ThemeToggle />
        <AccountChip />
      </div>
    </header>
  )
}
