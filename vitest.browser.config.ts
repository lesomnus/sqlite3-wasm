import { playwright } from '@vitest/browser-playwright'
import { defineConfig } from 'vitest/config'

// A separate file rather than an inline entry in `projects`: @vitest/browser
// serves its tester iframe from the *project's* Vite server and copies
// `server.headers` off that server's resolved config, and an inline project
// object does not get those settings applied. Without the COOP/COEP pair the
// page is not cross-origin isolated, SharedArrayBuffer is absent, and the opfs
// VFS silently fails to install — which shows up only as `opfs` missing from
// sqlite3_js_vfs_list(), not as an error.
export default defineConfig({
	server: {
		headers: {
			'Cross-Origin-Opener-Policy': 'same-origin',
			'Cross-Origin-Embedder-Policy': 'require-corp',
		},
	},
	optimizeDeps: {
		// The dep optimizer mangles sqlite3-worker1-bundler-friendly.mjs; every
		// browser test then dies as a bare 30s timeout.
		exclude: ['@sqlite.org/sqlite-wasm'],
	},
	test: {
		name: 'browser',
		include: ['src/**/*.test.ts'],
		exclude: ['src/**/*.node.test.ts'],
		browser: {
			enabled: true,
			provider: playwright(),
			// https://vitest.dev/guide/browser/playwright
			headless: true,
			// Every platform claim this library rests on — nested module workers,
			// blob-worker isolation inheritance, SharedArrayBuffer, OPFS sync
			// access handles — was verified on Chromium first. Firefox and WebKit
			// run the same suite so a claim that is only true of one engine
			// cannot go unnoticed.
			instances: [
				{ browser: 'chromium' },
				{ browser: 'firefox' },
				{ browser: 'webkit' },
			],
		},
	},
})
