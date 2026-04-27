import { defineConfig } from 'vite'
import path from 'node:path'
import react from '@vitejs/plugin-react'
import { hanzoguiPlugin } from '@hanzogui/vite-plugin'

// Tasks UI v2 — Tamagui via @hanzo/gui.
//
// Bundle goes to ui/dist. cmd/tasksd embeds it via go:embed in
// ui/embed.go. The base path /_/tasks/ matches what the Go server
// strips before serving, so deep links (.../namespaces/default/...)
// still resolve to the SPA shell.
//
// Critical: hanzoguiPlugin MUST be enabled (no `disable: true`) so
// Tamagui's static extractor emits the resolved theme CSS at build
// time. Without that CSS layer, runtime `getThemeProxied()` throws
// "Missing theme" and renders a blank page. Disabling extraction
// only works in dev mode — production builds need the CSS.

const APP_VERSION = process.env.VITE_APP_VERSION ?? '2.45.3'

export default defineConfig({
  plugins: [
    hanzoguiPlugin({
      components: ['hanzogui'],
      // Absolute path — keeps the extractor from getting confused
      // when it copies the config into a `.hanzogui/` temp dir and
      // tries to re-resolve workspace deps from there.
      config: path.resolve(__dirname, 'hanzogui.config.ts'),
      // Extraction is required for production. If the temp-dir
      // workspace-resolution still bites, we'll inline a stub at
      // build time, never disable.
    }),
    react(),
  ],
  base: '/_/tasks/',
  define: {
    'import.meta.env.VITE_APP_VERSION': JSON.stringify(APP_VERSION),
    __DEV__: process.env.NODE_ENV !== 'production' ? 'true' : 'false',
    'process.env.HANZOGUI_TARGET': JSON.stringify('web'),
    'process.env.HANZOGUI_REACT_19': '"1"',
  },
  resolve: {
    alias: {
      'react-native': 'react-native-web',
    },
  },
  optimizeDeps: {
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
