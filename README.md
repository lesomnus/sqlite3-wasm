# sqlite3-wasm

**sqlite3-wasm** is a bridge that enables **Go (WASM)** code running in the browser to use a **separately loaded SQLite3 WASM instance** through JavaScript.
I hope the [WebAssembly Component Model](https://component-model.bytecodealliance.org/) will soon be fully implemented in browsers so that this project can become **obsolete by design**.


## Acknowledgements

- Portions of the implementation were adapted from [ncruces/go-sqlite3](https://github.com/ncruces/go-sqlite3).
- The type-conversion rules follow [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3) and [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) closely enough that a database file written by one is read the same by the others; the divergences are listed under [Types](#types).
- Some code in the `driver` package was developed with the assistance of AI tooling.


## Motivation

The project started as a personal experiment to **use SQLite with OPFS directly from Go**.


## How it works

SQLite runs in its own Web Worker, separate from the worker running your Go program, and the two speak a binary protocol.

```
Go worker                            DB worker
  database/sql                         sqlite3 C API
    driver/                            row encoder
      binding/  <--- binary frames --->
                <--- SharedArrayBuffer (cancellation)
```

The split is not incidental: the `opfs` VFS refuses to run outside a Worker and blocks its thread with `Atomics.wait` on every I/O operation, so putting SQLite in the Go worker would freeze the entire Go runtime — every goroutine and timer — for the duration of each file read.

This library does **not** reimplement SQLite. It uses [`@sqlite.org/sqlite-wasm`](https://github.com/sqlite/sqlite-wasm) as shipped and replaces only its two topmost JavaScript layers — the `oo1` convenience wrapper and the `worker1` message protocol — because neither can carry a value's storage class or a column's declared type across the boundary. See [docs/DESIGN.md](docs/DESIGN.md).


## Usage

```sh
npm i sqlite3-wasm-go
```

Import the package **from inside the worker that runs your Go program**. It installs the global the Go driver looks for; importing it on the main thread installs it in the wrong realm.

```ts
// db-worker.ts — the worker that hosts your Go program
import 'sqlite3-wasm-go'
import './wasm_exec'

const go = new Go()
const { instance } = await WebAssembly.instantiateStreaming(fetch(wasmUrl), go.importObject)
await go.run(instance)
```

In your Go code:

```go
//go:build js && wasm

package main

import (
	sqlitewasm "github.com/lesomnus/sqlite3-wasm"
)

func main() {
	db, err := sqlitewasm.OpenDB("file:app.db?vfs=opfs")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	// ... ordinary database/sql from here
}
```

`sqlitewasm.OpenDB` is `sql.Open` plus `SetMaxOpenConns(1)`. Use `sql.Open("sqlite3-wasm", dsn)` directly if you want to choose the pool settings yourself — but read [Concurrency](#concurrency) first.


## DSN

```
file:app.db?vfs=opfs&_loc=Asia/Seoul&_fk=1
```

Standard SQLite URI parameters are passed through to `sqlite3_open_v2`. Driver parameters are `_`-prefixed and stripped.

| parameter | meaning |
| --- | --- |
| `vfs` | `opfs`, `opfs-sahpool`, `memdb`, or unset for a transient database |
| `_loc` / `_timezone` | location for naive timestamps: `UTC` (default), `auto`, or an IANA name |
| `_time_format` | how a `time.Time` is written: `offset` (default), `utc`, `datetime` |
| `_time_integer_format` | write times as integers: `unix`, `unix_milli`, `unix_micro`, `unix_nano` |
| `_fk` / `_foreign_keys` | `PRAGMA foreign_keys`, default on |
| `_txlock` | `immediate` (default), `deferred`, `exclusive` |

Two parameters are **rejected with an error** rather than silently ignored:

- **`cache=shared`** — this build has `SQLITE_OMIT_SHARED_CACHE`, so SQLite accepts the URI and then gives each connection its own invisible database. Use `vfs=memdb` for a shared in-memory database.
- **`_busy_timeout`** — SQLite's busy handler sleeps with `Atomics.wait` on the database worker's own thread, and the `COMMIT` that would release the lock can only be delivered when that thread returns to its event loop. Any non-zero timeout is a self-deadlock for its full duration. `SQLITE_BUSY` is retried on the Go side instead.

`:memory:` and `mode=memory` are rewritten to `file:/<generated>?vfs=memdb`, so every connection in a pool sees one database instead of several invisible ones.

Under `GOOS=js`, `time.Local` is UTC unless you embed tzdata, so `_loc=auto` and `_loc=UTC` behave the same by default.


## Types

Values keep their SQLite storage class across the boundary. An `INTEGER` is an `int64` even past 2^53; a `REAL` stays a `float64` even when its value is integral; `TEXT` keeps embedded NULs; and `NULL`, `''`, `x''` and `zeroblob(0)` stay distinct.

Conversion is keyed on the column's **declared type**, which SQLite reports for real columns and which survives views, joins, CTEs, subqueries, aliases and `RETURNING`.

| declared type | value |
| --- | --- |
| `DATE`, `DATETIME`, `TIMESTAMP` | `time.Time`, or the raw `string` if no layout matches |
| `BOOLEAN` | `bool` (from an `INTEGER`) |
| anything else | the storage class as-is |

Deliberate choices worth knowing:

- **A failed timestamp parse yields the original string**, not the zero time. A silent `0001-01-01` is unrecoverable; a string produces a debuggable `unsupported Scan` at the call site.
- **`REAL` is never read as a time.** A Julian day number is indistinguishable from any other float, and both incumbent drivers agree.
- **`BOOL` is not treated as a boolean, only `BOOLEAN`** — mattn/go-sqlite3 does the same, so a `BOOL` column keeps scanning into an integer for anyone migrating.
- **`TIME` is not treated as a timestamp.** SQLite's `time()` emits `HH:MM:SS`, which no layout parses, so including it would turn a usable string into a failed parse.
- `sqlite3_column_decltype` is null for expressions and aggregates, so `SELECT MAX(created_at)` has no declared type. Rather than guess-parse every date-shaped string — which would turn a user named `2024-01-02` into a timestamp — conversion there is opt-in per destination:

  ```go
  var t sqlitewasm.Time
  err := db.QueryRow("SELECT MAX(created_at) FROM events").Scan(&t)
  ```

`ColumnTypeDatabaseTypeName` and `ColumnTypeScanType` are derived from the declared type, so `rows.ColumnTypes()` is answerable before the first `Next`. `ColumnTypeNullable` is not implemented: this wasm build lacks `sqlite3_column_origin_name`, so a result column cannot be traced back to a base table.


## Errors

```go
if errors.Is(err, sqlitewasm.ErrConstraint) { ... }

var e *sqlitewasm.Error
if errors.As(err, &e) {
	fmt.Println(e.Code, e.ExtendedCode, e.Offset, e.Message)
}
```

`Error` carries the primary result code, the extended code (so `SQLITE_CONSTRAINT_UNIQUE` is distinguishable from `SQLITE_CONSTRAINT_FOREIGNKEY`), the message, and `sqlite3_error_offset` for syntax errors. `errors.Is` compares the primary code only.


## Concurrency

**Use one connection.** `sqlitewasm.OpenDB` does this for you.

There is a single JavaScript thread behind every connection, so more connections buy no parallelism. What they do buy is `SQLITE_BUSY`: this build has no WAL — neither OPFS VFS implements `xShmMap` — so readers block writers under rollback-journal locking. And a second connection to the same **file-backed** database is refused outright, because two handles contend for the same exclusive OPFS sync access handle, which costs about 4.5 s of `Atomics.wait` inside the worker before failing anyway.

`PRAGMA journal_mode=WAL` is therefore unavailable. On `memdb` it reports `memory`.


## Cancellation

Cancelling a context interrupts a running statement through a `SharedArrayBuffer` word polled by `sqlite3_progress_handler`. `sqlite3_interrupt` cannot be used: the wasm memory is not shared and the build is `THREADSAFE=0`.

It requires cross-origin isolation (for `SharedArrayBuffer`) and a CSP that permits `wasm-unsafe-eval`. Without either, a cancelled context returns `ctx.Err()` promptly and the connection is discarded rather than reused, but the statement runs to completion in the worker. Cancellation also cannot interrupt a statement stalled inside VFS I/O, because the progress handler does not fire there.


## Browser requirements

The floor is roughly **Chrome 80 / Firefox 114 / Safari 16.4**: nested module workers, and OPFS sync access handles for persistence.

### Headers

The `opfs` VFS needs cross-origin isolation:

```
Cross-Origin-Opener-Policy: same-origin
Cross-Origin-Embedder-Policy: require-corp
```

Without them, `vfs=opfs` fails with an error naming the missing header rather than silently falling back to a database that loses everything on reload. `vfs=opfs-sahpool` is the persistence tier that needs neither.

### Content Security Policy

| directive | why |
| --- | --- |
| `worker-src blob:` | the database worker is a blob URL |
| `script-src 'wasm-unsafe-eval'` | installing the cancellation progress handler builds a `WebAssembly.Module` at runtime |

### Vite

```ts
defineConfig({
	server: {
		headers: {
			'Cross-Origin-Opener-Policy': 'same-origin',
			'Cross-Origin-Embedder-Policy': 'require-corp',
		},
	},
})
```

### Next.js

Set the same two headers in `next.config.js` under `headers()`, and load the module client-side only (`next/dynamic` with `ssr: false`, or a `'use client'` module) — importing it during SSR has no `Worker` to spawn.


## Development

```sh
npm install
npx playwright install chromium

go test ./...   # wire codec, DSN, declared types, time, conversion
npm test        # builds the examples and the bundle, then runs both tiers
```

`go test ./...` runs on the host: the wire codec, the DSN parser, the declared-type classifier and the time layouts all carry no build tag, so the bulk of the driver's semantics is testable without a browser. Everything that touches sqlite3, workers, OPFS or Go/wasm runs under vitest browser mode.

The wire format has a third implementation in `scripts/gen-wire-vectors.py`, which produces the golden corpus both codecs are checked against, so neither can bless its own bug as the specification.

See [docs/PLAN.md](docs/PLAN.md) for the state of the rewrite, [docs/DESIGN.md](docs/DESIGN.md) for why it is shaped this way, and [docs/PROTOCOL.md](docs/PROTOCOL.md) for the wire format.
