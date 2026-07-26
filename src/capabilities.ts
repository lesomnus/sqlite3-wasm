/**
 * Engine capability probes, so tests can skip what an engine genuinely cannot
 * do instead of asserting a browser-specific truth everywhere.
 *
 * Only used by tests. The library itself learns the same facts from the
 * worker's READY frame, which is authoritative because it is measured inside
 * the worker rather than guessed from the page.
 */

/**
 * Whether this engine exposes OPFS at all.
 *
 * Playwright's Linux WebKit build has no `navigator.storage.getDirectory` and
 * no `FileSystemFileHandle`, so every OPFS tier is meaningless there. That is a
 * property of *that build*, not of Safari, which has supported OPFS since 15.2
 * and sync access handles since 17 — so a skip here is not evidence about
 * Safari either way.
 */
export function hasOpfs(): boolean {
	return (
		typeof navigator !== 'undefined' &&
		typeof navigator.storage?.getDirectory === 'function' &&
		typeof (globalThis as Record<string, unknown>).FileSystemFileHandle !== 'undefined'
	)
}

/** Whether the page is cross-origin isolated, which OPFS and cancellation need. */
export function isCrossOriginIsolated(): boolean {
	return globalThis.crossOriginIsolated === true && typeof SharedArrayBuffer === 'function'
}
