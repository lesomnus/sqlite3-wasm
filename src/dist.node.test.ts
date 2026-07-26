import { readFileSync, readdirSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { beforeAll, describe, expect, test } from 'vitest'


// These consume the *built* bundle rather than the source tree, because the
// distribution is where the interesting failures live: a Vite library build of
// a URL-based worker emits sibling assets a downstream bundler never copies,
// and the only symptom is OPFS quietly disappearing.
//
// Run `npx vite build` first; the suite says so rather than silently passing.

const distPath = fileURLToPath(new URL('../dist/index.es.js', import.meta.url))
const goWorkerPath = fileURLToPath(new URL('../dist/go-worker.es.js', import.meta.url))

let dist: string
let goWorker: string

beforeAll(() => {
	try {
		dist = readFileSync(distPath, 'utf8')
		goWorker = readFileSync(goWorkerPath, 'utf8')
	} catch {
		throw new Error('dist/ is missing or incomplete — run `npx vite build` first')
	}
})

describe('the built bundle', () => {
	test('is a single self-contained file', () => {
		// Any surviving `new URL(<literal>, import.meta.url)` is an asset the
		// consumer's build has to copy and almost certainly will not.
		const refs = dist.match(/new URL\((?:\/\* @vite-ignore \*\/ )?\\?"[^"]{0,120}\\?"/g)
		expect(refs).toBeNull()
	})

	test('inlines sqlite3.wasm', () => {
		expect(dist).toContain('data:application/wasm;base64')
	})

	test('does not drag in the unused worker1 promiser', () => {
		expect(dist).not.toContain('sqlite3Worker1Promiser')
	})

	// A data: worker has an opaque origin, so it is not cross-origin isolated
	// and has no SharedArrayBuffer: no OPFS, no cancellation. Failing loudly
	// beats degrading silently.
	//
	// Every emitted chunk is checked, not just the entry: a chunk can hold more
	// than one copy of Vite's worker wrapper, and a single string replace
	// patched only the first — leaving the real, exported one unguarded.
	test('refuses to fall back to a data: worker, in every chunk', () => {
		const dir = fileURLToPath(new URL('../dist', import.meta.url))
		let sites = 0
		for (const name of readdirSync(dir)) {
			if (!name.endsWith('.js')) continue
			const code = readFileSync(`${dir}/${name}`, 'utf8')
			for (let i = code.indexOf('data:text/javascript'); i !== -1; ) {
				sites++
				const before = code.slice(Math.max(0, i - 400), i)
				expect(before, `unguarded data: worker in ${name} at ${i}`).toContain(
					'could not create a blob worker',
				)
				i = code.indexOf('data:text/javascript', i + 1)
			}
		}
		// If Vite stops emitting the fallback the guard is pointless, and the
		// plugin's this.error would not have fired; notice that too.
		expect(sites).toBeGreaterThan(0)
	})
})

describe('the built go-worker entry', () => {
	// The 1.6 MB runtime sits behind a dynamic import so it stays off the
	// importing page's critical path. That is only safe because a relative
	// dynamic import is a module-graph edge every bundler follows and
	// rebundles — unlike `new Worker(new URL(...))`, which is an asset
	// reference a consumer's build silently drops.
	function runtimeChunk(): string {
		const ref = goWorker.match(/import\('(\.\/[^']+)'\)/)
		expect(ref, 'go-worker.es.js should lazily import the runtime chunk').not.toBeNull()
		const name = (ref as RegExpMatchArray)[1].replace(/^\.\//, '')
		return readFileSync(fileURLToPath(new URL(`../dist/${name}`, import.meta.url)), 'utf8')
	}

	test('keeps the runtime off the entry chunk', () => {
		expect(goWorker.length).toBeLessThan(50_000)
	})

	// Both of these arrive through bare side-effect imports inside the inlined
	// runtime worker, and rollup removed both when package.json's sideEffects
	// list named only the dist files: the worker shipped with no Go class and
	// no global, and nothing said so.
	test('carries the Go runtime into the inlined worker', () => {
		expect(runtimeChunk()).toContain('globalThis.Go = class')
	})

	test('installs the driver global inside the inlined worker', () => {
		expect(runtimeChunk()).toContain('sqlite3-wasm-go')
	})

	test('carries the database worker too, so the Go program can open one', () => {
		expect(runtimeChunk()).toContain('data:application/wasm;base64')
	})

	test('is self-contained apart from that one chunk', () => {
		const refs = runtimeChunk().match(/new URL\((?:\/\* @vite-ignore \*\/ )?\\?"[^"]{0,120}\\?"/g)
		expect(refs).toBeNull()
	})
})

describe('the emitted type declarations', () => {
	// vite-plugin-dts writes extensionless relative imports by default. The
	// package is "type": "module", so under moduleResolution node16/nodenext
	// those do not resolve and the entire public type surface degrades to
	// `any` — silently, under the near-universal skipLibCheck: true.
	test('have no unresolvable relative imports', () => {
		const dir = fileURLToPath(new URL('../dist', import.meta.url))
		for (const name of readdirSync(dir)) {
			if (!name.endsWith('.d.ts')) continue
			const code = readFileSync(`${dir}/${name}`, 'utf8')
			const relative = code.match(/from ['"]\.[^'"]*['"]/g) ?? []
			const extensionless = relative.filter((r) => !/\.js['"]$/.test(r))
			expect(extensionless, `${name} imports ${extensionless.join(', ')}`).toEqual([])
		}
	})
})
