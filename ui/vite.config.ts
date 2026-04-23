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
// One and one way only:
//   UI:  /_/tasks/*          (embedded SPA)
//   API: /v1/tasks/*         (gofiber JSON over ZAP-backed handlers)
// No /api/, no split paths, no dual mounts.
export default defineConfig({
  plugins: [react()],
  base: '/_/tasks/',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    assetsInlineLimit: 16 * 1024,
  },
  server: {
    port: 5173,
    proxy: {
      '/v1/tasks': 'http://localhost:7234',
    },
  },
})
