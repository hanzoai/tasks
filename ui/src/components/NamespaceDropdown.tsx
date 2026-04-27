import useSWR from 'swr'
import { useNavigate } from 'react-router-dom'
import { ChevronDown, ExternalLink } from 'lucide-react'
import { Popover, PopoverItem } from './ui/dropdown-menu'
import type { Namespace } from '../lib/api'

// NamespaceDropdown is the top-bar selector mirroring Temporal's
// namespace switcher. The "external link" learn-more icon points at
// the upstream cluster-deployment-guide because the ns model is
// 1:1 — a JWT-authenticated org's namespaces are listed under this
// menu, click switches the URL.

export function NamespaceDropdown({ activeNs }: { activeNs?: string }) {
  const navigate = useNavigate()
  const { data } = useSWR<{ namespaces: Namespace[] }>('/v1/tasks/namespaces?pageSize=200')
  const all = data?.namespaces ?? []

  return (
    <div className="inline-flex items-center gap-1.5 text-sm">
      <Popover
        align="start"
        trigger={
          <button
            type="button"
            className="inline-flex items-center gap-1.5 rounded-md border border-border bg-card/40 px-2.5 py-1.5 text-foreground transition-colors hover:bg-accent/50"
          >
            <span className="font-medium">{activeNs ?? 'default'}</span>
            <ChevronDown size={14} className="text-muted-foreground" />
          </button>
        }
      >
        <div className="px-2 py-1.5 text-xs font-medium text-muted-foreground">Switch namespace</div>
        <div className="my-1 border-t border-border" />
        {all.length === 0 ? (
          <PopoverItem disabled className="opacity-60">No namespaces</PopoverItem>
        ) : (
          all.map((ns) => {
            const name = ns.namespaceInfo.name
            return (
              <PopoverItem
                key={name}
                onClick={() => navigate(`/namespaces/${encodeURIComponent(name)}/workflows`)}
              >
                <span className={name === activeNs ? 'font-medium' : ''}>{name}</span>
              </PopoverItem>
            )
          })
        )}
        <div className="my-1 border-t border-border" />
        <PopoverItem onClick={() => navigate('/namespaces')}>All namespaces…</PopoverItem>
      </Popover>
      <a
        href="https://docs.temporal.io/cluster-deployment-guide#namespaces"
        target="_blank"
        rel="noopener noreferrer"
        className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground"
        aria-label="Learn about namespaces"
        title="Learn about namespaces"
      >
        <ExternalLink size={14} />
      </a>
    </div>
  )
}
