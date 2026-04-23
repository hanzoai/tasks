import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Tasks UI is a zero-dep single-page app. Vite 8 bundles it into
// ui/dist which is then embedded into the tasksd binary via go:embed
// at build time. Dev server proxies API calls to the local tasksd.
//
// Output: ui/dist
//   ├── index.html        (SPA shell; served at / on port 7234)
//   ├── assets/*.js|*.css (hashed chunks)
//   └── favicon.svg
//
// Routing: react-router renders client-side. Server serves index.html
// for every unknown path (except /api/*) so deep links survive reload.
export default defineConfig({
  plugins: [react()],
  base: '/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    // Inline assets <16KB to reduce round-trips. Bigger assets stay
    // in /assets/ with 1-year immutable cache (handled in Go).
    assetsInlineLimit: 16 * 1024,
  },
  server: {
    port: 5173,
    proxy: {
      // During dev, tasksd runs on 7234 and the Vite dev server
      // proxies /api/* to it so the frontend can call the real
      // Temporal gRPC-Gateway routes.
      '/api': 'http://localhost:7234',
    },
  },
})
