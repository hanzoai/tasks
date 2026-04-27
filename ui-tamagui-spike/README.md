# Hanzo Tasks UI v2 (Tamagui Spike)

Single-page proof-of-concept showing the Tasks admin UI rebuilt on
`@hanzo/gui` (Tamagui, react-native-web target). Lives alongside the
production `../ui/` directory; once this proves out across all pages it
will replace v1 as tasksd's embedded SPA.

## Why this exists

`../ui/` is React DOM + Tailwind + shadcn primitives. It works and
ships with every tasksd binary. We want a future where the same code
also runs natively (iOS/Android) when the operator console grows a
mobile companion. Tamagui's compile-once / target-many model is the
shortest path. This spike validates that the toolchain works end-to-end
against the live `/v1/tasks/*` API before we commit to the port.

## Status

**Single-page proof of concept.** Workflows list only. v1 is still the
ship target — DO NOT POINT `../ui/embed.go` AT THIS YET.

What works:

- `pnpm dev` launches Vite on `http://localhost:5174/_/tasks/` with the
  Workflows table populated from a live `tasksd` at `127.0.0.1:7243`.
- `pnpm build` emits a static `dist/` (single JS chunk + index.html)
  ready to be `go:embed`ed once feature parity lands.
- Realtime: SSE subscription to `/v1/tasks/events` re-fetches the table
  on `workflow.{started,canceled,terminated,signaled}`.
- Strict TypeScript passes with no `any`s in our code.
- React 19 + Tamagui v102 + react-native-web 0.21.

## Run

```bash
# Start tasksd in another terminal — see ../README.md.
# tasksd defaults to HTTP :7243; vite proxies /v1/tasks → 7243.

pnpm install
pnpm dev      # http://localhost:5174/_/tasks/
pnpm build    # → dist/
pnpm preview  # serve dist/ for a sanity check
```

## Bundle size (honest measurement)

| | v1 (`../ui/dist/`) | v2 (this spike) |
|---|---|---|
| JS  | 338 KB | **446 KB** |
| CSS | 24 KB  | 0 KB (CSS-in-JS at runtime) |
| Total | 363 KB | 446 KB |
| Gzip JS | ~110 KB | **139 KB** |

v2 is 23% larger. Tamagui's static CSS extractor (`@hanzogui/static`)
*should* shrink it ~30% by hoisting style props to atomic class names,
but it currently fails outside the gui workspace because its bundled-
config worker can't resolve `@hanzogui/core` from the temp `.hanzogui/`
directory it writes to. Reported limitation; not a v2 blocker. We
ship `disable: true` in `vite.config.ts` so build logs stay clean.

## Files

| Path | Role |
|---|---|
| `package.json` | pnpm manifest pinning published `hanzogui@102` and React 19 |
| `tsconfig.json` | strict TS, bundler resolution, JSX react-jsx |
| `vite.config.ts` | Vite 8 + `@vitejs/plugin-react` + `hanzoguiPlugin`; aliases `react-native` → `react-native-web`; proxies `/v1/tasks` → `127.0.0.1:7243`; serves under `/_/tasks/` to match v1's embed path |
| `hanzogui.config.ts` | extends `@hanzogui/config`'s v5 default with the Hanzo dark palette mirrored from `../ui/src/index.css` |
| `index.html` | SPA shell, dark `<body>`, Inter font preconnect |
| `src/main.tsx` | React root |
| `src/App.tsx` | mounts `HanzoguiProvider` + `WorkflowsPage` |
| `src/pages/Workflows.tsx` | the page — header, table, status badges, empty state |
| `src/lib/api.ts` | fetch wrapper, `ApiError`, `WorkflowExecution` type |
| `src/lib/useFetch.ts` | tiny SWR replacement (no extra dep) |
| `src/lib/events.ts` | SSE subscription against `/v1/tasks/events` |
| `public/favicon.svg` | the 'H' mark |

## What's missing before v2 can replace v1

Each item is one Svelte source path or v1 React page that needs a
Tamagui twin. Order is rough porting sequence — chrome → list pages →
detail pages → polish.

- [ ] **App chrome**: top nav, namespace switcher, sidebar, route shell
      (currently the spike hardcodes `namespace=default` and renders
      one route)
- [ ] **Routing**: `react-router` or `expo-router` — pick once we
      decide if iOS lands in the same target
- [ ] **`Namespaces`** list page (`../ui/src/pages/Namespaces.tsx`)
- [ ] **`NamespaceDetail`** (`../ui/src/pages/NamespaceDetail.tsx`)
- [ ] **`Schedules`** (`../ui/src/pages/Schedules.tsx`)
- [ ] **`Batches`** (`../ui/src/pages/Batches.tsx`)
- [ ] **`Deployments`** (`../ui/src/pages/Deployments.tsx`)
- [ ] **`Nexus`** (`../ui/src/pages/Nexus.tsx`)
- [ ] **`WorkflowDetail`** (`../ui/src/pages/WorkflowDetail.tsx`) —
      tabs (input, history, pending), event timeline, JSON pretty-print,
      cancel/terminate dialogs
- [ ] **`Support`** (`../ui/src/pages/Support.tsx`)
- [ ] **Workflows page parity**: `SavedViewsRail`, `FilterBar`,
      `EmptyWorkflowsHero`, "Start Workflow" dialog, `LocalTimeIndicator`
- [ ] **Tamagui static CSS extractor**: figure out the bundled-config
      resolution (or move into `~/work/hanzo/gui` workspace) so we
      reclaim the 30% bundle savings before cutover
- [ ] **`go:embed` switch**: update `../ui/embed.go` once feature
      parity lands; v1 dist/ becomes the legacy fallback

## Tradeoffs

- **Plain Vite over `@hanzogui/expo-router-starter` and `…/remix`**:
  the starters live inside the gui bun workspace and cross-link via
  `workspace:*` to ~30 internal packages. Pulling them into a
  standalone repo would require either copying every workspace dep or
  wiring this directory into the gui monorepo. Vite + published
  Tamagui packages keeps this spike self-contained and matches v1's
  toolchain so the repo only has one bundler.

- **Custom `useFetch` instead of SWR**: 30 lines, no extra runtime.
  Real SWR can come back when we port pages that need shared cache —
  this one doesn't.

- **Hardcoded `namespace="default"`**: the namespace switcher is part
  of the chrome that hasn't been ported yet. Wiring a router for one
  page is yak-shaving.

- **CSS values for status badges**: `rgba(...)` cast to `as never` to
  bypass Tamagui's strict `ColorTokens` generic. Runtime accepts any
  CSS color; the published types are stricter than the runtime.

- **Long-form `flex` instead of `f`**: the v5 shorthand mapping has
  no `f` — only `p/px/py/m/mx/my/items/justify/self/bg/rounded/text`.
  React-Native style names like `flex`, `width`, `height`,
  `borderTopWidth`, `borderBottomWidth`, `borderColor`, `opacity`,
  `numberOfLines` are the canonical form for those.
