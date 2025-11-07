import { describe, test } from "vitest";

describe("examples", () => {
	const doTest = (name: string)=> test(name, () => run(import(`./examples/${name}.wasm?url`)))
	doTest('version')
	doTest('open')
	doTest('query')
	doTest('driver')
})

async function run(pkg: Promise<{default: string}>): Promise<void> {
	const { default: p } = await pkg
	const { default: Runner } = await import('./example_runner?worker')
	
	const runner = new Runner({name: p})
	const result = new Promise<void>((resolve, reject)=>{
		runner.onmessage = ({data})=>{
			switch (data.type) {
				case 'success':
					resolve()
					return
					
				case 'fail':
					reject(data.err)
					return

				case 'log':
					console.log(data.message)
					return
			
				default:
					console.error('unknown message type from the runner: ' + data.type)
					break;
			}
		}
	})

	runner.postMessage(p)
	return result
}
