// Package ui exposes the built admin-tasks (@hanzo/gui) bundle as an
// embedded filesystem.
//
// The bundle is produced externally in the @hanzo/gui workspace at
// ~/work/hanzo/gui/code/admin-tasks (Vite + @hanzo/gui) and synced into
// ui/dist by scripts/sync-admin-ui.sh. The synced dist/ is baked into
// the tasksd binary at compile time — no external static-assets
// directory, no sidecar, no separate deploy. See ui/README.md.
//
// Mount the returned handler at /_/tasks/ in the tasksd HTTP router
// so it serves the SPA shell for every non-API request:
//
//	import tasksui "github.com/hanzoai/tasks/ui"
//	router.PathPrefix("/_/tasks/").Handler(http.StripPrefix("/_/tasks", tasksui.Handler()))
//
// API routes under /v1/* take precedence via earlier route
// registration; anything else falls through to the SPA, which uses
// client-side routing so deep links survive reload.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the embedded built-UI filesystem rooted at dist/.
// Empty when scripts/sync-admin-ui.sh has not been run (dev workflow:
// run the Vite dev server inside ~/work/hanzo/gui/code/admin-tasks).
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Impossible at runtime because embed.FS entries are
		// validated at compile time; if dist/ is missing the
		// binary simply carries an empty FS.
		return distFS
	}
	return sub
}

// Handler returns an http.Handler that serves the embedded SPA.
//
// Behaviour:
//  - Exact-match static assets (JS/CSS/images) ship with
//    immutable cache hints because Vite hashes filenames.
//  - Anything else rewrites to /index.html so the React router
//    handles the route client-side. This is the standard SPA
//    fallback and is the only way react-router's BrowserRouter
//    survives a page reload on a deep link.
//  - If the build hasn't run and index.html is missing, every
//    request returns 503 so operators notice in staging before
//    shipping a blank image to production.
func Handler() http.Handler {
	root := FS()
	fileServer := http.FileServer(http.FS(root))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only GET/HEAD — the UI is static; any other method on a
		// non-/api path is a client error, not a route we own.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		reqPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if reqPath == "" {
			reqPath = "index.html"
		}

		// Probe for the asset. If it doesn't exist, serve the SPA
		// shell so client-side routing picks up the deep link.
		if _, err := fs.Stat(root, reqPath); err != nil {
			serveIndex(w, r, root)
			return
		}

		// Hashed assets under /assets/ are immutable — Vite emits
		// them with content-addressed names so it's safe to tell
		// browsers to cache forever.
		if strings.HasPrefix(reqPath, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})
}

// serveIndex writes index.html with no-cache so a newly-deployed
// build replaces the stale shell on the next request.
func serveIndex(w http.ResponseWriter, r *http.Request, root fs.FS) {
	data, err := fs.ReadFile(root, "index.html")
	if err != nil {
		// No UI bundled — report honestly. Operators see this in
		// logs and in the browser; no blank screen in production.
		http.Error(w, "tasks UI not built (run scripts/sync-admin-ui.sh)", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
	_ = r
}
