import { readFileSync } from 'node:fs'
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
	test('refuses to fall back to a data: worker', () => {
		expect(dist).toContain('could not create a blob worker')
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
