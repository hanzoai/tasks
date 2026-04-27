# Tamagui admin UI moved

The Tamagui-based Tasks admin app that lived at `ui-tamagui-spike/`
moved into the gui workspace on 2026-04-27.

## New home

```
~/work/hanzo/gui/code/admin-tasks/
github.com/hanzoai/gui → code/admin-tasks/
```

Package: `@hanzogui/admin-tasks` (workspace).

## Why it moved

The Tamagui static extractor copies `hanzogui.config.ts` into
`.hanzogui/hanzogui.config.mjs` at build time and re-resolves
workspace deps from THAT temp file. Outside a workspace, pnpm hoisting
can't reach the temp dir and the extractor crashes on
`cannot find @hanzogui/core`. Inside the gui workspace every dep
resolves through the root `node_modules` cleanly and the extractor
emits the theme CSS layer.

Result of the move:

- JS bundle: 1,511,832 B → 674,728 B (-55%)
- Gzip JS:     294,962 B →   201,529 B (-32%)
- CSS:                0 B →     9,104 B (extracted theme)

## What stayed here

`~/work/hanzo/tasks/ui/` — the React/shadcn UI that ships embedded
inside the `tasksd` Go binary via `go:embed`. That's still the
production target for the single-binary deployment form factor and
will not be replaced.

## Two UIs, one backend

| UI | Path | Deploy | Stack |
|---|---|---|---|
| Embedded | `ui/` (this repo) | `tasksd` binary, single Go process | React + shadcn + Tailwind |
| Standalone | `gui/code/admin-tasks/` | `tasks.hanzo.ai`, separate K8s pod | Tamagui (`hanzogui` v102) |

Both consume the same `/v1/tasks/*` HTTP API and `/v1/tasks/events`
SSE stream. Only the chrome differs.
