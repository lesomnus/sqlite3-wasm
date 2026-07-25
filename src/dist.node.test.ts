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

let dist: string

beforeAll(() => {
	try {
		dist = readFileSync(distPath, 'utf8')
	} catch {
		throw new Error('dist/index.es.js is missing — run `npx vite build` first')
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
