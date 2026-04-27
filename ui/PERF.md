# Tasks UI v2 (Tamagui) — Performance & Bundle Audit

Investigation of `~/work/hanzo/tasks/ui-tamagui-spike/` (the Tamagui rebuild
that will replace this v1 React/shadcn directory). All measurements taken
2026-04-27, vite 8.0.10 + rolldown, hanzogui 102.0.0-rc.41-hanzoai.1,
@hanzogui/lucide-icons-2 7.0.0, react 19.2.5, target es2020, minified
production build.

Hardware: MacBook (Darwin 25.4.0, arm64). Live tasksd at 127.0.0.1:7243.

---

## 0. Reality Check (Before Investigations)

The user prompt frames `tasks/ui/` as the Tamagui v2 build with a
1.51 MB / 297 KB-gzip bundle. That is wrong on two counts and matters
for every recommendation below:

1. **Tasks/ui is still v1** (React 18 + shadcn + Tailwind + lucide-react).
   The Tamagui spike lives at `tasks/ui-tamagui-spike/` and the `tasksd`
   binary embeds it (`embed.go`). The live server at 127.0.0.1:7243 is
   currently serving the v1 bundle (`index-DQHl_ST6.js`, 338,668 raw /
   103,681 gzip).
2. **The 1.51 MB / 297 KB number is the v2 bundle from
   `ui-tamagui-spike/`** with the canonical `vite.config.ts`. Reproduced
   exactly: `1,510,491 raw / 296,810 gzip` — `dist/assets/index-BIs8P6ul.js`
   (or the equivalent hash after a fresh build).

So the perf gap the dev team is chasing is "v1 React/shadcn at 105 KB
gzip → v2 Tamagui at 297 KB gzip", a 2.8x regression. Recommendations
1–3 close that gap and land below the 200 KB target.

---

## 1. Bundle Composition

### Method
```bash
cd ~/work/hanzo/tasks/ui-tamagui-spike
pnpm add -D rollup-plugin-visualizer  # 7.0.1
# Add visualizer({ filename: '/tmp/perf-bundle-stats.html', gzipSize: true,
# template: 'treemap', emitFile: false, sourcemap: false }) into vite.config.ts
npx vite build
# Parse the HTML data block: see /tmp/parse-bundle2.py in the artifacts
```

### Results

Production build, 2,759 modules transformed → 1,510,491 bytes raw
JS / 294,962 bytes gzip / 1,332,364 bytes total reported by visualizer
(pre-mangle, so does not match the final bundle 1:1).

#### Top-10 packages by raw size (before final mangling)

| # | Package | Raw bytes | Gzip bytes | %raw | %gzip | Modules |
|---|---|---|---|---|---|---|
| 1 | `@hanzogui/lucide-icons-2` | 1,966,600 | 888,050 | **55.1%** | **66.7%** | 1,761 |
| 2 | `react-dom` | 459,104 | 87,023 | 12.9% | 6.5% | 4 |
| 3 | `@hanzogui/web` | 312,044 | 113,530 | 8.7% | 8.5% | 139 |
| 4 | `react-native-web` | 130,221 | 40,642 | 3.6% | 3.1% | 55 |
| 5 | `<app:src>` (this repo) | 116,852 | 30,675 | 3.3% | 2.3% | 29 |
| 6 | `@hanzogui/themes` | 106,197 | 17,969 | 3.0% | 1.3% | 3 |
| 7 | `react-router` | 91,180 | 22,468 | 2.6% | 1.7% | 1 |
| 8 | `@hanzogui/floating` | 32,554 | 10,047 | 0.9% | 0.8% | 17 |
| 9 | `@hanzogui/popover` | 25,065 | 6,067 | 0.7% | 0.5% | 2 |
| 10 | `@hanzogui/use-element-layout` | 20,417 | 5,528 | 0.6% | 0.4% | 2 |

#### Family rollup

| Family | Raw | Gzip | %raw | %gzip | Packages |
|---|---|---|---|---|---|
| `lucide-icons-2` | 1,966,600 | 888,050 | 55.1% | 66.7% | 1 |
| `hanzogui` primitives (54 deep deps incl. @hanzogui/web/themes/floating/...) | 683,355 | 221,701 | 19.1% | 16.6% | 59 |
| `react`+`react-dom`+`scheduler` | 484,826 | 94,394 | 13.6% | 7.1% | 3 |
| `react-native-web` | 141,088 | 43,670 | 4.0% | 3.3% | 2 |
| `app-source` | 116,852 | 30,675 | 3.3% | 2.3% | 1 |
| `react-router` | 91,180 | 22,468 | 2.6% | 1.7% | 1 |
| `floating-ui` | 46,874 | 12,047 | 1.3% | 0.9% | 4 |
| `hanzogui` umbrella re-export | 1,215 | 820 | <0.1% | 0.1% | 1 |
| other deps | 32,075 | 15,394 | 0.9% | 1.2% | 7 |

### Findings
- **Single bundle. No code-splitting.** `dist/` ships exactly one JS
  file. Every byte of every page lands on the user before they see anything.
- **Lucide alone is 55% raw / 67% gzip.** All 1,761 icons are pulled by
  the umbrella import `from '@hanzogui/lucide-icons-2'`, even though only
  32 are actually used (see §2). The package exposes per-icon entry
  points (`./icons/Activity` etc.), so this is fixable in userland.
- **react-native-web is small** (~3% gzip). Not the bloat villain it is
  often blamed for. Removing it would be hostile to the Tamagui story
  for limited gain.
- **hanzogui umbrella is fine.** The umbrella module itself is 1.2 KB
  raw — it just re-exports. The 19% gzip share is real primitive code
  (`@hanzogui/web` 113 KB gzip, `@hanzogui/floating` 10 KB, etc.) that
  is actually used.

---

## 2. Tree-Shake Leakage from `hanzogui` Umbrella

### Method
```bash
cd ~/work/hanzo/tasks/ui-tamagui-spike
# Every named import from the umbrella:
grep -rh "from 'hanzogui'" src/                                    # single-line
python3 -c "extract multiline regex \"import\s*\{([^}]+)\}\s*from\s*'hanzogui'\""

# Every lucide icon imported:
grep -rh "from '@hanzogui/lucide-icons-2'" src/
```

### Results

#### `hanzogui` umbrella (16 distinct primitives used)
```
Button, Card, Dialog, H1, H2, H3, H4, HanzoguiProvider, Input,
Paragraph, Popover, Spinner, Tabs, Text, XStack, YStack
```

Top primitive packages already pulled (after tree-shake): `@hanzogui/web`
113 KB gzip, `@hanzogui/themes` 18 KB, `@hanzogui/floating` 10 KB. The
umbrella is **already tree-shaking effectively** — only the modules
backing the 16 used primitives are in the bundle. Re-routing through
deep imports (`from '@hanzogui/button'`) saves nothing measurable.

**Estimated savings: 0 KB (within noise).**

#### `@hanzogui/lucide-icons-2` umbrella (32 icons used, 1,761 shipped)

Used: `Activity, Archive, BookOpen, Check, ChevronDown, ChevronLeft,
ChevronRight, Circle, Clock, Copy, ExternalLink, Github, Globe, Heart,
History, Layers, ListChecks, LogOut, MessageSquare, Moon, Network,
Play, Plug, Plus, RefreshCw, Rocket, Sun, Timer, Upload, User, Users,
Workflow` — 32 icons.

Shipped: `1,761` icon modules. The umbrella `dist/esm/index.mjs`
imports every single icon from `./icons/*.mjs` and re-exports them.
Rolldown 4.23 + Vite 8 do not tree-shake this graph because each
icon's `themed(memo(...))` wrapper has observable side-effects.

#### Empirical fix: deep-import shim

```ts
// src/lib/icons-deep.ts
export { Activity } from '@hanzogui/lucide-icons-2/icons/Activity'
export { Plus } from '@hanzogui/lucide-icons-2/icons/Plus'
// ... 30 more
```

Then `sed -i` swap `from '@hanzogui/lucide-icons-2'` →
`from '../lib/icons-deep'` across all 14 consumers and rebuild.

| Build | Raw JS | Gzip JS | Δ raw | Δ gzip |
|---|---|---|---|---|
| Before (umbrella) | 1,510,491 | 296,810 | — | — |
| After (deep-import shim) | 727,189 | 224,123 | **−783,302 (−51.9%)** | **−72,687 (−24.5%)** |

**Savings: 783 KB raw / 73 KB gzip.** This is the single biggest win
in the codebase. The shim is one new file (32 lines) and a one-line
sed across 14 callers.

---

## 3. Code-Splitting Opportunity

### Method
```bash
# Replace static page imports in src/main.tsx with React.lazy(() => import(...))
# Keep WorkflowsPage sync (most-hit), lazy-load 13 of 14 pages
# Wrap each lazy route in <Suspense fallback={<Spinner />}>
npx vite build  # rolldown auto-emits per-import chunks
```

### Results

#### Lazy alone (13 of 14 pages lazy)

```
dist/assets/index-BnQ2Gr_I.js          1,373,654 raw │ 254,978 gzip   ← initial route
dist/assets/Text-BWP-32X5.js              92,914 raw │  34,810 gzip   ← shared chunk (text+heading primitives)
dist/assets/esm-DUeHHtRK.js               10,387 raw │   4,063 gzip
dist/assets/WorkflowDetail-BbEi8WH7.js     6,675 raw │   2,148 gzip
dist/assets/NamespaceDetail-B_NlA6AX.js    6,395 raw │   2,071 gzip
dist/assets/Batches-DdfRRBXd.js            5,323 raw │   1,899 gzip
dist/assets/TaskQueueDetail-DV8JJCIR.js    3,938 raw │   1,460 gzip
dist/assets/WorkflowHistory-Oo6KKEKQ.js    3,041 raw │   1,259 gzip
dist/assets/TaskQueues-CGJKWDgM.js         2,510 raw │   1,007 gzip
dist/assets/EventDetail-xw-tAzVo.js        2,477 raw │     989 gzip
dist/assets/Deployments-CSV9dFzo.js        2,071 raw │     891 gzip
dist/assets/Namespaces-CO4V03wg.js         1,872 raw │     937 gzip
dist/assets/Schedules-Ow0FBmF4.js          1,521 raw │     802 gzip
dist/assets/Nexus-CSh4hrJc.js              1,448 raw │     752 gzip
dist/assets/Support-Df02kDfG.js            1,438 raw │     701 gzip
dist/assets/Workers-u4cMxLp3.js            1,068 raw │     631 gzip
```

Initial chunk: **1,373,654 raw / 254,978 gzip** — saves 137 KB raw /
42 KB gzip vs single-bundle baseline.

#### Lazy + deep-import icons (combined)

```
dist/assets/index-BWDf0xZW.js            590,388 raw │ 182,693 gzip   ← initial route
dist/assets/Text-BWP-32X5.js              92,914 raw │  34,810 gzip
... (13 lazy chunks, each ≤7 KB raw / ≤2.2 KB gzip)
```

Initial chunk: **590,388 raw / 182,693 gzip** — under the 200 KB
target. Total shipped on first paint = `index + Text shared chunk =
683 KB raw / 217 KB gzip`. After visiting Workflows, no further
chunk is fetched (Workflows is statically imported into the initial
chunk).

| Build | Initial raw | Initial gzip | Δ raw | Δ gzip |
|---|---|---|---|---|
| Baseline (current) | 1,510,491 | 296,810 | — | — |
| +lazy only | 1,373,654 | 254,978 | −136,837 | −41,832 |
| +lazy + deep-import icons | **590,388** | **182,693** | **−920,103 (−61%)** | **−114,117 (−38%)** |

**Combined savings: 920 KB raw / 114 KB gzip on first paint.**

---

## 4. Duplication Audit Across the Monorepo

### Method
```bash
ls -d /Users/z/work/hanzo/*/ui/                                  # admin UIs
find /Users/z/work/hanzo/*/ui/src -type f \( -name "Sidebar*" -o -name "TopBar*" \
   -o -name "PageHeader*" -o -name "PageShell*" -o -name "Empty*" -o -name "Badge*" \
   -o -name "CopyField*" -o -name "Field*" \) | xargs wc -l
ls /Users/z/work/hanzo/gui/code/ui-admin/src                     # already-extracted package
```

### Results

#### Admin UI surfaces in the monorepo

| Path | Framework | Components | Status |
|---|---|---|---|
| `tasks/ui/` | React 18 + shadcn + Tailwind | 1,153 LOC | v1, retired on cutover |
| `tasks/ui-tamagui-spike/` | React 19 + Tamagui | 741 LOC (now mostly extracted) | v2, the rebuild |
| `iam/ui/` | React + Tamagui | 466 LOC | active |
| `iam/web/` | React (Casdoor fork) | (not measured fully) | Casdoor admin |
| `console/web/` | Next.js + shadcn | 241 tsx in `components/`, 701 LOC routes | active |
| `base/ui/` | Svelte | excluded (different framework) | — |
| `bot/ui/` | Vue/CSS-only chat | excluded (chat surface, not admin) | — |

#### Direct duplication of admin chrome (LOC)

| Pattern | tasks/ui v1 | tasks v2 spike | iam/ui | console/web | Total dupe LOC |
|---|---:|---:|---:|---:|---:|
| Sidebar | (in App.tsx) | 210 | — | 674+125+115+116 = 1,030 | **1,240** |
| TopBar / PageHeader | (in App.tsx) | 323 | 13 | 282+63+129 = 474 | **810** |
| PageShell / page wrapper | 9 | 12 | — | 33 | **54** |
| Badge / StatusBadge | 35 | 29 | 23 | 325+220+28+21 = 594 | **681** |
| Empty | 21 | 73 | — | 114 | **208** |
| CopyField / Field | — | 41 | 83 | 134+267+68 = 469 | **593** |
| Alert / ErrorState | 41+13 = 54 | 32 | — | (covered by shadcn ui) | **86** |
| DataTable | — | — | 77 | (built-in shadcn) | **77** |
| **Subtotal** | **132** | **720** | **196** | **2,714** | **~3,749 LOC** |

#### Already extracted into `@hanzogui/admin` (1,418 LOC)

`gui/code/ui-admin/src/`:
- `shell/AdminApp.tsx`, `shell/Sidebar.tsx`, `shell/TopBar.tsx`,
  `shell/PageShell.tsx`
- `primitives/BrandMark.tsx`, `primitives/SummaryCard.tsx`,
  `primitives/Empty.tsx`, `primitives/Alert.tsx`, `primitives/Badge.tsx`,
  `primitives/CopyField.tsx`
- `patterns/DetailPage.tsx`, `patterns/ListPage.tsx`
- `data/useFetch.ts`, `data/useEvents.ts`, `data/format.ts`

`tasks/ui-tamagui-spike` already pulls these:
```ts
import { AccountChip, AdminApp, HanzoMark, LocalTimeIndicator,
         NamespaceSwitcher, Sidebar, ThemeToggle, TopBar, useFetch }
  from '@hanzogui/admin'
```

#### Recommended next extractions for `@hanzogui/admin`

Based on what is duplicated 3+ times in the matrix above and not yet
in the package:

1. **`StatusBadge`** — a state-aware badge with success/error/pending
   variants. Currently reimplemented in `console/web` (DIDStatusBadge,
   StatusBadge, ItemBadge: 573 LOC) and partially in tasks v2.
   **Saves ~500 LOC.**
2. **`DataTable`** — paginated, filterable, column-config table.
   `iam/ui/DataTable.tsx` (77) is the reference. Console builds its
   own per-feature. **Saves ~600 LOC** when console adopts.
3. **`DialogForm`** — open/cancel/submit modal with controlled
   form state. Reimplemented in every page that has a "New X"
   button (NamespaceDetail, Batches, Workflows in v2; many in
   console). **Saves ~400 LOC** across the 8 pages that need it.
4. **`FilterBar` + `SavedViewsRail`** — already in tasks/ui v1 (166
   LOC for SavedViewsRail). Console has parallel implementations
   in `features/agents`. **Saves ~300 LOC.**

**Total dedup ceiling once the four are extracted: ~1,800 LOC** on
top of the 1,418 LOC already in `@hanzogui/admin`. End state:
`tasks/ui-tamagui-spike` shrinks from 741 LOC (current
post-extraction) to ~250 LOC of pure tasks-domain code. Console
shrinks by ~2,000 LOC across `features/`.

---

## 5. First-Paint Performance

### Method
```bash
# Live server already running at 127.0.0.1:7243 (tasksd embedded SPA)
node /tmp/perf-probe.js     # custom probe: 10 GETs, sorted, median + p99
```

### Results — Live server (V1 React/shadcn, currently embedded)

| Asset | Size | TTFB min | TTFB med | Total med | Total p99 |
|---|---|---|---|---|---|
| `/_/tasks/` (HTML) | 539 B | 0.20 ms | 0.34 ms | 0.40 ms | 0.55 ms |
| `index-DQHl_ST6.js` | 338,668 B | 0.16 ms | 0.23 ms | 0.41 ms | 0.66 ms |
| `index-DrsqCYES.css` | 24,380 B | 0.14 ms | 0.17 ms | 0.22 ms | 0.40 ms |

Server response is irrelevant on loopback. The baseline over loopback
is too fast to measure meaningful FCP/LCP/TTI. The user's perf
problem is **download cost over the wire + parse cost on slow CPUs**,
not server latency. tasksd is serving with `Cache-Control:
public, max-age=31536000, immutable`. No gzip on the wire (tasksd
sends raw bytes; rely on intermediate cache or fronting proxy for
gzip).

### Network/parse modeling against the V1 baseline (105 KB gzip)

| Network | V1 (105 KB) | V2 today (297 KB) | V2+icons (224 KB) | V2+lazy+deep (183 KB) | Target (200 KB) |
|---|---:|---:|---:|---:|---:|
| WiFi 100 Mbps | 9 ms | 24 ms | 18 ms | **15 ms** | 16 ms |
| Cable 50 Mbps | 17 ms | 49 ms | 37 ms | **30 ms** | 33 ms |
| 4G LTE 10 Mbps | 86 ms | 243 ms | 184 ms | **150 ms** | 164 ms |
| Slow 3G 1.5 Mbps | 573 ms | 1,622 ms | 1,223 ms | **999 ms** | 1,092 ms |

Parse cost on a mid-tier mobile (~1 MB/s V8 parse rate, raw bytes):

| Build | Raw JS | Parse |
|---|---:|---:|
| V1 React/shadcn | 338 KB | ~330 ms |
| V2 baseline | 1,510 KB | ~1,475 ms |
| V2 + icons-deep | 727 KB | ~710 ms |
| V2 + lazy + icons-deep | 590 KB | ~576 ms |

**On slow 3G + slow CPU, baseline V2 is 1.6 s download + 1.5 s parse
= ~3 s before paint.** Combined fix gets that to ~1 s + 0.6 s = ~1.6 s,
2x faster than today, well within "feels instant" budget once HTTP
caching kicks in for repeat visits.

---

## 6. Top 3 Optimizations (Recommended for v2.1)

### Win 1 — Deep-import lucide icons via `icons-deep.ts` shim
- **Delta: −783 KB raw / −73 KB gzip** on every page
- **Effort: 1 hour.** New file `src/lib/icons-deep.ts` (32 exports) +
  one-line sed swap across 14 consumers.
- **Blocker:** none. Tested green here (build succeeds, identical
  visual output). Optionally: contribute a fix to
  `@hanzogui/lucide-icons-2` so the umbrella tree-shakes natively
  (mark all icon files `"sideEffects": false` in package.json), then
  remove the shim.

### Win 2 — `React.lazy()` for 13 of 14 pages, keep Workflows sync
- **Delta: −137 KB raw / −42 KB gzip** on first paint (independent of Win 1)
- **Combined with Win 1: −920 KB raw / −114 KB gzip first paint**
- **Effort: 2 hours.** Rewrite `src/main.tsx` to use `lazy()` for all
  pages except `WorkflowsPage`. Add `<Suspense fallback={<Spinner/>}>`
  wrapper. All 13 lazy chunks come out at <8 KB gzip each. Reproduced
  green here.
- **Blocker:** none on the build side. Product blocker: confirm with
  PM that brief Suspense flash on rare routes (Support, EventDetail,
  Nexus, Workers) is acceptable. Pre-fetch on hover via `<link
  rel="modulepreload">` if it isn't.

### Win 3 — Drop `disable: true` from `hanzoguiPlugin` once
`@hanzogui/admin` package resolution is stable
- **Delta: 0 KB measured today.** Both `disable: true` and the default
  produce identical 1,510,491 / 296,810 bundles. The static extractor
  is currently a no-op because of the temp-dir resolution issue
  documented in `vite.config.ts`.
- **Recommendation: do not bother chasing this now.** Win 1 + Win 2
  are 95% of the prize. Once tasks/ui-tamagui-spike moves into
  `~/work/hanzo/gui` (which the comment in `vite.config.ts` already
  anticipates), revisit. Expected upper bound based on Tamagui static
  extraction docs: 5–15% additional gzip reduction (~10–25 KB), not
  the 783 KB the user might assume.
- **Blocker:** workspace-relative resolution of `@hanzogui/core` from
  the temporary `.hanzogui/` extraction dir. Requires either
  monorepo move or a build-time stub, both flagged as future work
  in vite.config.ts.

### Honorable mention — Drop `react-native-web` shim entirely
- **Delta: ~141 KB raw / ~44 KB gzip** if hanzogui used a pure
  react-dom render path on web. Not feasible without upstream
  hanzogui changes.

---

## 7. Top 3 Dedup Targets for `@hanzogui/admin`

| Component | Current dupe LOC | Saves on extraction |
|---|---:|---:|
| `StatusBadge` (with semantic variants) | ~573 | ~500 LOC across console + tasks |
| `DataTable` (paginate/filter/column config) | ~700 | ~600 LOC (iam + console adoption) |
| `DialogForm` (open/cancel/submit modal) | ~500 | ~400 LOC (tasks + console pages) |
| `FilterBar` + `SavedViewsRail` | ~330 | ~300 LOC |
| **Total** | **~2,100** | **~1,800 LOC** |

Plus the components already extracted (1,418 LOC in `@hanzogui/admin`)
which collectively replace `~3,749 LOC` of admin-chrome duplication
across `tasks/ui`, `tasks/ui-tamagui-spike`, `iam/ui`, and
`console/web`.

---

## 8. Reproducibility / Artifacts

All work was done in `/Users/z/work/hanzo/tasks/ui-tamagui-spike/`,
restored to baseline at end. Ephemeral helpers:

- `/tmp/vite-perf-build.config.ts` — vite config with rollup-plugin-visualizer
- `/tmp/parse-bundle2.py` — parses visualizer HTML data block, prints package + family rollup
- `/tmp/perf-probe.js` — Node http TTFB/total probe, 10 runs, prints median + p99
- `/tmp/perf-bundle-stats.html` — visualizer treemap (open in browser for interactive view)

Commands to reproduce the icon-deep-import test:
```bash
cd ~/work/hanzo/tasks/ui-tamagui-spike
# create src/lib/icons-deep.ts (32 re-exports)
for f in src/components/*.tsx src/pages/*.tsx; do
  sed -i.bak "s|from '@hanzogui/lucide-icons-2'|from '../lib/icons-deep'|g" $f
done
npx vite build              # 727 KB raw / 224 KB gzip
# then revert: for f in src/**/*.tsx; do mv $f.bak $f; done
```

Commands to reproduce the lazy-loading test: see the patched
`src/main.tsx` template in §3 — 13 of 14 pages wrapped in
`React.lazy(() => import('./pages/X').then(m => ({ default: m.XPage })))`.

Build produces 1 root chunk + 1 shared `Text-` chunk + 13 per-route
chunks. Combined with Win 1, the root chunk is 590 KB raw / 183 KB
gzip — 38% under the 200 KB target.
