# sqlite3-wasm

**sqlite3-wasm** is a bridge that enables **Go (WASM)** code running in the browser to use a **separately loaded SQLite3 WASM instance** through JavaScript.
I hope the [WebAssembly Component Model](https://component-model.bytecodealliance.org/) will soon be fully implemented in browsers so that this project can become **obsolete by design**.


## Acknowledgements

- Portions of the implementation were adapted from [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3).
- Some code in the `driver` package was developed with the assistance of AI tooling.


## Motivation

The project started as a personal experiment to **use SQLite with OPFS directly from Go**.


## Usage

**sqlite3-wasm** uses ['@sqlite.org/sqlite-wasm'](https://github.com/sqlite/sqlite-wasm) to interact with [sqlite3 WASM](https://sqlite.org/wasm/doc/trunk/index.md).

```sh
npm i @sqlite.org/sqlite-wasm
```

The JS side must expose a promiser factory that the Go driver will call to obtain the running SQLite WASM instance:

```ts
import { sqlite3Worker1Promiser } from '@sqlite.org/sqlite-wasm';

(globalThis as any)['sqlite-wasm-go'] = () => sqlite3Worker1Promiser.v2();
```

This code must run in the same global context (main thread or the same Web Worker) where the Go WASM executes and before the Go code tries to open the database.

In your Go code (compiled to WASM):

```go
//go:build js && wasm

package main

import (
	"database/sql"

	_ "github.com/lesomnus/sqlite3-wasm"
)

func main() {
	db, err := sql.Open("sqlite3-wasm", "file:sqlite3.db?vfs=opfs")
	
	// ...
}
```


## with Vite

If you plan to use the OPFS VFS, the page that runs the WASM must be served with the following headers. For a Vite dev server, add these to `vite.config.ts`:

```ts
defineConfig({
	optimizeDeps: { exclude: ['@sqlite.org/sqlite-wasm'] },
	server: {
		headers: {
			'Cross-Origin-Opener-Policy': 'same-origin',
			'Cross-Origin-Embedder-Policy': 'require-corp',
		},
	},
})
```
