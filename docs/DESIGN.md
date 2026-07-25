# Design: binary-protocol rewrite

Status: **accepted** — see [PLAN.md](./PLAN.md) for implementation progress.

This document explains *why* the driver is being rewritten and *what* the new
shape is. The normative wire format lives in [PROTOCOL.md](./PROTOCOL.md).

---

## 1. What is wrong today

The current driver talks to SQLite through `@sqlite.org/sqlite-wasm`'s **worker1**
API (`sqlite3Worker1Promiser`). Every row value therefore crosses the JS boundary
via `oo1.Stmt.get()`, and that conversion is lossy:

| SQLite storage class | what `Stmt.get()` produces | consequence |
| --- | --- | --- |
| `INTEGER` (within ±2^53) | `Number` | indistinguishable from `REAL` |
| `INTEGER` (outside ±2^53) | **`BigInt`** | crashes Go, see below |
| `FLOAT` | `Number` | indistinguishable from `INTEGER` |
| `TEXT` | `string` | truncated at an embedded NUL |
| `BLOB` | `Uint8Array` | fine |

There is no slot anywhere in the worker1 message protocol for a column's declared
type, so the driver cannot know that a column is a `DATETIME`.

Three concrete defects follow, all reproduced in this repository against
Chromium 141 before any code was changed:

1. **`REAL` is destroyed.** `driver/rows.go` coerced every integral `float64` to
   `int64`, because that was the only way to make `BOOLEAN` columns scannable.
   `SELECT 1.0` returned `int64(1)`.

2. **`int64` beyond 2^53 panics the Go program.** `sqlite3.mjs:11218-11227`
   returns a `BigInt`; Go's `wasm_exec.js` assigns `typeof "bigint"` a type flag
   of 0 (`src/wasm_exec.js:179-192`), and `syscall/js.Value.Type()` reaches its
   `default: panic("bad type flag")` (`$GOROOT/src/syscall/js/js.go:288`).
   Observed stack: `jz.unmarshal → syscall/js.Value.Type → panic`.
   `makeValue` also skips the finalizer for such a value
   (`js.go:49-50`), so the JS reference slot leaks permanently.
   `bigIntEnabled` defaults to `!!globalThis.BigInt64Array`
   (`sqlite3.mjs:7411-7414`) — i.e. **on** in every browser.

3. **Dates were guessed from the column name.** `driver/rows.go:82` treated any
   column named `date_*` or `*_at` as a timestamp, tried exactly one layout, and
   **returned `nil` on failure** — silently discarding data. The one layout was
   `"2006-01-02 15:04:05.999999999 -0700 MST"` because writes went through
   `fmt.Sprintf("%v", time.Time)` in `driver/utils.go:192`, so only values written
   by this driver could ever be read back.

Beyond the type system, the audit found:

- `substituteParams` (`driver/utils.go:88`) is string interpolation: SQL injection,
  NUL truncation, and it rewrites `?` inside string literals.
- `binding.NewPromiser` spawns a **new Worker per connection**, so a `database/sql`
  pool of N connections was N mutually invisible `:memory:` databases.
- `binding/promiser.go:87` sends on an **unbuffered channel from inside a
  `js.FuncOf` callback**. When no receiver is parked this wedges the JS event loop
  and busy-spins the Go runtime into `fatal error: all goroutines are asleep`
  (`runtime/lock_js.go:243-247`). The three `js.Func`s at `promiser.go:138-146` are
  never `Release()`d — three leaks per query.
- `RowsAffected` was inferred from `strings.HasPrefix(query, "UPDATE")`
  (`driver/conn.go:74`), missing `INSERT`, `DELETE`, lowercase, and leading whitespace.
- worker1 posts **one `postMessage` per row**.

## 2. Why not just patch worker1

Everything in §1 that matters requires two facts to cross the boundary that
worker1 never sends: the per-value **storage class**
(`sqlite3_column_type`) and the per-column **declared type**
(`sqlite3_column_decltype`). Both exist in the wasm build; neither is reachable
through the worker1 message protocol. So the protocol has to be ours.

Once we own the protocol, the per-row `postMessage` and the per-cell
`syscall/js` traffic go away too. The old path costs, measured with Go 1.26 under
Node 24:

| operation | cost |
| --- | --- |
| `js.Value.Index()` → number | 102–110 ns |
| `js.Value.Get()` → object/string | 1.9–3.3 µs (dominated by `runtime.SetFinalizer` in `makeValue`) |
| decode 5 000 cells via per-cell `Index` | **51.3 ms** |
| decode 5 000 cells via one `CopyBytesToGo` + pure Go | **0.90 ms** |

57× on decoding, ~146× on messaging. The win is not the memory copy — it is never
materialising a `js.Value` per cell.

### 2.1 …but the boundary is not where the time goes

Having removed that, the remaining per-cell budget was measured again, this time in
Chromium under cross-origin isolation with a real worker→worker link and the real
sqlite3 build. It reorders the priorities completely:

| stage, per cell of a 4-column scan | cost |
| --- | --- |
| sqlite3 extract + encode via `capi.*` wrappers | **634 ns** |
| the same via raw `wasm.exports.*` | **86 ns** |
| `postMessage` transport at 256 KiB batches | ~1 ns |
| `js.CopyBytesToGo` | ~0.3 ns |
| pure-Go parse | 0.58 ns |
| boxing into `driver.Value` | 11 ns |

Over 70 % of the cost of a row is `wasm.xWrap` argument/result adapters inside the
`capi.*` functions — not the wire format, not the transport, not Go. The entire
columnar-vs-row-major and varint-vs-fixed64 question is worth single-digit
nanoseconds against a 634 ns baseline; both were benchmarked and both are noise.

Latency tells the same story:

| operation | cost |
| --- | --- |
| worker↔worker `postMessage` round trip | **32 µs**, flat from 64 B to 256 KiB |
| the same at 1 MiB | 128 µs |
| `SharedArrayBuffer` + `Atomics` round trip | 8.7 µs |
| cached indexed 1-row query inside sqlite3 | **0.97 µs** |
| `sqlite3_prepare_v3` + `sqlite3_finalize` | 4.6 µs |

A point query — the dominant ORM call — is ~1 µs of SQLite work. Spending two round
trips (64 µs) plus a recompile (4.6 µs) on it would make transport 33× the query.

So three decisions matter far more than the encoding, and all three are in §4.6–4.8:
**raw exports in the row loop**, **one fused round trip per query**, and **a
statement cache in the worker**.

## 3. Architecture

```
┌─ Go worker ─────────────────────────────┐
│  Go/wasm  (GOOS=js GOARCH=wasm)         │
│    database/sql                         │
│      driver/     database/sql driver    │
│        binding/  transport + codec      │
│          syscall/js                     │
└──────────────┬──────────────────────────┘
               │  postMessage(Uint8Array, [buffer])   ← binary frames
               │  SharedArrayBuffer                   ← abort word
┌──────────────▼──────────────────────────┐
│  DB worker (blob module worker)         │
│    sqlite3InitModule() → capi + wasm    │
│    frame dispatcher                     │
│    row encoder (reads the wasm heap)    │
└─────────────────────────────────────────┘
        │
        └─ OPFS async proxy worker (spawned by the OPFS VFS itself)
```

The two-worker split is not incidental. The `opfs` VFS refuses to run outside a
Worker and blocks its own thread with `Atomics.wait` on every I/O operation
(`sqlite3.mjs:11923-11928`, `:12191-12197`). Putting SQLite in the Go worker would
freeze the entire Go runtime — every goroutine and timer — for the duration of each
file read. Keeping them apart is what makes the Go side stay responsive.

Go never blocks: `Atomics.wait` from Go works but would freeze its event loop, so
Go only ever *stores* into the shared word.

## 4. Design decisions

### 4.1 Binary frames, one `CopyBytesToGo` per batch

Every message in both directions is a single `Uint8Array` whose buffer is
transferred. Posting the **view** (not the `ArrayBuffer`) matters:
`js.CopyBytesToGo` accepts only `Uint8Array`/`Uint8ClampedArray`
(`wasm_exec.js:449`), so `evt.data` arriving as a `Uint8Array` can be copied
directly with no wrapper allocation.

`int64` is written with `DataView.setBigInt64`, so a `BigInt` value never becomes a
`js.Value`. This applies to `changes64` and `last_insert_rowid` too, not just column
values.

### 4.2 Read column bytes straight off the wasm heap

`sqlite3_column_blob` + `sqlite3_column_bytes` hand back the raw UTF-8 bytes of a
`TEXT` value with no JS string materialisation (verified: `'héllo☃'` → 9 bytes).
Two hazards, both confirmed, both designed around:

- Calling `sqlite3_column_blob` on an `INTEGER`/`REAL` column **silently rewrites
  the value to its text rendering** while `sqlite3_column_type` keeps reporting
  `SQLITE_INTEGER`. So the encoder must branch on `sqlite3_column_type` **first**.
- `NULL`, `''`, `x''` and `zeroblob(0)` all yield `ptr=0, bytes=0`. Only
  `sqlite3_column_type` separates them.

`wasm.heap8u()` returns a view that is genuinely detached when wasm memory grows
(measured: `.length === 0` after a 64 MiB alloc), so it is re-fetched after every
capi call and never cached.

### 4.3 Declared types drive conversion

`sqlite3_column_decltype` is captured once at prepare time and normalised
(uppercase, trim, cut at the first `(` so `VARCHAR(255)` → `VARCHAR`). It is used
for both value conversion and `ColumnTypeScanType` — a single normalised value,
avoiding the lower/upper asymmetry mattn/go-sqlite3 has.

decltype is more robust than expected. Verified against SQLite 3.50.4: it survives
**views, joins, CTEs, subqueries, column aliases and `INSERT ... RETURNING`**. It is
`NULL` only for aggregates and expressions (`max(dt)`, `coalesce(dt, dt)`). One trap
worth documenting: `SELECT a FROM t UNION ALL SELECT b FROM u` reports the **first
arm's** decltype for the whole column.

The conversion table is in [PROTOCOL.md §9](./PROTOCOL.md#9-type-mapping).

`RowsColumnTypeNullable` is **not implemented**. Returning `ok=false` is
byte-for-byte identical to omitting the method (`sql.go:3295-3297`), so it would be
dead code. The reason it cannot be supported is *not* that
`sqlite3_table_column_metadata` is missing — that function is exported and works —
but that `sqlite3_column_origin_name` / `_table_name` / `_database_name` are absent
from this wasm build, so a result column cannot be mapped back to a base table.

### 4.4 Cancellation: a generation word in a SharedArrayBuffer

`sqlite3_interrupt` is useless here: the wasm memory is not shared (import section:
`MEMORY flags=1 NOT shared`), there are no pthread imports, and the build is
`THREADSAFE=0`. A message-based interrupt cannot be dequeued either, because the
worker is inside `sqlite3_step`.

So: one `Int32Array` slot per open database in a `SharedArrayBuffer`, and one
`sqlite3_progress_handler` installed per database at OPEN time. The word holds a
**request id**, not a boolean — a boolean would let a late `ctx.Done()` abort the
*next*, unrelated statement. The handler aborts only when
`Atomics.load(word) === currentRequestId`.

Limits, to be documented rather than hidden:

- The progress handler does not fire inside the VFS, so a statement stalled in an
  OPFS `Atomics.wait` is not interruptible.
- Installing the handler goes through `jsFuncToWasm`, which builds a
  `WebAssembly.Module` at runtime and therefore needs CSP `script-src
  'wasm-unsafe-eval'`. Installation is probed at OPEN; on failure the connection
  reports `cancelSupported=false` and takes the degraded path below.
- Without `SharedArrayBuffer` (no cross-origin isolation) there is no cancellation
  at all.

Degraded path, when cancellation is unavailable: return `ctx.Err()` immediately,
mark the connection poisoned so `Validator.IsValid()` returns false and
`ResetSession` returns `driver.ErrBadConn`, and discard late frames for the dead
request. Never return `driver.ErrBadConn` from the operation itself — `db.retry`
would re-run a statement that may already have executed, up to three times
(`sql.go:1569-1584`).

### 4.5 Streaming: eager push, geometric batches, a yield, and a credit window

Row batches are pushed eagerly — one request produces N result messages with no
per-batch round trip. This genuinely works in the direction that matters: a worker
that posts a batch and then busy-spins for 100 ms without yielding still has each
batch delivered to its parent ~0.3 ms after posting. (The rule "never `postMessage`
then block in the same task" applies to the parent→nested direction combined with
`Atomics.wait` on the receiver; it does not forbid eager outbound streaming.)

Three refinements, each with a measured reason:

**Geometric batch growth.** A fixed 1024-row threshold means `rows.Next()` blocks
until 1024 rows have been stepped, and a query abandoned after one row pays for a
whole batch. One-way push costs 4.4 µs at 256 B and 29.6 µs at 256 KiB — the fixed
per-message cost is tiny, so a small first batch is nearly free. Batches therefore
grow 1 → 8 → 64 → 512 → 1024 rows (1 KiB → 4 KiB → 32 KiB → 256 KiB by bytes). The
first row reaches Go after one step; steady-state amortisation is unchanged.

256 KiB stays a hard ceiling. Round-trip time is flat at 31.8–37.4 µs from 4 KiB to
256 KiB but jumps to 127.6 µs at 1 MiB — that is allocation and zeroing of the
payload, not transfer, which is O(1) for a detachable buffer.

**A yield between batches.** One DB worker serves every connection on a Connector.
Without yielding, a 100 000-row scan sits in its step loop for ~34 ms and cannot
dispatch anything else — head-of-line blocking that `database/sql` has no visibility
into. So the worker returns to its event loop after each flush. It must be a
*macrotask*: `await null` does not drain the `postMessage` task queue, and
`setTimeout(…, 0)` is clamped to 4 ms once nested more than five deep, so the yield
uses a `MessageChannel` self-ping. Cost: one macrotask per batch.

**A credit window.** The worker may have at most `CREDIT_WINDOW` (4) unacknowledged
batches in flight. Without a bound, a slow `Rows.Next` consumer accumulates a whole
result set in memory twice — a 500 MB table takes the tab down. Because the worker
already yields per batch, checking credit is nearly free. This is a *window*, not a
request/response per batch; the distinction is the point.

`Rows.Close` must **never block** — it is called from `awaitDone` while holding the
connection mutex (`sql.go:3443`, `:3455-3457`), so a blocking Close pins a pooled
connection for as long as the worker takes. After EOF it sends nothing at all; before
EOF it sends a fire-and-forget `ABORT` and stops granting credit. Batches already in
flight for a dead request are discarded, not treated as protocol errors.

### 4.6 The row loop uses raw wasm exports

`sqlite3.capi.*` functions are `wasm.xWrap`-generated wrappers that run per-argument
adapters (pointer coercion, result adapters) on every call. On a 50 000-row,
4-column scan:

| call | via `capi.*` | via `wasm.exports.*` |
| --- | --- | --- |
| `sqlite3_step` | 304.9 ns/row | 108.5 ns/row |
| `sqlite3_column_type` | 264.3 ns/cell | 33.3 ns/cell |
| `sqlite3_column_int64` | 343.0 ns/cell | 45.5 ns/cell |
| full extract + encode loop | **634.4 ns/cell** | **85.7 ns/cell** |

On a 100 000 × 4 scan that is 254 ms versus 34 ms.

`wasm.exports` is documented public surface (`index.d.ts:2286-2300`, "available for
those who want to make use of it") and is present in the bundler-friendly build. The
hot-loop functions all take and return only `i32`/`f64`/`i64`, so no marshalling is
needed and none is lost — raw `sqlite3_column_int64` still returns a JS `BigInt`
(wasm `i64` integration) and raw `sqlite3_column_blob` still returns an `i32` heap
pointer, exactly what the encoder wants.

`capi.*` stays for the cold path — `prepare_v3` with heap-copied SQL, `open_v2`,
`errmsg`, `error_offset`, `bind_text`/`bind_blob`, `decltype`, `parameter_name` —
where the adapters earn their keep.

### 4.7 One fused round trip per query

`db.Query(sql, args...)` must not cost PREPARE + QUERY + FINALIZE. At a flat 32 µs
per round trip that is 64 µs of transport plus 4.6 µs of recompilation wrapped
around ~1 µs of work.

So `QUERY{dbId, sql, values}` is fused exactly like `EXEC`: prepare, bind, step,
stream, auto-finalise, all inside one dispatcher call. Column names and decltypes
are answerable immediately after prepare — before the first step — so they ride in
the header of the first `ROWS` frame instead of a separate `PREPARED` reply.
`QueryerContext` and `ExecerContext` use this op; the explicit `PREPARE` op exists
only for `ConnPrepareContext` (`db.Prepare`).

Halving a point query's wall clock (~71 µs → ~33 µs) is a bigger win than every
encoding decision combined.

### 4.8 A statement cache in the DB worker

Even fused, every call otherwise recompiles. Measured against an indexed 1-row query:

| path | cost |
| --- | --- |
| `prepare_v3` + `finalize` alone | 4 589 ns |
| uncached `prepare + bind + step + read + finalize` | 7 155 ns |
| cached `reset + bind + step + read` | **972 ns** |
| `reset` + `clear_bindings` on release | 390 ns |

That is 6.2 µs of waste per query — ~19 % of a round trip — and it grows with SQL
length, so a 500-character ORM `SELECT` costs far more than the 30-character probe.

An LRU of `sqlite3_stmt*` keyed by `(dbId, sql)`, default 64 entries per handle,
prepared with `SQLITE_PREPARE_PERSISTENT`. Released with `sqlite3_reset` +
`sqlite3_clear_bindings`. Invalidated on `SQLITE_SCHEMA` (17) from step or reset, and
on any DDL routed through `EXEC`. Statements whose prepare tail was non-empty are
never cached.

The cache belongs in the worker, not in Go: a Go-side cache could only avoid the
compile, not the round trip.

### 4.9 Decode lazily into the destination slice

95 % of the Go-side cost is boxing into the `driver.Value` interface, not parsing.
Measured natively on 1000 × 8 `int64` cells: decoding into `[]driver.Value` costs
11.55 ns/cell with 7 744 allocations per 8 000 cells; the identical bytes into
`[]int64` cost 0.58 ns/cell with zero allocations. `TEXT` is worse — 39.7 ns/cell,
dominated by the `string(...)` allocation.

So a batch is never materialised as `[][]driver.Value`. The `[]byte` stays alive for
the life of the batch and `Rows.Next` decodes one row at a time directly into the
`dest []driver.Value` slice that `database/sql` reuses across calls. This is natural
with the row-major layout, and is a concrete reason to keep it.

The boxing cost itself is not worth chasing — 11 ns against 86 ns of unavoidable
JS-side work per cell — but multiplying it by materialising whole batches up front is.

### 4.10 One database connection, deliberately

This is the decision most likely to surprise. The wasm build has
`SQLITE_OMIT_SHARED_CACHE` and `THREADSAFE=0`, and neither OPFS VFS implements
`xShmMap` (`opfsIoMethods.$iVersion = 1`), so **WAL is unavailable**. That leaves
rollback-journal locking, where readers block writers.

Worse, SQLite's busy handler sleeps with `Atomics.wait` **on the DB worker's own
thread** (`sqlite3.mjs:12585-12586`). The message that would release the lock — a
`COMMIT` from another connection — can only be dequeued when that thread returns to
its event loop. So a non-zero busy timeout is a guaranteed self-deadlock for its
full duration. Measured: with connection A in `BEGIN IMMEDIATE`, connection B's
`INSERT` blocks the worker for 1001 ms with `busy_timeout=1000` and then returns
`SQLITE_BUSY` anyway.

OPFS is worse still: `getSyncHandle` creates one exclusive
`FileSystemSyncAccessHandle` per opened file, and a second one retries six times
with escalating backoff (≈4.5 s of `Atomics.wait`) before throwing.

Therefore:

- `busy_timeout` is pinned to **0**. A `_busy_timeout` DSN parameter is rejected.
- `SQLITE_BUSY`/`SQLITE_LOCKED` are mapped to a Go sentinel and retried **in Go**,
  after the request has returned and the worker is free.
- A second connection to the same file-backed database is refused with an error
  naming `db.SetMaxOpenConns(1)`, rather than freezing for 4.5 s.
- `sqlitewasm.OpenDB(dsn)` is provided and sets `SetMaxOpenConns(1)`. More than one
  connection buys zero parallelism — there is one JS thread — while enabling
  `SQLITE_BUSY`.
- Explicit write transactions default to `BEGIN IMMEDIATE` (`_txlock=immediate`) so
  lock upgrades cannot happen mid-transaction.

### 4.11 In-memory databases go through the `memdb` VFS

`file:x?mode=memory&cache=shared` — the DSN every migrating user pastes — returns
`rc=0` and then yields *mutually invisible* databases, because
`SQLITE_OMIT_SHARED_CACHE` makes the `cache=` parameter a silent no-op. Verified:
handle A's `CREATE TABLE t` is invisible to handle B.

The `memdb` VFS is compiled in and does share state across handles (verified). So:

- `cache=shared` / `cache=private` are **rejected** with a message pointing at
  `vfs=memdb`.
- Bare `:memory:` and `mode=memory` are rewritten to
  `file:/<connector-scoped-id>?vfs=memdb`, and the rewrite is documented.

### 4.12 Distribution: one inlined blob worker, zero consumer config

The requirement is that a consumer writes no JavaScript glue and no bundler
configuration. Four options were built and measured against downstream Vite dev,
Vite build, esbuild and rollup:

| approach | outcome |
| --- | --- |
| `new Worker(new URL('./worker.js', import.meta.url))` from a Vite lib build | worker chunk references sibling assets the consumer's build does not copy → **OPFS silently disappears**; `base:'/'` emits an absolute path; rollup/esbuild leave the expression untransformed |
| prebuilt `dist/worker.js` | dies in downstream `vite dev` — the dep optimiser relocates `import.meta.url` into `.vite/deps/` |
| consumer constructs the Worker | violates the requirement |
| **fully inlined blob module worker** | **works everywhere it was tested** |

So the shipped shape is an entry that carries a single fully-inlined worker module — sqlite3 JS, `sqlite3.wasm` as a single-argument
`data:application/wasm;base64` URI, and the OPFS async proxy as a nested blob — and
spawns it with `new Worker(URL.createObjectURL(blob), {type:'module'})`.

Two facts make this viable, both measured in Chromium 141 under COOP/COEP:

- A **`blob:` worker fully inherits cross-origin isolation**:
  `crossOriginIsolated === true`, `SharedArrayBuffer` present, `Atomics.wait` works,
  `createSyncAccessHandle()` works. An end-to-end OPFS round trip was proven.
- A **`data:` worker does not** — opaque origin, no isolation, no
  `SharedArrayBuffer`. Vite's inline-worker runtime falls back to a `data:` worker
  on failure; that fallback must be replaced with a hard error, or persistence
  silently vanishes.

Costs, stated plainly: the bundle is ~1.6 MB (≈581 KB gzipped), kept off the
critical path by the lazy `import()`; and blob workers require CSP
`worker-src blob:`.

`@sqlite.org/sqlite-wasm` moves to `devDependencies` because it is vendored, and its
version is pinned exactly — the build string-patches
`sqlite3-bundler-friendly.mjs:12050-12052` (which hardcodes the OPFS proxy URL with
no escape hatch) and fails loudly if the needle is missing.

### 4.13 The Go worker bootstrap ships too

"Zero JavaScript" is false if the consumer still has to hand-write the Go worker
entry — vendoring `wasm_exec.js`, resolving the Go `.wasm` URL,
`WebAssembly.instantiateStreaming`, `new Go()`, `go.run()`, and error relaying. That
file is the most failure-prone in the stack.

So the package ships a second entry, `sqlite3-wasm-go/go-worker`, exporting
`createGoRuntimeWorker(goWasmUrl, opts)`. `wasm_exec.js` is vendored and pinned,
with a build-time diff against `$(go env GOROOT)/lib/wasm/wasm_exec.js` so a
toolchain bump fails the build instead of shipping a mismatch.

What the consumer still owns, and what the README must say: resolving their own Go
`.wasm` URL, and serving it as `application/wasm`.

**The library import must be evaluated inside the Go worker's global scope.**
Importing it on the main thread installs the global in the wrong realm.

### 4.14 Handshake, not polling

The DB worker's first *synchronous* statement buffers incoming messages:

```js
const pending = []
let sink = (d) => pending.push(d)
self.onmessage = (e) => sink(e.data)
await sqlite3InitModule()      // anything posted during this await is buffered
sink = dispatch; for (const d of pending) dispatch(d)
```

This is not defensive coding. Measured in Chromium 141: two messages posted
immediately after `new Worker()` were **silently dropped** when the worker
registered `onmessage` after a top-level await — no error, no rejection, the Go side
just blocks until its context deadline.

The worker then sends an unsolicited `READY` frame carrying a capability record.
Go must not send `OPEN` before it arrives, and registers `onerror` /
`onmessageerror` so a CSP block or a wasm compile failure becomes a real Go error
instead of a hang. The handshake has its own timeout, separate from the caller's
context.

The old 100 ms poll for the global (`binding/promiser.go:23-31`) is deleted. ESM
guarantees the global is installed before the importing module body runs, so the
global is read **once, synchronously**, and its absence is an immediate error naming
the exact import line — not an indefinite hang.

The global becomes a versioned object rather than a bare function, so a protocol
mismatch is a typed Go error:

```ts
globalThis["sqlite3-wasm-go"] = { protocolVersion: 1, createWorker(): Worker }
```

The key is defined as one exported constant on both sides (`src/global.ts`,
`binding/global.go`) — it is currently spelled three different ways across the repo.

### 4.15 Capability ladder, never a silent substitution

`READY` reports `{protocolVersion, sqliteVersion, crossOriginIsolated, hasSAB,
bigIntEnabled, canInstallProgressHandler, vfs:{opfs, opfs-sahpool, memdb}}`.

- `vfs=opfs` unavailable → **hard error** naming the missing
  `Cross-Origin-Embedder-Policy: require-corp` header and suggesting
  `vfs=opfs-sahpool`.
- `vfs=opfs-sahpool` → an explicitly awaited `installOpfsSAHPoolVfs()` in the OPEN
  path. It is never auto-installed, needs no SAB or cross-origin isolation, and is
  the correct persistence tier for pages that cannot set COOP/COEP.
- No `vfs=` → default transient VFS with a one-time warning that data is not
  persisted.

`wasm.bigIntEnabled` is asserted at worker start; without it `sqlite3_column_int64`,
`bind_int64`, `changes64` and `last_insert_rowid` all throw `fI64Disabled`
(`sqlite3.mjs:9012-9016`) — the entire foundation of the wire format.

### 4.16 Lifecycle

- `Connector` implements `io.Closer` (honoured by `DB.Close`, `driver.go:120-121`):
  send `CLOSE`, await the ack, then `worker.terminate()`.
- On `CLOSE` the worker calls `sqlite3_close_v2` on every handle — which also
  uninstalls the progress handler — and `pauseVfs()` on an installed SAH pool.
- The JS factory keeps a registry of live workers and terminates them from
  `import.meta.hot?.dispose()` and a `pagehide` listener. Without this, `vite dev`
  HMR accumulates workers each holding the OPFS file, and a worker parked in
  `Atomics.wait` keeps a sync access handle open into the next page or test.
- The factory also keeps a `Map<vfs + '\0' + resolvedPath, Worker>` so two
  Connectors for the same OPFS file reuse one worker instead of racing for the
  exclusive sync access handle.
- The blob object URL is created once, cached module-scoped, and revoked only at
  registry teardown.

### 4.17 Sharing the compiled wasm module

`sqlite3.wasm` is 856 KB, and because it is delivered as a `data:` URI inside a blob
worker the browser's wasm code cache cannot be reused across workers. Each DB worker
otherwise pays a full base64 decode plus compile, on top of 16 MiB of committed
linear memory (`min 256` pages).

`WebAssembly.Module` is structured-serialisable to same-agent-cluster workers, so
the factory compiles once, caches module-scoped, `postMessage`s the module into each
DB worker, and the worker passes an `instantiateWasm` hook to `sqlite3InitModule`.
`locateFile` is *not* exposed — it is advertised in the typings but unconditionally
overwritten before the module-overrides snapshot (`sqlite3.mjs:72-74`).

An opt-in `createWorker({ wasmUrl })` escape hatch remains for size-sensitive
consumers.

## 5. Public API

Unchanged, and load-bearing:

- Go import path `github.com/lesomnus/sqlite3-wasm`
- `sql.Register("sqlite3-wasm", …)` via blank import

New:

- `sqlitewasm.OpenDB(dsn) (*sql.DB, error)` — `sql.Open` + `SetMaxOpenConns(1)`
- `sqlitewasm.Time` / `sqlitewasm.NullTime` — `sql.Scanner`s for columns with no
  decltype (`SELECT MAX(created_at)`). This is deliberately an explicit
  per-destination opt-in rather than a global "guess-parse TEXT" flag, which would
  turn a user's name of `'2024-01-02'` into a `time.Time`.
- `sqlitewasm.ErrBusy`, `ErrConstraint`, … and `*sqlitewasm.Error` carrying the
  result code, extended result code, message and `sqlite3_error_offset`.

A non-js build stub registers the same driver name with an `Open` that returns
`errors.New("sqlite3-wasm requires GOOS=js GOARCH=wasm")`, instead of the current
`unknown driver` from an empty registration.

npm package renames to `sqlite3-wasm-go` (`sqlite3-wasm` is taken; the current
package is `"private": true` and has never been publishable).

## 6. Known limitations

| limitation | reason |
| --- | --- |
| WAL is unavailable | neither OPFS VFS implements `xShmMap`; `journal_mode=WAL` on `memdb` returns `memory` |
| One connection per database | no shared cache, no WAL, busy-sleep blocks the worker thread |
| `cache=shared` rejected | `SQLITE_OMIT_SHARED_CACHE` makes it a silent no-op |
| Cancellation needs COOP/COEP + `wasm-unsafe-eval` | `SharedArrayBuffer` and `jsFuncToWasm` |
| Cancellation cannot interrupt VFS I/O | the progress handler does not fire inside the VFS |
| CSP must allow `worker-src blob:` | the worker is a blob URL |
| Browser floor ≈ Chrome 80 / Firefox 114 / Safari 16.4 | nested module workers + OPFS sync access handles |
| ~1.6 MB bundle | sqlite3 is vendored and inlined; lazily imported |

## 7. Evidence index

Claims above were verified against, and cite:

- `node_modules/@sqlite.org/sqlite-wasm/sqlite-wasm/jswasm/sqlite3.mjs` (SQLite 3.50.4)
- `$GOROOT/src/syscall/js/{js.go,func.go}`, `runtime/lock_js.go`, `runtime/proc.go`
- `$GOROOT/src/database/sql/{sql.go,convert.go,ctxutil.go}`, `driver/driver.go`
- `mattn/go-sqlite3` and `modernc.org/sqlite` master sources
- runtime probes in Chromium 141 (blob/nested workers, COI, OPFS, SAB) and Node 24
  (syscall/js cost model, Go/wasm scheduler behaviour)
