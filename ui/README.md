# tasks/ui — embedded SPA bundle

Compiled assets only. There is no source in this directory.

## Where the source lives

`~/work/hanzo/gui/code/admin-tasks/` — Vite + Tamagui app in the
`@hanzo/gui` workspace. Build it from there:

```
cd ~/work/hanzo/gui/code/admin-tasks
bun install
bun run build
```

That produces `admin-tasks/dist/` with `index.html` and hashed
`assets/*.{js,css}`.

## How the bundle gets here

`scripts/sync-admin-ui.sh` (top of this repo) rsyncs
`admin-tasks/dist/` into `ui/dist/` and writes `dist/.sync-stamp` for
provenance. Run it after every admin-tasks build before invoking
`go build`:

```
scripts/sync-admin-ui.sh
go build -trimpath -ldflags="-s -w" -o tasksd ./cmd/tasksd
```

Override the source location with `ADMIN_TASKS_DIR=/abs/path` if the
gui repo is checked out elsewhere.

## How tasksd serves it

`embed.go` declares `//go:embed all:dist`. The `tasksd` binary carries
the SPA — no sidecar container, no static volume, no nginx. The
HTTP router mounts the SPA at `/_/tasks/` (see
`pkg/tasks/server_http.go`). API routes (`/v1/*`) take precedence;
anything else falls through to `index.html` so the SPA router handles
deep links.

Hashed assets under `/_/tasks/assets/` are served with
`Cache-Control: public, max-age=31536000, immutable`. The shell
(`index.html`) is always `no-cache`.
