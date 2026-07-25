import { describe, expect, test } from 'vitest'

import { Cap } from './wire'
import { Client } from './worker/client'

// These consume the *built* bundle rather than the source tree, because the
// distribution is where the interesting failures live: a Vite library build of
// a URL-based worker emits sibling assets a downstream bundler never copies,
// and the only symptom is OPFS quietly disappearing.
//
// The static shape of the bundle is checked in dist.node.test.ts, which can
// read the file; this tier runs it.
//
// Run `npx vite build` first.

type Api = { protocolVersion: number; createWorker(): Worker }

async function loadDist(): Promise<{ mod: Record<string, never>; api: Api }> {
	// The bundle has no .d.ts of its own from the test's point of view; it is
	// loaded as an artifact, not as a typed dependency.
	const mod = (await import(/* @vite-ignore */ '../dist/index.es.js' as string)) as unknown
	const api = (globalThis as Record<string, unknown>)[
		(mod as { GLOBAL_KEY: string }).GLOBAL_KEY
	] as Api
	return { mod: mod as never, api }
}

describe('the built bundle at runtime', () => {
	test('installs the global and speaks the protocol', async () => {
		const { mod, api } = await loadDist()
		const version = (mod as unknown as { PROTOCOL_VERSION: number }).PROTOCOL_VERSION
		expect(api).toBeTruthy()
		expect(api.protocolVersion).toBe(version)

		const client = new Client(api.createWorker())
		try {
			const ready = await client.ready
			expect(ready.protocolVersion).toBe(version)

			// The whole point of inlining the OPFS async proxy: without it the
			// installer fails, the failure is swallowed into a warning, and the
			// VFS is simply absent.
			expect(ready.vfsList).toContain('opfs')
			expect(ready.capabilities & Cap.VFS_OPFS).toBeTruthy()
			expect(ready.capabilities & Cap.PROGRESS_HANDLER).toBeTruthy()

			const { dbId } = await client.open('file:/dist-smoke?vfs=memdb', 'memdb')
			await client.exec(dbId, 'CREATE TABLE t(x INTEGER); INSERT INTO t VALUES (9007199254740993)')
			const rs = await client.query(dbId, 'SELECT x FROM t')
			expect(rs.rows[0][0]).toBe(9007199254740993n)
		} finally {
			client.terminate()
		}
	}, 30_000)

	// The claim that makes persistence real, exercised against the built
	// artifact and a genuine OPFS file rather than memdb.
	test('an OPFS database persists across workers', async () => {
		const { api } = await loadDist()

		const name = `dist-opfs-${Math.random().toString(36).slice(2)}.db`
		const dsn = `file:/${name}?vfs=opfs`

		const first = new Client(api.createWorker())
		try {
			await first.ready
			const { dbId } = await first.open(dsn, 'opfs')
			await first.exec(dbId, 'CREATE TABLE t(x TEXT)')
			await first.exec(dbId, 'INSERT INTO t VALUES (?)', ['persisted'])
			await first.close(dbId)
		} finally {
			first.terminate()
		}

		// A fresh worker, so nothing can be carried over in memory.
		const second = new Client(api.createWorker())
		try {
			await second.ready
			const { dbId } = await second.open(dsn, 'opfs')
			const rs = await second.query(dbId, 'SELECT x FROM t')
			expect(rs.rows[0][0]).toBe('persisted')
			await second.close(dbId)
		} finally {
			second.terminate()
		}

		const root = await navigator.storage.getDirectory()
		await root.removeEntry(name).catch(() => {})
	}, 60_000)
})
