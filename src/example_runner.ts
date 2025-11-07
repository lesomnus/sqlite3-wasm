import './init'
import "./wasm_exec";

console.log = (message: string) => {
	self.postMessage({type: 'log', message})
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
