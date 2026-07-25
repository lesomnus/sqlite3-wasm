import { describe, test } from "vitest";

describe("examples", () => {
	const doTest = (name: string) => test(name, () => run(name), 60_000)
	doTest('version')
	doTest('open')
	doTest('query')
	doTest('driver')
	doTest('conformance')
})

async function run(name: string): Promise<void> {
	const { default: url } = await import(`./examples/${name}.wasm?url`)
	const { default: Runner } = await import('./example_runner?worker')
	const runner = new Runner({ name })

	try {
		await new Promise<void>((resolve, reject) => {
			// Without these the only symptom of a worker that fails to
			// construct -- or of a grandchild DB worker that throws -- is a
			// bare vitest timeout with no message.
			const timer = setTimeout(() => reject(new Error(`timeout running ${name}`)), 45_000)
			const settle = (fn: () => void) => { clearTimeout(timer); fn() }

			runner.onerror = (e) => settle(() => reject(new Error(e.message ?? String(e))))
			runner.onmessageerror = () => settle(() => reject(new Error('messageerror')))
			runner.onmessage = ({ data }) => {
				switch (data.type) {
					case 'success': settle(resolve); return
					case 'fail': settle(() => reject(new Error(data.err))); return
					case 'log': console.log(data.message); return
					default: console.error('unknown message type from the runner: ' + data.type)
				}
			}
			runner.postMessage(url)
		})
	} finally {
		// A leaked DB worker parked in the OPFS VFS holds an exclusive sync
		// access handle, and the next test to open that file fails. Nested
		// workers are terminated with their owner, so this is enough.
		runner.terminate()
	}
}
