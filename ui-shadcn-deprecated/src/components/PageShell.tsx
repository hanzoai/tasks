import type { ReactNode } from 'react'

// PageShell adds the standard page padding for surfaces that don't
// own their own scroll layout (Workflows uses a sidebar+empty hero,
// so it bypasses this).

export function PageShell({ children }: { children: ReactNode }) {
  return <div className="px-8 py-6">{children}</div>
}
