import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { hanzoguiPlugin } from '@hanzogui/vite-plugin'

// Tasks UI v2 (Tamagui spike).
//
// Bundle goes to ui-tamagui/dist. Once the spike is full-coverage and
// the v1 React DOM bundle is retired, tasksd's go:embed in
// ../ui/embed.go gets repointed at this directory.
//
// Mirror of v1 vite.config.ts — same base, same proxy, same build
// settings — so the embedded SPA path doesn't shift when we cut over.
const APP_VERSION = process.env.VITE_APP_VERSION ?? '2.45.3'

export default defineConfig({
  plugins: [
    react(),
    // hanzoguiPlugin runs Tamagui's static extractor + react-native-web
    // shim layer. We pass `components: ['hanzogui']` so the extractor
    // knows which JSX surface to visit. `disableExtraction: true` keeps
    // the runtime CSS-in-JS path on — the static extractor can't resolve
    // `@hanzogui/core` from its temp `.hanzogui/` bundle when we're
    // outside the gui workspace, which produces noisy build warnings.
    // The runtime path produces the same visible output, just slightly
    // larger. Re-enable extraction once we cut over and live inside
    // the hanzo monorepo at ~/work/hanzo/gui.
    hanzoguiPlugin({
      components: ['hanzogui'],
      config: 'hanzogui.config.ts',
      disable: true,
    }),
  ],
  base: '/_/tasks/',
  define: {
    'import.meta.env.VITE_APP_VERSION': JSON.stringify(APP_VERSION),
    // Tamagui's react-native-web shim wants this — without it, react-native
    // imports try to evaluate __DEV__ at runtime in browser code.
    __DEV__: process.env.NODE_ENV !== 'production' ? 'true' : 'false',
    'process.env.HANZOGUI_TARGET': JSON.stringify('web'),
    'process.env.HANZOGUI_REACT_19': '"1"',
  },
  resolve: {
    alias: {
      // Tamagui uses react-native primitives; on web they resolve to
      // react-native-web. Aliasing once at the bundler level keeps deep
      // imports (e.g. inside @hanzogui/stacks) consistent.
      'react-native': 'react-native-web',
    },
  },
  optimizeDeps: {
    // These three are CJS — pre-bundling avoids "default export not
    // found" errors when Vite tries to ESM-eval them on the fly.
    include: ['react', 'react-dom', 'react-native-web', 'hanzogui'],
    esbuildOptions: {
      resolveExtensions: ['.web.tsx', '.web.ts', '.web.jsx', '.web.js', '.tsx', '.ts', '.jsx', '.js'],
      loader: { '.js': 'jsx' },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
    assetsInlineLimit: 16 * 1024,
    target: 'es2020',
  },
  server: {
    port: 5174,
    proxy: {
      '/v1/tasks': 'http://127.0.0.1:7243',
    },
  },
})
