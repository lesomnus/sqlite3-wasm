import { expect, test } from 'vitest'

// Exercises the shipped go-worker entry against the *built* bundle, which is
// where it can actually fail: the inlined runtime worker has to carry both the
// Go class and the global installation with it, and both arrive through bare
// side-effect imports that a bundler will happily tree-shake away.

type RunGoWasm = (
	url: string,
	options?: { argv?: string[]; env?: Record<string, string>; name?: string },
) => Promise<{ worker: Worker; exited: Promise<number> }>

async function loadGoWorkerEntry(): Promise<RunGoWasm> {
	const mod = (await import(
		/* @vite-ignore */ '../dist/go-worker.es.js' as string
	)) as unknown as { runGoWasm: RunGoWasm }
	return mod.runGoWasm
}

test('runs a Go program that uses the driver', async () => {
	const runGoWasm = await loadGoWorkerEntry()
	const { default: url } = await import('./examples/driver.wasm?url')

	const { worker, exited } = await runGoWasm(url, { name: 'driver-example' })
	try {
		// The example asserts its way through types, ColumnTypes, named
		// parameters, transactions, int64 extremes and the error model, so a
		// zero exit code means the whole stack worked from a consumer's entry
		// point.
		await expect(exited).resolves.toBe(0)
	} finally {
		worker.terminate()
	}
}, 60_000)

test('reports a program that exits non-zero rather than hanging', async () => {
	const runGoWasm = await loadGoWorkerEntry()
	// A URL that does not resolve to wasm: the failure has to surface as a
	// rejection, not as a promise that never settles. Root-relative on
	// purpose — the runtime worker is a blob: worker and cannot resolve one
	// itself, so this also pins down that the caller absolutises it.
	const { worker, exited } = await runGoWasm('/definitely-not-a.wasm')
	try {
		// The message differs per engine, so only the contract is asserted:
		// it rejects rather than never settling.
		await expect(exited).rejects.toThrow()
	} finally {
		worker.terminate()
	}
}, 30_000)

test('passes argv and env through to the program', async () => {
	const runGoWasm = await loadGoWorkerEntry()
	const { default: url } = await import('./examples/version.wasm?url')

	const { worker, exited } = await runGoWasm(url, {
		argv: ['version', '--flag'],
		env: { SQLITE3_WASM_TEST: '1' },
	})
	try {
		await expect(exited).resolves.toBe(0)
	} finally {
		worker.terminate()
	}
}, 60_000)
