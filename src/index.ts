/**
 * The library entry point.
 *
 * Importing it installs `globalThis["sqlite3-wasm-go"]`, which is the only
 * thing the Go program looks for. **It must be evaluated in the same realm as
 * the Go program** — importing it on the main thread while Go runs in a worker
 * installs the global in the wrong global scope, and the Go side will report
 * that the library was never imported.
 */

import { type CreateWorkerOptions, GLOBAL_KEY, type Sqlite3WasmGo } from './global'
import { PROTOCOL_VERSION } from './wire'
// Inlined, not URL-based: a Vite library build of a URL worker emits a chunk
// that references sibling assets a downstream bundler does not copy, and the
// only symptom is OPFS quietly disappearing. See docs/DESIGN.md §4.12.
import DbWorker from './worker/index?worker&inline'

/**
 * Live workers, so they can be torn down together.
 *
 * A leaked DB worker is not merely idle: one parked in the OPFS VFS's
 * Atomics.wait holds an exclusive FileSystemSyncAccessHandle, and the next
 * attempt to open that file fails. Vite's HMR replaces modules without
 * terminating workers, so a dev loop would accumulate them.
 */
const live = new Set<Worker>()

function terminateAll(): void {
	for (const w of live) w.terminate()
	live.clear()
}

function createWorker(options: CreateWorkerOptions = {}): Worker {
	if (typeof Worker === 'undefined') {
		throw new Error(
			'sqlite3-wasm-go requires a browser Worker environment; ' +
				'it cannot run during server-side rendering',
		)
	}

	const worker = new DbWorker({ name: options.name ?? 'sqlite3-wasm-go' })
	live.add(worker)

	// Prune on termination, or the set retains every worker handle the Go side
	// has already killed — it calls terminate() directly and never comes back
	// through here.
	const terminate = worker.terminate.bind(worker)
	worker.terminate = () => {
		live.delete(worker)
		terminate()
	}
	return worker
}

const api: Sqlite3WasmGo = { protocolVersion: PROTOCOL_VERSION, createWorker }

;(globalThis as unknown as Record<string, unknown>)[GLOBAL_KEY] = api

// pagehide is a Window event. This module's supported realm is a Worker, where
// registering it would silently never fire, so it is only wired up when there
// is actually a document to hide.
if (typeof document !== 'undefined' && typeof addEventListener === 'function') {
	addEventListener('pagehide', terminateAll)
}
if (import.meta.hot) {
	import.meta.hot.dispose(terminateAll)
}

export { GLOBAL_KEY, PROTOCOL_VERSION }
export type { CreateWorkerOptions, Sqlite3WasmGo }
