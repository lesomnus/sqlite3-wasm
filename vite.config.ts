import { defineConfig } from 'vitest/config'
import { playwright } from '@vitest/browser-playwright'
import dts from 'vite-plugin-dts'
import path from 'path'

export default defineConfig({
  build: {
    lib: {
      entry: path.resolve(__dirname, 'src/index.ts'),
			formats: ["es"],
			fileName: (format, entryName) => `${entryName}.${format}.js`,
    },
    rollupOptions: {
      external: [
        '@sqlite.org/sqlite-wasm'
      ],
    },
    sourcemap: true,
    target: 'esnext',
    minify: false,
  },
  plugins: [
    dts({
			tsconfigPath: "./tsconfig.build.json",
      insertTypesEntry: true,
      outDir: 'dist',
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
