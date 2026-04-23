// Neutral status pill — 4 kinds, no external css deps, no animations.
// Matches Tailwind zinc palette for dark-first rendering.

type Kind = 'ok' | 'warn' | 'error' | 'muted'

const styles: Record<Kind, string> = {
  ok: 'bg-emerald-950/50 text-emerald-300 border border-emerald-900/60',
  warn: 'bg-amber-950/50 text-amber-300 border border-amber-900/60',
  error: 'bg-red-950/50 text-red-300 border border-red-900/60',
  muted: 'bg-zinc-800/60 text-zinc-400 border border-zinc-700/60',
}

export function StatusPill({ kind, children }: { kind: Kind; children: React.ReactNode }) {
  return (
    <span className={`inline-flex items-center text-xs px-2 py-0.5 rounded-full font-medium ${styles[kind]}`}>
      {children}
    </span>
  )
}
