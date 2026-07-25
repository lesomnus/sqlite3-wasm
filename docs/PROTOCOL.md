# Wire protocol v1

Normative specification of the messages exchanged between the Go worker and the DB
worker. Rationale lives in [DESIGN.md](./DESIGN.md).

Both sides must agree on this document exactly. The Go and TypeScript codecs are
checked against a shared golden-vector corpus in `testdata/wire/` so they cannot
drift silently.

---

## 1. Transport

Every message except `READY` is a **bare `Uint8Array`**, posted with its underlying
buffer in the transfer list:

```js
self.postMessage(view, [view.buffer])
```

Posting the *view* rather than the `ArrayBuffer` is required: `js.CopyBytesToGo`
accepts only `Uint8Array` and `Uint8ClampedArray` (`wasm_exec.js:449`), so `evt.data`
must already be a `Uint8Array`.

`READY` is the single exception — it is a structured-clone object, because a
`SharedArrayBuffer` cannot be carried inside a byte buffer nor placed in a transfer
list:

```js
{ p: Uint8Array /* the READY frame */, cancel: SharedArrayBuffer | null }
```

All multi-byte integers are **little-endian**. There is no alignment requirement;
fields are packed.

## 2. Frame header

Every frame begins with 8 bytes.

| offset | size | field | notes |
| --- | --- | --- | --- |
| 0 | 1 | `version` | `1` |
| 1 | 1 | `op` | see §3 |
| 2 | 2 | `flags` | see §3.3 |
| 4 | 4 | `id` | request id; `0` for unsolicited frames |

Request ids are assigned by Go, start at 1, and must never be 0 (0 means "no
request" in the cancellation word, §6).

## 3. Opcodes

### 3.1 Requests (Go → DB worker)

| op | name | payload |
| --- | --- | --- |
| `0x01` | `OPEN` | `string filename`, `string vfs`, `i32 flags` |
| `0x02` | `CLOSE` | `u32 dbId` |
| `0x03` | `PREPARE` | `u32 dbId`, `string sql` |
| `0x04` | `FINALIZE` | `u32 stmtId` |
| `0x05` | `QUERY` | `u32 dbId`, `string sql`, *args* |
| `0x06` | `QUERY_STMT` | `u32 stmtId`, *args* |
| `0x07` | `EXEC` | `u32 dbId`, `string sql`, *args* |
| `0x08` | `EXEC_STMT` | `u32 stmtId`, *args* |
| `0x09` | `CREDIT` | `u32 batches` — `id` is the streaming request's id |
| `0x0A` | `ABORT` | *(empty)* — `id` is the streaming request's id |
| `0x0B` | `SHUTDOWN` | *(empty)* |

### 3.2 Responses (DB worker → Go)

| op | name | payload |
| --- | --- | --- |
| `0x80` | `READY` | §5; `id = 0` |
| `0x81` | `OK` | *(empty)* — ack for `CLOSE`, `FINALIZE`, `SHUTDOWN` |
| `0x82` | `ERROR` | `i32 code`, `i32 extendedCode`, `i32 offset`, `string message` |
| `0x83` | `OPENED` | `u32 dbId`, `u32 cancelSlot`, `u8 cancelSupported` |
| `0x84` | `PREPARED` | §4.5 |
| `0x85` | `ROWS` | §4.6 |
| `0x86` | `EXEC_RESULT` | `i64 changes`, `i64 lastInsertRowId` |
| `0x87` | `ABORTED` | *(empty)* — terminal frame for an aborted stream |

`ERROR` and `ABORTED` are terminal for their `id`. `ROWS` frames continue until one
carries `EOF`.

`offset` in `ERROR` is `sqlite3_error_offset`, or `-1` when not applicable.

### 3.3 Flags

| bit | name | meaning |
| --- | --- | --- |
| 0 | `EOF` | last frame for this `id` |
| 1 | `HAS_COLUMNS` | this `ROWS` frame is prefixed with a *columns* block |

All other bits are reserved and must be zero.

## 4. Payload encodings

### 4.1 `string`

```
u32 byteLen
u8  bytes[byteLen]        UTF-8, no NUL terminator
```

`byteLen == 0xFFFFFFFF` means **absent** (SQL `NULL`), distinct from an empty
string. Only fields documented as nullable may use it.

### 4.2 `value`

```
u8 tag
  0x00 NULL   no payload
  0x01 INT    i64                (8 bytes)
  0x02 REAL   f64                (8 bytes)
  0x03 TEXT   u32 len + bytes    UTF-8, may contain NUL
  0x04 BLOB   u32 len + bytes
```

`INT` is written on the JS side with `DataView.setBigInt64` and read on the Go side
with `binary.LittleEndian.Uint64`. A `BigInt` must never become a `js.Value`.

`TEXT` and `BLOB` bytes are copied straight out of the wasm heap; the encoder must
call `sqlite3_column_type` **before** `sqlite3_column_blob`, because
`sqlite3_column_blob` on a numeric column rewrites the value in place to its text
rendering.

`NULL`, `''`, `x''` and `zeroblob(0)` are indistinguishable by pointer and length —
only `sqlite3_column_type` separates them.

### 4.3 *args*

```
u32 count
count × {
  u32   nameLen           0xFFFFFFFF = positional
  u8    name[nameLen]     no sigil; e.g. "foo" for :foo / @foo / $foo
  u32   ordinal           1-based; always present (database/sql always sets it)
  value
}
```

Binding rules in the worker:

- positional arg → bind at `ordinal`
- named arg → `sqlite3_bind_parameter_index` for `":"+name`, then `"@"+name`, then
  `"$"+name`; first hit wins; no hit is an error naming the parameter

### 4.4 *columns*

```
u32 count
count × {
  string name             sqlite3_column_name
  string declType         nullable; sqlite3_column_decltype
}
```

`declType` is sent **verbatim**. Normalisation (uppercase, trim, cut at the first
`(`) happens once on the Go side.

### 4.5 `PREPARED`

```
u32     stmtId
u32     paramCount        sqlite3_bind_parameter_count
u8      paramCountExact   see below
u32     tailOffset        byte offset into the submitted SQL of the unconsumed tail
u8      readOnly          sqlite3_stmt_readonly
columns
```

`paramCountExact` is `1` only when **both** hold:

- `tailOffset` consumed the whole statement (the tail is whitespace/comments only), and
- every slot `1..paramCount` has a `NULL` `sqlite3_bind_parameter_name` — i.e. all
  parameters are anonymous `?`.

Otherwise `0`. This matters because `sqlite3_bind_parameter_count` returns the
*largest index used*, so `SELECT ?5` reports 5 for one real parameter, and
`database/sql` turns a wrong `NumInput` into `sql: expected N arguments, got M`
*before the driver is ever called* (`convert.go:121`, `:202-204`). When
`paramCountExact == 0` the driver reports `NumInput() == -1`.

### 4.6 `ROWS`

```
[columns]                 present iff HAS_COLUMNS
u32 rowCount
rowCount × columnCount × value
```

`HAS_COLUMNS` is set on the first `ROWS` frame of every result set and on no other.
A result set with no rows is a single `ROWS` frame with `HAS_COLUMNS | EOF` and
`rowCount == 0`.

## 5. `READY`

Unsolicited, `id = 0`, sent once when the DB worker has finished initialising.

```
u16     protocolVersion
string  sqliteVersion            e.g. "3.50.4"
u32     capabilities             bitfield, see below
u32     vfsCount
vfsCount × string                sqlite3_js_vfs_list()
```

| bit | capability |
| --- | --- |
| 0 | `crossOriginIsolated` |
| 1 | `SharedArrayBuffer` available |
| 2 | `wasm.bigIntEnabled` |
| 3 | progress handler installable (CSP allows `wasm-unsafe-eval`) |
| 4 | `opfs` VFS present |
| 5 | `opfs-sahpool` installable |
| 6 | `memdb` VFS present |

Bit 2 is mandatory. Without it `sqlite3_column_int64`, `sqlite3_bind_int64`,
`sqlite3_changes64` and `sqlite3_last_insert_rowid` all throw `fI64Disabled`
(`sqlite3.mjs:9012-9016`), which the wire format depends on entirely — the worker
must refuse to become ready.

The accompanying `cancel` `SharedArrayBuffer` is `256 × Int32` (1 KiB), or `null`
when bit 1 is clear.

### 5.1 Startup ordering

The DB worker's **first synchronous statement** must install a buffering listener,
before any `await`:

```js
const pending = []
let sink = (d) => pending.push(d)
self.onmessage = (e) => sink(e.data)
```

After initialisation completes it swaps `sink` for the real dispatcher and drains
`pending`. Messages posted between `new Worker()` and the end of the worker's
top-level `await` are otherwise **silently dropped** — measured in Chromium 141.

Go must not send `OPEN` before `READY` arrives, must register `onerror` and
`onmessageerror`, and must apply its own handshake timeout independent of the
caller's context.

## 6. Cancellation

One `Int32` slot per open database, indexed by the `cancelSlot` returned in
`OPENED`.

- Go aborts request `R` by `Atomics.store(ia, slot, R)`.
- The worker installs exactly **one** `sqlite3_progress_handler` closure per
  database at `OPEN` time (the `FuncPtrAdapter` caches by function identity, so a
  fresh closure per query would churn the wasm function table). The handler returns
  non-zero — aborting `sqlite3_step` with `SQLITE_INTERRUPT` (9) — only when
  `Atomics.load(ia, slot) === currentRequestId`.
- The worker stores `0` into the slot when it dequeues the next request for that
  database.

The word holds a **request id, not a boolean**: a boolean lets a late `ctx.Done()`
abort the *next*, unrelated statement, and a per-worker boolean would abort another
connection's query.

`sqlite3_finalize` also returns `9` after an interrupt. That is not a protocol
error.

The handler does not fire inside the VFS, so a statement stalled in an OPFS
`Atomics.wait` is not interruptible. When bit 1 or bit 3 of `capabilities` is clear
there is no cancellation at all; see [DESIGN.md §4.4](./DESIGN.md#44-cancellation-a-generation-word-in-a-sharedarraybuffer)
for the degraded path.

## 7. Flow control

The worker may have at most `CREDIT_WINDOW` unacknowledged `ROWS` frames in flight
per request. Having spent its credit it returns to its event loop and waits for a
`CREDIT` frame. Go grants credit from the `Rows.Next` consumer — never from the
message callback.

Defaults:

| constant | value |
| --- | --- |
| `MAX_BATCH_ROWS` | 1024 |
| `MAX_BATCH_BYTES` | 256 KiB |
| `CREDIT_WINDOW` | 4 |

`ABORT` stops a stream. Go may send it at any time and must not wait for the reply;
the worker responds with `ABORTED`. Frames that arrive for an aborted or unknown
`id` are **discarded**, not treated as protocol errors.

`Rows.Close` must never block — it is called from `awaitDone` while holding the
connection mutex.

## 8. Type mapping

Applied on the Go side. `declType` is normalised once at prepare:
`strings.ToUpper(strings.TrimSpace(s))`, then cut at the first `(`.

- `timeClass` = `DATE`, `DATETIME`, `TIMESTAMP`
- `boolClass` = `BOOLEAN`

`BOOL` is deliberately *not* in `boolClass`: mattn/go-sqlite3 matches only
`boolean`, so `CREATE TABLE t(x BOOL)` yields `int64` there today, and widening the
class would silently break `Scan(&myInt)` on migration.

### 8.1 Read: (tag, declType) → `driver.Value`

| tag | class | result |
| --- | --- | --- |
| `NULL` | any | `nil` |
| `INT` | time | `time.Time` — `_time_integer_format` if set, else `abs(v) > 1e12` ⇒ milliseconds, else seconds; built in UTC then `.In(loc)` |
| `INT` | bool | `bool` (`v != 0`) |
| `INT` | other | `int64` |
| `REAL` | any | `float64` — **no** time conversion, matching both incumbents |
| `TEXT` | time | `time.Time`, or the **raw `string`** if no layout matches |
| `TEXT` | other | `string` |
| `BLOB` | any | `[]byte`, non-nil; `[]byte{}` for a zero-length blob |

Returning the raw string on a failed parse is deliberate: a silent
`0001-01-01T00:00:00Z` is unrecoverable, whereas a string produces a debuggable
`unsupported Scan` error.

Time layouts, tried in order:

```
2006-01-02 15:04:05.999999999-07:00      ← also the write format
2006-01-02T15:04:05.999999999-07:00
2006-01-02 15:04:05.999999999
2006-01-02T15:04:05.999999999
2006-01-02 15:04
2006-01-02T15:04
2006-01-02
```

A trailing `Z` is stripped first (`strings.CutSuffix`) — Go's `-07:00` layout does
not accept `Z`. Naive strings are parsed with `time.ParseInLocation(f, s, loc)`, so
the wall clock is preserved; `loc` defaults to `time.UTC`.

mattn's list contains two unreachable entries (`…15:04:05` without fractional
seconds) because `.999999999` is optional; they are omitted here.

### 8.2 Write: `driver.Value` → tag

| value | tag | notes |
| --- | --- | --- |
| `nil` | `NULL` | |
| `int64` | `INT` | full 64-bit |
| `bool` | `INT` | `1` / `0` |
| `float64` | `REAL` | |
| `string` | `TEXT` | |
| `[]byte` non-nil | `BLOB` | length 0 ⇒ zero-length blob, not `NULL` |
| `[]byte` nil | `NULL` | mattn parity |
| `time.Time` | `TEXT` | `t.In(loc).Format("2006-01-02 15:04:05.999999999-07:00")` |

`_time_format=utc` writes `t.UTC().Format("2006-01-02 15:04:05.999999999")`, which
is lexicographically sortable across zones; `_time_format=datetime` writes
`2006-01-02 15:04:05`; `_time_integer_format` writes an `INT` instead.

### 8.3 `ColumnTypeScanType`

Derived from `declType` only — never from the value, because `ColumnTypes()` may be
called before the first `Next()`. Never returns `nil`.

| declType | scan type |
| --- | --- |
| contains `INT` | `sql.NullInt64` |
| `CLOB`, `TEXT`, contains `CHAR` | `sql.NullString` |
| `BLOB` | `sql.RawBytes` |
| `REAL`, `FLOAT`, contains `DOUBLE` | `sql.NullFloat64` |
| `timeClass` | `sql.NullTime` |
| `NUMERIC`, contains `DECIMAL` | `sql.NullFloat64` |
| `boolClass` | `sql.NullBool` |
| anything else | `any` |

## 9. DSN

```
file:name.db?vfs=opfs&_loc=UTC&_fk=1
```

Standard SQLite URI parameters are passed through. Driver parameters are
`_`-prefixed.

| parameter | meaning |
| --- | --- |
| `vfs` | `opfs`, `opfs-sahpool`, `memdb`, or unset (transient) |
| `_loc` / `_timezone` | `UTC` (default), `auto` (= `time.Local`), or an IANA name |
| `_time_format` | `offset` (default), `utc`, `datetime` |
| `_time_integer_format` | `unix`, `unix_milli`, `unix_micro`, `unix_nano` |
| `_fk` | `PRAGMA foreign_keys` |
| `_txlock` | `immediate` (default for write transactions), `deferred`, `exclusive` |

Rejected with an explicit error:

| parameter | reason |
| --- | --- |
| `cache=shared` / `cache=private` | `SQLITE_OMIT_SHARED_CACHE` — silently a no-op; use `vfs=memdb` |
| `_busy_timeout` | a non-zero busy timeout self-deadlocks the DB worker |

Rewritten: bare `:memory:` and `mode=memory` become
`file:/<connector-scoped-id>?vfs=memdb`, so a connection pool sees one database
instead of N invisible ones.

Under `GOOS=js`, `time.Local` is UTC unless tzdata is embedded — `_loc=auto` is
documented accordingly.

## 10. Versioning

`protocolVersion` appears in three places and all three must agree:

- `globalThis["sqlite3-wasm-go"].protocolVersion` — checked synchronously at first
  `Connect`, before a worker is spawned
- the `version` byte of every frame header
- the `READY` payload

A mismatch is a typed Go error naming both versions. Any change to this document's
opcodes, payloads or flags increments `protocolVersion`.
