// Legacy worker1 bootstrap. Kept alive only so the pre-rewrite examples keep
// running while the binary-protocol transport is landed; deleted in phase 4
// along with the rest of the worker1 path. See docs/PLAN.md.
import * as sqliteWasm from '@sqlite.org/sqlite-wasm'

// The package's index.d.ts declares only the default export, though index.mjs
// re-exports the promiser too.
const { sqlite3Worker1Promiser } = sqliteWasm as unknown as {
	sqlite3Worker1Promiser: { v2(): Promise<unknown> }
}

;(globalThis as Record<string, unknown>)['sqlite-wasm-go'] = () => sqlite3Worker1Promiser.v2()
