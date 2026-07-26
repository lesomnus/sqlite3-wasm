/**
 * The worker that hosts a Go program.
 *
 * Importing the library entry here, rather than leaving it to the consumer, is
 * the point: the global the Go driver looks for has to exist in *this* realm,
 * and importing it on the main thread instead is the mistake that produces a
 * confusing "the library was never imported" at `sql.Open`.
 */

// Installs globalThis["sqlite3-wasm-go"] in this worker.
import '../index'
// Defines the `Go` class.
import '../wasm_exec'

const ctx = self as unknown as {
	postMessage(message: unknown, transfer?: Transferable[]): void
}

export type StartMessage = {
	wasmUrl: string
	argv?: string[]
	env?: Record<string, string>
}

export type RuntimeMessage =
	| { type: 'exit'; code: number }
	| { type: 'error'; message: string }

let started = false

self.onmessage = async (e: MessageEvent<StartMessage>) => {
	if (started) return
	started = true

	const { wasmUrl, argv, env } = e.data
	try {
		const go = new Go()
		if (argv) go.argv = argv
		if (env) go.env = env

		// instantiateStreaming needs the server to send application/wasm; the
		// fallback keeps a misconfigured content type from being fatal.
		const response = await fetch(wasmUrl)
		if (!response.ok) {
			throw new Error(`fetching ${wasmUrl} failed: ${response.status} ${response.statusText}`)
		}
		let instance: WebAssembly.Instance
		try {
			;({ instance } = await WebAssembly.instantiateStreaming(response.clone(), go.importObject))
		} catch {
			const bytes = await response.arrayBuffer()
			;({ instance } = await WebAssembly.instantiate(bytes, go.importObject))
		}

		await go.run(instance)
		ctx.postMessage({ type: 'exit', code: go.exitCode ?? 0 } satisfies RuntimeMessage)
	} catch (err) {
		ctx.postMessage({
			type: 'error',
			message: err instanceof Error ? (err.stack ?? err.message) : String(err),
		} satisfies RuntimeMessage)
	}
}
