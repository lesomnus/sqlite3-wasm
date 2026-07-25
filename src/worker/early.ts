/**
 * Installs the worker's message listener before anything can await.
 *
 * This module must be the DB worker's *first* import. ES modules evaluate
 * depth-first in source order, so importing this one before
 * `@sqlite.org/sqlite-wasm` guarantees `self.onmessage` exists before any
 * dependency's top-level await yields control.
 *
 * That ordering is load-bearing, not defensive. Measured in Chromium 141:
 * messages posted immediately after `new Worker()` are **silently dropped** if
 * the worker registers `onmessage` after a top-level await — no error, no
 * rejection, the sender just blocks until its deadline.
 */

const pending: unknown[] = []
let sink: ((data: unknown) => void) | null = null

self.onmessage = (e: MessageEvent) => {
	if (sink) sink(e.data)
	else pending.push(e.data)
}

/** Hands over to the real dispatcher and drains anything buffered so far. */
export function setSink(f: (data: unknown) => void): void {
	sink = f
	const buffered = pending.splice(0, pending.length)
	for (const data of buffered) f(data)
}
