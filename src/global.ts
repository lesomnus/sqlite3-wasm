/**
 * The single point of contact between the JavaScript half of this library and
 * the Go half.
 *
 * The key and the protocol version are defined here and mirrored in
 * `binding/global.go`; keeping them in one named constant on each side is what
 * stops them drifting, which they already had (the repository previously spelled
 * this three different ways).
 */

/** The property the Go program reads off `globalThis`. */
export const GLOBAL_KEY = 'sqlite3-wasm-go'

/**
 * The pre-rewrite key, whose value was a bare promiser factory. It is checked
 * for only so a stale bundle produces a real message instead of a hang.
 */
export const LEGACY_GLOBAL_KEY = 'sqlite-wasm-go'

export type CreateWorkerOptions = {
	/** A name for the worker, as it appears in devtools. */
	name?: string
}

export type Sqlite3WasmGo = {
	/** Must match `wire.Version` on the Go side. */
	protocolVersion: number
	/** Spawns a database worker. The caller owns terminating it. */
	createWorker(options?: CreateWorkerOptions): Worker
}
