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

	const worker = new DbWorker({ name: 'sqlite3-wasm-go' })

	if (options.wasmUrl) {
		// Reserved for the inlined build, where the worker cannot resolve a
		// relative URL of its own.
		worker.postMessage({ wasmUrl: options.wasmUrl })
	}

	live.add(worker)
	return worker
}

const api: Sqlite3WasmGo = { protocolVersion: PROTOCOL_VERSION, createWorker }

;(globalThis as unknown as Record<string, unknown>)[GLOBAL_KEY] = api

if (typeof addEventListener === 'function') {
	addEventListener('pagehide', terminateAll)
}
if (import.meta.hot) {
	import.meta.hot.dispose(terminateAll)
}

export { GLOBAL_KEY, PROTOCOL_VERSION }
export type { CreateWorkerOptions, Sqlite3WasmGo }
