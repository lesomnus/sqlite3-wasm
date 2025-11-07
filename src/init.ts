import {sqlite3Worker1Promiser, type Promiser} from '@sqlite.org/sqlite-wasm'

function factory(): Promise<Promiser> {
	return sqlite3Worker1Promiser.v2()
}

;(globalThis as any)['sqlite-wasm-go'] = factory;
