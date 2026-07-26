/**
 * Spawns a worker that runs a Go program with the SQLite driver available to it.
 *
 * This exists so "the consumer writes no JavaScript glue" is actually true.
 * Without it every consumer hand-writes the same ~25 lines — vendoring
 * `wasm_exec.js`, instantiating, running, relaying errors — which is the most
 * failure-prone file in the stack and the one place where importing the library
 * into the wrong realm silently breaks everything.
 *
 * What the consumer still owns is resolving their own Go `.wasm` URL, because
 * only their bundler can do that:
 *
 * ```ts
 * import { runGoWasm } from 'sqlite3-wasm-go/go-worker'
 * import appWasm from './app.wasm?url'          // Vite
 *
 * const { worker, exited } = await runGoWasm(appWasm)
 * console.log('exit code', await exited)
 * ```
 */

import type { RuntimeMessage, StartMessage } from './go-worker/runtime'

export type RunGoWasmOptions = {
	/** `os.Args`. The first element is the program name. */
	argv?: string[]
	/** Environment variables visible to `os.Getenv`. */
	env?: Record<string, string>
	/** A name for the worker, as it appears in devtools. */
	name?: string
}

export type GoWasmRun = {
	/** The worker running the program. Terminating it kills the program. */
	worker: Worker
	/**
	 * Resolves with the program's exit code, or rejects if it could not be
	 * started. A non-zero exit is a resolution, not a rejection: it is a normal
	 * outcome for a program.
	 */
	exited: Promise<number>
}

/**
 * Asynchronous so the ~1.6 MB runtime — sqlite3, the database worker and the Go
 * runtime, all inlined — is code-split behind a dynamic import instead of
 * sitting on the importing page's critical path. Callers pay for it the first
 * time they actually start a program.
 */
export async function runGoWasm(
	wasmUrl: string | URL,
	options: RunGoWasmOptions = {},
): Promise<GoWasmRun> {
	if (typeof Worker === 'undefined') {
		throw new Error(
			'sqlite3-wasm-go: runGoWasm requires a browser Worker environment; ' +
				'it cannot run during server-side rendering',
		)
	}

	const { default: GoRuntimeWorker } = await import('./go-worker/runtime?worker&inline')
	const worker: Worker = new GoRuntimeWorker({ name: options.name ?? 'go-wasm' })

	const exited = new Promise<number>((resolve, reject) => {
		worker.onmessage = (e: MessageEvent<RuntimeMessage>) => {
			const data = e.data
			if (data?.type === 'exit') resolve(data.code)
			else if (data?.type === 'error') reject(new Error(data.message))
		}
		// Without these a worker that fails to construct — a CSP block, an
		// engine without module workers — surfaces as a promise that never
		// settles.
		worker.onerror = (e) => reject(new Error(e.message ?? 'the Go worker failed to start'))
		worker.onmessageerror = () =>
			reject(new Error('the Go worker sent an undeserialisable message'))
	})

	const start: StartMessage = {
		// Absolute, resolved here rather than in the worker. The runtime worker
		// is a blob: worker, whose base URL is the blob URL itself, so a
		// root-relative path — which is exactly what a bundler's asset import
		// hands you — fails there with "Failed to parse URL". This realm has a
		// real location to resolve against.
		wasmUrl: new URL(String(wasmUrl), location.href).href,
		argv: options.argv,
		env: options.env,
	}
	worker.postMessage(start)

	return { worker, exited }
}

export type { RuntimeMessage, StartMessage }
