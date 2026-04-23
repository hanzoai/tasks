import { NavLink, Outlet, useParams } from 'react-router-dom'
import { Clock, Layers, Workflow } from 'lucide-react'

export function App() {
  const { ns } = useParams()
  const nsPrefix = ns ? `/namespaces/${encodeURIComponent(ns)}` : ''
  return (
    <div className="min-h-screen flex flex-col bg-zinc-950 text-zinc-100">
      <header className="border-b border-zinc-800 bg-zinc-900/80 backdrop-blur px-6 py-3 flex items-center gap-6">
        <h1 className="font-semibold tracking-tight">Hanzo Tasks</h1>
        <nav className="flex items-center gap-1 text-sm">
          <Tab to="/namespaces" icon={<Layers size={16} />}>Namespaces</Tab>
          {ns && (
            <>
              <Tab to={`${nsPrefix}/workflows`} icon={<Workflow size={16} />}>Workflows</Tab>
              <Tab to={`${nsPrefix}/schedules`} icon={<Clock size={16} />}>Schedules</Tab>
            </>
          )}
        </nav>
        <div className="ml-auto text-xs text-zinc-400">
          {ns ? <span>ns: <code className="text-zinc-200">{ns}</code></span> : null}
        </div>
      </header>
      <main className="flex-1 p-6 max-w-6xl w-full mx-auto">
        <Outlet />
      </main>
    </div>
  )
}

function Tab({ to, icon, children }: { to: string; icon: React.ReactNode; children: React.ReactNode }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        `inline-flex items-center gap-1.5 px-3 py-1.5 rounded-md transition ${
          isActive ? 'bg-zinc-800 text-white' : 'text-zinc-400 hover:text-zinc-100 hover:bg-zinc-800/50'
        }`
      }
    >
      {icon}
      {children}
    </NavLink>
  )
}
