# Implementation plan

Living document. Updated at the end of every phase; see [§ Changelog](#changelog).

Design rationale: [DESIGN.md](./DESIGN.md) · Wire format: [PROTOCOL.md](./PROTOCOL.md)

---

## Status

| # | Phase | Status |
| --- | --- | --- |
| 0 | Design documents | ✅ done |
| 1 | Wire codec + golden vectors | ⬜ not started |
| 2 | DSN parser + type mapping | ⬜ not started |
| 3 | DB worker | ⬜ not started |
| 4 | Packaging | ⬜ not started |
| 5 | Go transport | ⬜ not started |
| 6 | database/sql driver | ⬜ not started |
| 7 | Tests, examples, README | ⬜ not started |

Everything under `binding/` and `driver/` may be broken freely: the repository has
no tags, so the only load-bearing public contract is the import path
`github.com/lesomnus/sqlite3-wasm` and the driver name `"sqlite3-wasm"`.

---

## Phase 0 — Design documents

Capture the architecture, the wire format, and this plan.

- [x] `docs/DESIGN.md`
- [x] `docs/PROTOCOL.md`
- [x] `docs/PLAN.md`

## Phase 1 — Wire codec + golden vectors

The one piece both languages must agree on byte for byte.

- [ ] `binding/wire/` — Go encoder/decoder. **No build tag**, so `go test ./...`
      runs it on the host without a browser (the trick `driver/utils.go` already uses).
- [ ] `src/wire.ts` — TypeScript encoder/decoder, same opcodes and layouts.
- [ ] `testdata/wire/` — golden corpus: every value tag, `int64` at ±2^63 and ±2^53±1,
      `-0.0`, `±Inf`, `NaN`, empty vs NULL text and blob, TEXT with an embedded NUL
      and non-BMP runes, multi-row batches, every frame type.
- [ ] Go test decoding the corpus and re-encoding to identical bytes.
- [ ] vitest (node, no browser) doing the same from TypeScript.
- [ ] `go test -fuzz` round trip on the Go side.

Exit: both languages produce identical bytes for the whole corpus.

## Phase 2 — DSN parser + type mapping

Also build-tag-free and host-testable — the bulk of the semantics with none of the
browser.

- [ ] `driver/dsn.go` — parse, validate, reject `cache=`/`_busy_timeout`, rewrite
      `:memory:` → `file:/<id>?vfs=memdb`.
- [ ] `driver/decltype.go` — normalisation, `timeClass`/`boolClass`, scan types.
- [ ] `driver/timefmt.go` — layout list, `Z` handling, `ParseInLocation`, write formats.
- [ ] `driver/convert.go` — `(tag, declType) → driver.Value` and the reverse.
- [ ] Host tests including the parse matrix from
      [PROTOCOL.md §9](./PROTOCOL.md#9-type-mapping) and both incumbents' edge cases.

Exit: `GOOS=linux go test ./...` covers DSN, decltype, time and conversion.

## Phase 3 — DB worker

- [ ] `src/worker/index.ts` — synchronous pre-`await` message buffer, init,
      `instantiateWasm` hook, capability probe, `READY`.
- [ ] `src/worker/db.ts` — OPEN (VFS ladder, `extended_result_codes`,
      `busy_timeout=0`, progress handler install + probe), CLOSE.
- [ ] `src/worker/stmt.ts` — `sqlite3_prepare_v3` pointer form with tail iteration
      (copy `oo1.DB.exec`, `sqlite3.mjs:10638-10707`), bind, finalize.
- [ ] `src/worker/rows.ts` — step loop, `sqlite3_column_type` first, raw heap reads,
      geometric batch growth, `MessageChannel` yield between flushes, credit window,
      abort.
- [ ] `src/worker/exec.ts` — multi-statement tail loop, per-statement arg
      consumption, `changes64` + `last_insert_rowid` captured at exec time.
- [ ] `src/worker/cache.ts` — LRU of `sqlite3_stmt*` per db, `SQLITE_PREPARE_PERSISTENT`,
      reset + clear_bindings on release, flush on `SQLITE_SCHEMA` and DDL.
- [ ] `src/worker/error.ts` — copy `errmsg` synchronously from a fresh `heap8u()`.
- [ ] **Hot loop bound to `sqlite3.wasm.exports.*`, not `capi.*`** — 634 → 86 ns/cell.
      Hoist the handles once at init. `capi.*` for the cold path only.
- [ ] vitest browser tests driving the worker over the raw protocol, no Go involved.
- [ ] Microbenchmark asserting the `capi` → raw-exports win did not regress, and
      pinning the progress-handler instruction interval (start ~10k VDBE ops).

Exit: the worker passes a protocol-level conformance suite in Chromium.

## Phase 4 — Packaging

- [ ] `src/global.ts` — the global key and `protocolVersion` as exported constants.
- [ ] `src/index.ts` — installs `{protocolVersion, createWorker}`; lazy `import()`
      of the inlined worker; worker registry; `pagehide` and `import.meta.hot.dispose`
      teardown; per-path worker reuse map; SSR guard.
- [ ] `src/go-worker.ts` — `createGoRuntimeWorker(goWasmUrl, opts)` with vendored,
      pinned `wasm_exec.js`.
- [ ] `scripts/vite-plugin-inline-sqlite3.ts` — resolve `@sqlite.org/sqlite-wasm` to
      `sqlite3-bundler-friendly.mjs`, inline the wasm as a `data:` URI, string-patch
      the OPFS proxy into a nested blob, `this.error()` if the needle is missing.
- [ ] Reject Vite's `data:` worker fallback — a `data:` worker is not cross-origin
      isolated.
- [ ] `wasm_exec.js` drift check against `$(go env GOROOT)/lib/wasm/wasm_exec.js`.
- [ ] `package.json`: rename to `sqlite3-wasm-go`, drop `private`, add `files`,
      `sideEffects`, `types` first in every exports condition; move
      `@sqlite.org/sqlite-wasm` to devDependencies and pin it exactly.
- [ ] Downstream smoke test: build the package, consume it from a scratch Vite app,
      assert an OPFS round trip.

Exit: a consumer app writes zero JS glue and gets a working OPFS database.

## Phase 5 — Go transport

- [ ] `binding/global.go` — read the global once, synchronously; typed errors for
      absent / version-mismatched / retired-key cases.
- [ ] `binding/worker.go` — spawn, `onmessage`/`onerror`/`onmessageerror` as
      `js.Func`s with `Release()` on close, handshake with its own timeout.
- [ ] `binding/conn.go` — request correlation, **mutex-guarded queue + cap-1 wake
      channel** hand-off (never a blocking send from a `js.Func`), `defer recover()`
      in every callback, credit granting from the consumer, abort, late-frame discard.
- [ ] `binding/cancel.go` — `Int32Array` over the `SharedArrayBuffer`, generation
      word, watcher goroutine torn down with `defer`.

Exit: a Go program can open, query, and cancel through the worker.

## Phase 6 — database/sql driver

- [ ] `Driver` + `DriverContext`; `OpenConnector` memoised by DSN so
      `Driver.Open` cannot mint a second worker.
- [ ] `Connector` as a pointer type, worker created under `sync.Once` with the error
      memoised; implements `io.Closer`.
- [ ] `Conn`: `Pinger`, `ExecerContext`, `QueryerContext`, `ConnPrepareContext`,
      `ConnBeginTx`, `SessionResetter`, `Validator`, `NamedValueChecker`.
- [ ] `Stmt`: `StmtExecContext`, `StmtQueryContext`, `NumInput` per
      [PROTOCOL.md §4.5](./PROTOCOL.md#45-prepared).
- [ ] `Rows`: `RowsColumnTypeDatabaseTypeName`, `RowsColumnTypeScanType`; bare
      `io.EOF`; non-blocking `Close`; ctx polled in `Next`; **decode one row at a
      time straight into the `dest []driver.Value` slice** — never materialise a
      `[][]driver.Value` (95 % of Go-side cost is interface boxing).
- [ ] `Tx`: `BEGIN IMMEDIATE` by default, reject non-default isolation, map `ReadOnly`.
- [ ] Error model: `*sqlitewasm.Error` with rc / extended rc / message / offset,
      `Is` support, `ErrBusy` / `ErrConstraint`; Go-side `SQLITE_BUSY` retry;
      never `driver.ErrBadConn` from an operation that may have executed.
- [ ] `ResetSession`: reconcile zombie statements, zero the cancel word, `ROLLBACK`
      when `sqlite3_get_autocommit` is 0, `ErrBadConn` when the worker is dead.
- [ ] `sqlitewasm.OpenDB`, `sqlitewasm.Time` / `NullTime`, non-js build stub.
- [ ] Delete `substituteParams`, `escapeValue`, `convertToDriverValue`, `isInteger`,
      `CloseResult`; wire up `notWhitespace` for prepare-tail checks.

Exit: the existing `examples/driver` passes, plus transactions and cancellation.

## Phase 7 — Tests, examples, README

- [ ] Replace `internal/assert` with a runner emitting `{name, ok, msg, file:line}`
      so a failed assertion is diagnosable (today it panics with `[]`).
- [ ] One `conformance` Go binary with subtests selected by `postMessage`, instead of
      one wasm blob per scenario.
- [ ] `src/examples.test.ts`: `onerror`, `onmessageerror`, a per-test timeout,
      `terminate()` in a `finally`, unique OPFS filenames, `afterEach` cleanup.
- [ ] Wire `scripts/build-examples.mts` into an npm `pretest` so stale wasm cannot run.
- [ ] Coverage: storage-class fidelity (`int64` past 2^53, `-0.0`, `1e300`, NaN/±Inf,
      empty blob, TEXT with NUL and non-BMP runes); decltype time round trip;
      `sql.Named`; `LastInsertId`/`RowsAffected` for INSERT/UPDATE/DELETE;
      multi-statement `Exec`; transactions and rollback; ctx cancellation;
      `ColumnTypes()`; early `rows.Close()` mid-stream; two connections on one OPFS
      file producing a fast explicit error; pooled memory DSN sharing one database.
- [ ] OPFS tier gated on `crossOriginIsolated`; add `firefox` and `webkit` vitest
      instances with at least a smoke tier.
- [ ] README rewrite: new global contract, `go-worker` entry, DSN table,
      `SetMaxOpenConns(1)`, COOP/COEP **and** CSP (`worker-src blob:`,
      `script-src 'wasm-unsafe-eval'`), WAL unavailability, browser floor,
      Next.js recipe alongside Vite. Keep the ncruces attribution and add mattn.

Exit: `npm test` and `go test ./...` both green; README describes what actually ships.

---

## Open questions

- Does `opfs-sahpool` have the same one-handle-per-path constraint as `opfs`? It is
  the recommended no-COI persistence tier, so this must be measured before the README
  recommends it for multi-connection use.
- Progress-handler instruction count: start at ~10 000 VDBE ops and measure. Each
  firing costs a wasm→JS call plus an `Atomics.load`.
- Whether to offer WAL at all via `PRAGMA locking_mode=exclusive` (heap wal-index).
  Currently planned as: detect and reject with a clear message.
- Batch-buffer recycling (Go returns a spent buffer in the transfer list of its next
  request). Worth ~10 µs per 256 KiB batch; deferred until phase 3 benchmarks say the
  allocation actually shows up.

---

## Changelog

### Phase 0 — design documents

Investigated the current driver against the real sources and reproduced its three
core defects in Chromium 141 before writing anything:

- `SELECT 1.0` → `int64(1)` (`REAL` collapsed by the `isInteger` heuristic)
- `SELECT 9007199254740993` → `panic: bad type flag` at `syscall/js/js.go:288`,
  via `jz/unmarshal.go:34` ← `binding/promiser.go:107`

Also repaired the local test harness: the Playwright browser download extracts only
`v8_context_snapshot.bin`, so `chromium_headless_shell-1194` was installed by hand.
`npx vitest run` and the Go-in-worker example suite both pass on the pre-rewrite code,
which gives the rewrite a working baseline to regress against.

The design was then reviewed against measurements rather than intuition, which moved
three things:

- **The encoding is not the bottleneck.** Per cell, sqlite3 extraction + encoding
  costs 634 ns through `capi.*` wrappers versus 86 ns through `wasm.exports.*`;
  transport is ~1 ns and the Go-side parse 0.58 ns. Row-major vs columnar and
  fixed64 vs varint were both benchmarked and are noise. The plan now targets the
  wrapper overhead and the round-trip count instead.
- **Round trips dominate small queries.** A worker↔worker round trip is a flat
  ~32 µs against ~1 µs for a cached point query, so `QUERY`/`EXEC` are fused into one
  message and the worker keeps a statement cache (7 155 ns uncached → 972 ns cached).
- **Eager push needed bounding in three ways**: geometric batch growth so the first
  row does not wait for 1024 steps, a `MessageChannel` macrotask yield so one scan
  cannot block every other pooled connection, and a 4-batch credit window so a slow
  consumer cannot OOM the tab.

Blockers found and folded in before any code was written: the DB worker must install
a message buffer *before* its first `await` (messages posted right after
`new Worker()` are otherwise silently dropped — reproduced); the cancellation word
must hold a request generation rather than a boolean; pooled `:memory:` is silently
broken by `SQLITE_OMIT_SHARED_CACHE` and must be rewritten onto the `memdb` VFS; and
a non-zero `busy_timeout` self-deadlocks the worker because SQLite's busy sleep is
`Atomics.wait` on that same thread.
