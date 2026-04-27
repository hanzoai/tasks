import { LogOut, User } from 'lucide-react'
import { Popover, PopoverItem } from './ui/dropdown-menu'

// Reads identity headers populated by hanzoai/gateway after IAM JWT
// validation (X-User-Email, X-User-Id, X-Org-Id). When run locally
// without a gateway, falls back to a placeholder.
//
// In a browser context we can't read X-* response headers reliably
// for the page itself, so the chip currently shows a static label.
// When IAM SSO lands, this reads /v1/tasks/me which the server
// echoes from request headers.

export function AccountChip() {
  const initials = 'HZ'
  return (
    <Popover
      align="end"
      trigger={
        <button
          className="inline-flex h-8 w-8 items-center justify-center rounded-full border border-border bg-primary text-xs font-semibold text-primary-foreground hover:opacity-90"
          aria-label="Account"
        >
          {initials}
        </button>
      }
    >
      <div className="px-2 py-1.5 text-xs text-muted-foreground">
        <p className="font-medium text-foreground">Hanzo</p>
        <p>local · embedded</p>
      </div>
      <div className="my-1 border-t border-border" />
      <PopoverItem onClick={() => window.open('/v1/tasks/health', '_blank')}>
        <User size={14} />
        Identity
      </PopoverItem>
      <PopoverItem onClick={() => alert('Sign out is wired through hanzoai/gateway IAM session.')}>
        <LogOut size={14} />
        Sign out
      </PopoverItem>
    </Popover>
  )
}
