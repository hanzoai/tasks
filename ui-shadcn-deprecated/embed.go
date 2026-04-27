// Package ui exposes the built Vite bundle as an embedded filesystem.
//
// The bundle is produced by `pnpm --prefix ui build` (CI: the
// Dockerfile runs it in a node:22 stage, then the final image stage
// compiles tasksd with this embed). The resulting ui/dist is baked
// into the tasksd binary — no external static-assets directory, no
// sidecar, no separate deploy.
//
// Mount the returned handler at / in the Temporal frontend HTTP
// router so it serves the SPA shell for every non-API request:
//
//	import tasksui "github.com/hanzoai/tasks/ui"
//	router.PathPrefix("/").Handler(tasksui.Handler())
//
// The Temporal gRPC-Gateway routes under /api/v1/* take precedence
// via earlier route registration; everything else falls through to
// the SPA, which supports client-side routing (react-router) so
// deep links survive reload.
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
// Empty when the build step has not run (dev workflow uses the Vite
// dev server instead — see ui/vite.config.ts).
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
		http.Error(w, "tasks UI not built (run `pnpm --prefix ui build`)", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
	_ = r
}
