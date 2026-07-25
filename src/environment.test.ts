import { expect, test } from 'vitest'

// Cross-origin isolation is a silent dependency: without the COOP/COEP pair the
// opfs VFS installer's preconditions fail, sqlite3InitModule() still resolves,
// and the only symptom is `opfs` missing from sqlite3_js_vfs_list(). Assert it
// directly so a config change cannot quietly turn persistence off — that is
// exactly what a vitest `projects` refactor did once already.
test('the test page is cross-origin isolated', () => {
	expect(globalThis.crossOriginIsolated).toBe(true)
	expect(typeof SharedArrayBuffer).toBe('function')
	expect(typeof Atomics).toBe('object')
})

test('OPFS is reachable from the page', () => {
	expect(typeof navigator.storage?.getDirectory).toBe('function')
	// createSyncAccessHandle is [Exposed=DedicatedWorker], so it is absent
	// here by design. That is why sqlite3 refuses to install the opfs VFS
	// outside a worker.
	expect(
		(FileSystemFileHandle.prototype as unknown as Record<string, unknown>)
			.createSyncAccessHandle,
	).toBeUndefined()
})

// The whole distribution strategy rests on this: the DB worker ships as an
// inlined blob module worker, and that only works if a blob: worker inherits
// the page's cross-origin isolation. (A data: worker does not — opaque origin,
// no SharedArrayBuffer — which is why Vite's data: fallback must never fire.)
test('a blob module worker inherits cross-origin isolation and can reach OPFS', async () => {
	const src = `
		self.postMessage({
			coi: self.crossOriginIsolated,
			sab: typeof SharedArrayBuffer,
			atomicsWait: typeof Atomics?.wait,
			syncAccess: typeof FileSystemFileHandle.prototype.createSyncAccessHandle,
			nested: typeof Worker,
		})
	`
	const url = URL.createObjectURL(new Blob([src], { type: 'text/javascript' }))
	const worker = new Worker(url, { type: 'module' })
	try {
		const got = await new Promise<Record<string, unknown>>((resolve, reject) => {
			const timer = setTimeout(() => reject(new Error('blob worker never replied')), 10_000)
			worker.onerror = (e) => {
				clearTimeout(timer)
				reject(new Error(e.message ?? 'blob worker failed to construct'))
			}
			worker.onmessage = (e) => {
				clearTimeout(timer)
				resolve(e.data)
			}
		})

		expect(got).toEqual({
			coi: true,
			sab: 'function',
			atomicsWait: 'function',
			syncAccess: 'function',
			nested: 'function',
		})
	} finally {
		worker.terminate()
		URL.revokeObjectURL(url)
	}
})
