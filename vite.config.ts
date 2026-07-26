import { defineConfig } from 'vitest/config'
import { playwright } from '@vitest/browser-playwright'
import dts from 'vite-plugin-dts'
import { sqlite3Inline } from './scripts/vite-plugin-sqlite3-inline'
import path from 'path'

export default defineConfig({
  // The default worker format is 'iife', which cannot carry a top-level await;
  // dev always uses module workers, so without this the DB worker would work in
  // development and break in a production build.
  worker: {
    format: 'es',
    plugins: () => [sqlite3Inline()],
  },
  build: {
    lib: {
      entry: {
        index: path.resolve(__dirname, 'src/index.ts'),
        'go-worker': path.resolve(__dirname, 'src/go-worker.ts'),
      },
      formats: ['es'],
      fileName: (format, entryName) => `${entryName}.${format}.js`,
    },
    sourcemap: true,
    target: 'esnext',
    minify: false,
  },
  plugins: [
    sqlite3Inline(),
    dts({
      tsconfigPath: './tsconfig.build.json',
      insertTypesEntry: true,
      outDir: 'dist',
      // Without this the emitted .d.ts files import './global' and './wire'
      // with no extension. The package is "type": "module", so under
      // moduleResolution node16/nodenext those specifiers do not resolve and
      // the whole public type surface silently degrades to `any`.
      rollupTypes: true,
    }),
  ],
  server: {
    headers: {
      'Cross-Origin-Opener-Policy': 'same-origin',
      'Cross-Origin-Embedder-Policy': 'require-corp',
    },
  },
  optimizeDeps: {
    exclude: ['@sqlite.org/sqlite-wasm'],
  },
  test: {
    // Two tiers. The wire codec is plain byte manipulation with no DOM in it,
    // so it runs in node where it is fast and debuggable; everything that
    // touches sqlite3, workers, OPFS or Go/wasm needs a real browser.
    projects: [
      {
        test: {
          name: 'node',
          environment: 'node',
          include: ['src/**/*.node.test.ts'],
        },
      },
      './vitest.browser.config.ts',
    ],
  },
})
