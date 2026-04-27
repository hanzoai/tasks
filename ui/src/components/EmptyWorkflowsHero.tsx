import { Github } from 'lucide-react'

// EmptyWorkflowsHero is the structural twin of upstream's
// table-empty-state.svelte. Two columns: copy on the left, an
// SVG-built moon-and-sea illustration on the right. We do NOT
// bundle the upstream PNG (copyrighted) — the SVG below is hand-built
// from gradient + simple shapes.
//
// Sample-repo links remain pointed at github.com/temporalio because those
// are the canonical worker SDK examples.

const SAMPLES = [
  'samples-go',
  'samples-java',
  'samples-typescript',
  'samples-python',
  'samples-dotnet',
  'samples-php',
]

export function EmptyWorkflowsHero({ namespace }: { namespace: string }) {
  return (
    <div className="grid grid-cols-1 overflow-hidden rounded-md border border-border xl:grid-cols-[minmax(420px,1fr)_minmax(320px,1.1fr)]">
      <div className="flex flex-col gap-4 bg-card/40 p-8">
        <h2 className="text-lg font-semibold">
          No Workflows running in this Namespace
        </h2>
        <p className="text-sm leading-relaxed text-muted-foreground">
          You can populate the Web UI with sample Workflows. You can find a
          complete list of executable code samples at{' '}
          <a
            href="https://github.com/temporalio"
            className="text-foreground underline underline-offset-2 hover:text-primary"
            target="_blank"
            rel="noopener noreferrer"
          >
            github.com/temporalio
          </a>
          .
        </p>
        <ul className="flex flex-col gap-2">
          {SAMPLES.map((s) => (
            <li key={s}>
              <a
                href={`https://github.com/temporalio/${s}`}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-2 text-sm text-foreground hover:text-primary"
              >
                <Github size={14} className="shrink-0" />
                <span className="underline underline-offset-2">{s}</span>
              </a>
            </li>
          ))}
        </ul>
        <p className="pt-2 text-xs text-muted-foreground/70">
          Namespace: <code className="text-foreground">{namespace}</code>
        </p>
      </div>
      <div className="relative min-h-[280px]">
        <MoonSeascape />
      </div>
    </div>
  )
}

function MoonSeascape() {
  // Hand-built SVG: night sky gradient, full moon, low cloud bank,
  // distant mountain silhouette, water reflection. No external assets.
  return (
    <svg
      viewBox="0 0 600 400"
      preserveAspectRatio="xMidYMid slice"
      className="absolute inset-0 h-full w-full"
      aria-hidden="true"
    >
      <defs>
        <linearGradient id="sky" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#1e1b4b" />
          <stop offset="55%" stopColor="#2e1065" />
          <stop offset="100%" stopColor="#581c87" />
        </linearGradient>
        <linearGradient id="sea" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor="#312e81" />
          <stop offset="100%" stopColor="#0c0a35" />
        </linearGradient>
        <radialGradient id="moonGlow" cx="50%" cy="50%" r="50%">
          <stop offset="0%" stopColor="#fde68a" stopOpacity="0.4" />
          <stop offset="60%" stopColor="#fde68a" stopOpacity="0.05" />
          <stop offset="100%" stopColor="#fde68a" stopOpacity="0" />
        </radialGradient>
      </defs>

      {/* sky */}
      <rect x="0" y="0" width="600" height="240" fill="url(#sky)" />
      {/* moon glow */}
      <circle cx="420" cy="120" r="120" fill="url(#moonGlow)" />
      {/* moon */}
      <circle cx="420" cy="120" r="48" fill="#fef3c7" />
      <circle cx="408" cy="110" r="6" fill="#fde68a" opacity="0.4" />
      <circle cx="436" cy="130" r="4" fill="#fde68a" opacity="0.3" />

      {/* stars */}
      <g fill="#f3f4f6">
        <circle cx="60" cy="40" r="1" />
        <circle cx="120" cy="80" r="1.2" opacity="0.7" />
        <circle cx="200" cy="50" r="0.8" />
        <circle cx="260" cy="100" r="1" opacity="0.6" />
        <circle cx="540" cy="60" r="1.2" />
        <circle cx="560" cy="160" r="0.8" opacity="0.7" />
        <circle cx="320" cy="40" r="0.8" />
        <circle cx="500" cy="200" r="0.8" opacity="0.5" />
      </g>

      {/* clouds */}
      <g fill="#1e1b4b" opacity="0.55">
        <ellipse cx="120" cy="160" rx="80" ry="14" />
        <ellipse cx="180" cy="180" rx="100" ry="12" />
        <ellipse cx="500" cy="200" rx="70" ry="10" />
      </g>

      {/* mountains */}
      <path
        d="M 0 240 L 60 200 L 130 215 L 220 175 L 320 220 L 410 195 L 520 230 L 600 210 L 600 240 Z"
        fill="#0f172a"
      />

      {/* sea */}
      <rect x="0" y="240" width="600" height="160" fill="url(#sea)" />

      {/* moon reflection */}
      <ellipse cx="420" cy="246" rx="40" ry="3" fill="#fef3c7" opacity="0.3" />
      <ellipse cx="420" cy="260" rx="34" ry="2" fill="#fef3c7" opacity="0.2" />
      <ellipse cx="420" cy="276" rx="26" ry="1.5" fill="#fef3c7" opacity="0.12" />

      {/* water highlights */}
      <g stroke="#a5b4fc" strokeWidth="0.5" opacity="0.25" fill="none">
        <path d="M 0 290 Q 60 286 120 290 T 240 290 T 360 290 T 480 290 T 600 290" />
        <path d="M 0 320 Q 60 316 120 320 T 240 320 T 360 320 T 480 320 T 600 320" />
        <path d="M 0 350 Q 60 346 120 350 T 240 350 T 360 350 T 480 350 T 600 350" />
      </g>
    </svg>
  )
}
