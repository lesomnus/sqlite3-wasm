import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { dirname, join } from 'node:path'

import type { Plugin } from 'vite'

/**
 * Makes the database worker self-contained, so a consumer needs no bundler
 * configuration and no extra assets served alongside the package.
 *
 * Three things stand in the way of that, all verified against a real library
 * build:
 *
 *  1. Importing `@sqlite.org/sqlite-wasm` pulls its `index.mjs`, which
 *     side-effect-imports the worker1 promiser, whose default config spawns
 *     `sqlite3-worker1-bundler-friendly.mjs`. That drags in a **second** 1.6 MB
 *     copy of sqlite3 that is never used.
 *
 *  2. The OPFS VFS spawns its async proxy with
 *     `new Worker(new URL('sqlite3-opfs-async-proxy.js', import.meta.url))`.
 *     Vite emits that as a sibling asset, which a downstream build does not
 *     copy — and in a blob worker `import.meta.url` cannot resolve a relative
 *     URL at all. The failure is swallowed into a console warning, so OPFS just
 *     silently disappears and every database becomes transient.
 *
 *  3. Vite's inline-worker runtime falls back to a `data:` worker when blob
 *     construction fails. A `data:` worker has an opaque origin, so it is not
 *     cross-origin isolated and has no SharedArrayBuffer — meaning no OPFS and
 *     no cancellation. Failing loudly is strictly better than that.
 */
export function sqlite3Inline(): Plugin {
	const require = createRequire(import.meta.url)
	const pkgJson = require.resolve('@sqlite.org/sqlite-wasm/package.json')
	const jswasm = join(dirname(pkgJson), 'sqlite-wasm', 'jswasm')
	const bundlerFriendly = join(jswasm, 'sqlite3-bundler-friendly.mjs')
	const proxySource = join(jswasm, 'sqlite3-opfs-async-proxy.js')

	const PROXY_NEEDLE = "const W = new Worker(\n              new URL('sqlite3-opfs-async-proxy.js', import.meta.url),\n            );"

	const DATA_WORKER_NEEDLE = 'return new Worker(\n      "data:text/javascript;charset=utf-8," + encodeURIComponent(jsContent),'

	return {
		name: 'sqlite3-wasm-go:inline',
		enforce: 'pre',

		// (1) Skip the package entry, whose only extra contribution is the
		// worker1 promiser we replaced.
		resolveId(source) {
			if (source === '@sqlite.org/sqlite-wasm') return bundlerFriendly
			return null
		},

		// (2) Inline the OPFS async proxy as a nested blob.
		transform(code, id) {
			if (!id.startsWith(bundlerFriendly)) return null
			if (!code.includes(PROXY_NEEDLE)) {
				this.error(
					'sqlite3-wasm-go: could not find the OPFS async-proxy spawn in ' +
						'sqlite3-bundler-friendly.mjs. The pinned @sqlite.org/sqlite-wasm version ' +
						'has changed; re-check the needle before bumping it.',
				)
			}

			const proxy = readFileSync(proxySource, 'utf8')
			const replacement =
				'const W = new Worker(URL.createObjectURL(new Blob([' +
				JSON.stringify(proxy) +
				"], { type: 'text/javascript' })));"

			return { code: code.replace(PROXY_NEEDLE, replacement), map: null }
		},

		// (3) Turn the silent de-isolation into an error.
		//
		// replaceAll, not replace: a chunk can hold more than one copy of the
		// wrapper. dist/runtime-*.js embeds the whole go-runtime worker source
		// as a template literal, so the first textual match is the *database*
		// worker's wrapper inside that string — already patched by the nested
		// worker pass — and a single replace would leave the real, exported
		// wrapper untouched with its data: fallback intact.
		renderChunk(code) {
			if (!code.includes('WorkerWrapper')) return null
			if (!code.includes(DATA_WORKER_NEEDLE)) {
				this.error(
					'sqlite3-wasm-go: a chunk defines WorkerWrapper but does not match the ' +
						"data: worker fallback. Vite's inline-worker helper has changed shape; " +
						're-check the needle before bumping vite.',
				)
			}
			return {
				code: code.replaceAll(
					DATA_WORKER_NEEDLE,
					'throw new Error(\n      "sqlite3-wasm-go: could not create a blob worker. " +\n      "A data: worker would not be cross-origin isolated, so OPFS and cancellation " +\n      "would silently stop working. Allow worker-src blob: in your CSP." + (e ? " (" + e + ")" : "")\n    ); return new Worker(\n      "data:text/javascript;charset=utf-8," + encodeURIComponent(jsContent),',
				),
				map: null,
			}
		},
	}
}
