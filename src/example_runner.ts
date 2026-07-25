import './index'
import "./wasm_exec";

// Go writes panics and stack traces to stderr, which lands on console.error.
// Forwarding only console.log meant a failing example surfaced as a bare
// "exit code 2" with the reason nowhere to be seen.
console.log = (message: string) => {
	self.postMessage({ type: 'log', message })
}
console.error = (message: string) => {
	self.postMessage({ type: 'log', message: '[stderr] ' + message })
}

async function run(p: string){
	const go = new Go();
	const { instance } = await WebAssembly.instantiateStreaming(fetch(p), go.importObject);

	await go.run(instance)
	if(go.exitCode === undefined){
		throw new Error('no exit code')
	}
	if(go.exitCode > 0){
		throw new Error(`exit with non-zero exit code: ${go.exitCode}`)
	}
}

self.onmessage = async e=> {
	const p = e.data
	try {
		await run(p)
		self.postMessage({type: 'success'})
	} catch(e){
		self.postMessage({type: 'fail', err: String(e)})
	}
}
