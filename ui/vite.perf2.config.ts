import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { hanzoguiPlugin } from '@hanzogui/vite-plugin'
import { visualizer } from 'rollup-plugin-visualizer'

const APP_VERSION = process.env.VITE_APP_VERSION ?? '2.45.3'

export default defineConfig({
  plugins: [
    react(),
    hanzoguiPlugin({
      components: ['hanzogui'],
      config: 'hanzogui.config.ts',
      // CANONICAL: leave extraction default (the actual production
      // build matches what's in dist/). The user's vite.config.ts has
      // `disable: true` which BLOATS the bundle. We test both.
    }),
    visualizer({
      filename: '/tmp/perf-bundle-stats-real.html',
      template: 'treemap',
      gzipSize: true,
      brotliSize: false,
      sourcemap: false,
      open: false,
      emitFile: false,
    }),
  ],
  base: '/_/tasks/',
  define: {
    'import.meta.env.VITE_APP_VERSION': JSON.stringify(APP_VERSION),
    __DEV__: process.env.NODE_ENV !== 'production' ? 'true' : 'false',
    'process.env.HANZOGUI_TARGET': JSON.stringify('web'),
    'process.env.HANZOGUI_REACT_19': '"1"',
  },
  resolve: {
    alias: { 'react-native': 'react-native-web' },
  },
  optimizeDeps: {
    include: ['react', 'react-dom', 'react-native-web', 'hanzogui'],
    esbuildOptions: {
      resolveExtensions: ['.web.tsx', '.web.ts', '.web.jsx', '.web.js', '.tsx', '.ts', '.jsx', '.js'],
      loader: { '.js': 'jsx' },
    },
  },
  build: {
    outDir: '/tmp/perf-dist-real',
    emptyOutDir: true,
    sourcemap: false,
    assetsInlineLimit: 16 * 1024,
    target: 'es2020',
  },
})
