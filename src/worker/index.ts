// Must be first: it installs self.onmessage before any dependency can await.
// See early.ts for why that ordering is load-bearing.
import { setSink } from './early'

import sqlite3InitModule from '@sqlite.org/sqlite-wasm'

import { Cap, Flag, FrameReader, FrameWriter, Op, PROTOCOL_VERSION, Tag } from '../wire'
import { type Column, type PreparedStatement, Session, SqliteError } from './session'

/**
 * Row batches grow geometrically so the first row reaches the caller after one
 * sqlite3_step instead of after a thousand. A one-way push costs 4.4 us at
 * 256 B and 29.6 us at 256 KiB, so a small first batch is nearly free.
 */
const BATCH_ROW_TARGETS = [1, 8, 64, 512, 1024]

/**
 * Hard ceiling. Round-trip time is flat at 31.8-37.4 us from 4 KiB to 256 KiB
 * but jumps to 127.6 us at 1 MiB -- that is allocating and zeroing the payload,
 * not transferring it.
 */
const MAX_BATCH_BYTES = 256 * 1024

/** Unacknowledged batches allowed in flight per request. */
const CREDIT_WINDOW = 4

/** Cancellation slots in the shared word array; one per open database. */
const CANCEL_SLOTS = 256

// biome-ignore lint/suspicious/noExplicitAny: the sqlite3 namespace is untyped
type Sqlite3 = any

/**
 * The dedicated-worker global. The project is type-checked against the DOM lib,
 * where `self.postMessage` has the window signature and rejects a transfer
 * list, so the worker scope is reached through one narrow alias.
 */
const ctx = self as unknown as {
	postMessage(message: unknown, transfer?: Transferable[]): void
	crossOriginIsolated: boolean
}

type Stream = {
	id: number
	credit: number
	aborted: boolean
	wake: (() => void) | null
}

/**
 * Aborts that arrived before their request started.
 *
 * Bounded so a client that aborts ids which never arrive cannot grow it
 * without limit; the oldest entry is dropped, which at worst costs one
 * wasted scan rather than a wedged worker.
 */
const abortedBeforeStart = new Set<number>()
const MAX_EARLY_ABORTS = 1024

function rememberAbort(id: number): void {
	abortedBeforeStart.add(id)
	while (abortedBeforeStart.size > MAX_EARLY_ABORTS) {
		const oldest = abortedBeforeStart.values().next()
		if (oldest.done) break
		abortedBeforeStart.delete(oldest.value)
	}
}

/** Consumes an early abort for this id, if one is pending. */
function takeEarlyAbort(id: number): boolean {
	return abortedBeforeStart.delete(id)
}

const sessions = new Map<number, Session>()
const statements = new Map<number, { session: Session; stmt: PreparedStatement; dbId: number }>()
const streams = new Map<number, Stream>()

let sqlite3: Sqlite3
let cancelBuffer: SharedArrayBuffer | null = null
let cancelView: Int32Array | null = null
const freeSlots: number[] = []
let nextDbId = 1
let nextStmtId = 1

// -- posting ----------------------------------------------------------------

function post(frame: Uint8Array): void {
	// Post the view, transfer the buffer: js.CopyBytesToGo accepts only a
	// Uint8Array and it honours byteOffset, so Go copies straight out with no
	// wrapper allocation.
	ctx.postMessage(frame, [frame.buffer as ArrayBuffer])
}

function postOK(id: number): void {
	post(new FrameWriter(Op.OK, Flag.EOF, id).frame())
}

function postError(id: number, err: unknown): void {
	const w = new FrameWriter(Op.ERROR, Flag.EOF, id, 128)
	if (err instanceof SqliteError) {
		w.i32(err.code)
		w.i32(err.extendedCode)
		w.i32(err.offset)
		w.str(err.message)
	} else {
		w.i32(1)
		w.i32(1)
		w.i32(-1)
		w.str(err instanceof Error ? err.message : String(err))
	}
	post(w.frame())
}

/**
 * Yields to the event loop between batches so a long scan cannot block every
 * other connection on this worker.
 *
 * It has to be a macrotask: `await null` does not drain the postMessage task
 * queue, and setTimeout(0) is clamped to 4 ms once nested more than five deep.
 */
const yieldChannel = new MessageChannel()
let yieldWaiters: (() => void)[] = []
yieldChannel.port1.onmessage = () => {
	const waiters = yieldWaiters
	yieldWaiters = []
	for (const w of waiters) w()
}

function macrotask(): Promise<void> {
	return new Promise((resolve) => {
		yieldWaiters.push(resolve)
		yieldChannel.port2.postMessage(0)
	})
}

// -- dispatch ---------------------------------------------------------------

/**
 * One serial chain per database, plus a global one keyed 0 for requests that
 * do not belong to a database yet.
 *
 * Serialising *within* a database is required: one sqlite3 handle, one
 * statement cache. Serialising across databases is not, and doing it means a
 * streaming query holds the chain for as long as its consumer takes to drain
 * it — so a slow reader on one connection stalls every other connection on the
 * worker, including CLOSE.
 */
const chains = new Map<number, Promise<void>>()

function chainKey(op: number, data: Uint8Array): number {
	switch (op) {
		case Op.CLOSE:
		case Op.PREPARE:
		case Op.QUERY:
		case Op.EXEC:
			// dbId is the first payload field.
			return FrameReader.header(data).reader.u32()
		case Op.FINALIZE:
		case Op.QUERY_STMT:
		case Op.EXEC_STMT: {
			const stmtId = FrameReader.header(data).reader.u32()
			return statements.get(stmtId)?.dbId ?? 0
		}
		default:
			// OPEN has no database yet; SHUTDOWN concerns all of them.
			return 0
	}
}

function dispatch(data: unknown): void {
	if (!(data instanceof Uint8Array)) {
		console.error('sqlite3-wasm worker: expected a Uint8Array frame, got', typeof data)
		return
	}

	let header: { op: number; flags: number; id: number }
	let reader: FrameReader
	try {
		;({ header, reader } = FrameReader.header(data))
	} catch (e) {
		// Nothing to correlate the failure with.
		console.error('sqlite3-wasm worker: undecodable frame', e)
		return
	}

	// Flow control must not queue behind the request it is controlling.
	if (header.op === Op.CREDIT) {
		const s = streams.get(header.id)
		if (s) {
			s.credit += reader.u32()
			s.wake?.()
		}
		return
	}
	if (header.op === Op.ABORT) {
		const s = streams.get(header.id)
		if (s) {
			s.aborted = true
			s.wake?.()
		} else {
			// The request has not started yet — it is still queued behind
			// another one on `chain`. Dropping the abort here would be fatal:
			// the query would later stream into a route Go has already torn
			// down, spend its credit window and park forever on a credit that
			// can never arrive, taking the whole worker with it.
			rememberAbort(header.id)
		}
		return
	}

	let key: number
	try {
		key = chainKey(header.op, data)
	} catch {
		key = 0
	}

	const next = (chains.get(key) ?? Promise.resolve())
		.then(() => handle(header.op, header.id, reader))
		.catch((e) => {
			postError(header.id, e)
		})
		.finally(() => {
			// Nothing will look this id up again.
			abortedBeforeStart.delete(header.id)
			if (chains.get(key) === next) chains.delete(key)
		})
	chains.set(key, next)
}

async function handle(op: number, id: number, r: FrameReader): Promise<void> {
	switch (op) {
		case Op.OPEN:
			return openDatabase(id, r)
		case Op.CLOSE:
			return closeDatabase(id, r)
		case Op.PREPARE:
			return prepareStatement(id, r)
		case Op.FINALIZE:
			return finalizeStatement(id, r)
		case Op.QUERY: {
			const session = sessionOf(r.u32())
			const sql = r.str()
			return runQuery(id, session, session.prepareOne(sql), r, true)
		}
		case Op.QUERY_STMT: {
			const entry = statementOf(r.u32())
			return runQuery(id, entry.session, entry.stmt, r, false)
		}
		case Op.EXEC: {
			const session = sessionOf(r.u32())
			return runExec(id, session, r.str(), r)
		}
		case Op.EXEC_STMT: {
			const entry = statementOf(r.u32())
			return runExecStatement(id, entry.session, entry.stmt, r)
		}
		case Op.SHUTDOWN:
			return shutdown(id)
		default:
			throw new Error(`sqlite3-wasm worker: unknown opcode 0x${op.toString(16)}`)
	}
}

function sessionOf(dbId: number): Session {
	const s = sessions.get(dbId)
	if (!s) throw new Error(`sqlite3-wasm worker: unknown database ${dbId}`)
	return s
}

function statementOf(stmtId: number): { session: Session; stmt: PreparedStatement; dbId: number } {
	const e = statements.get(stmtId)
	if (!e) throw new Error(`sqlite3-wasm worker: unknown statement ${stmtId}`)
	return e
}

// -- database lifecycle -----------------------------------------------------

async function openDatabase(id: number, r: FrameReader): Promise<void> {
	const filename = r.str()
	const vfs = r.str()
	const flags = r.i32()

	const slot = freeSlots.pop()
	if (slot === undefined) {
		throw new Error(`sqlite3-wasm worker: more than ${CANCEL_SLOTS} open databases`)
	}

	let session: Session
	try {
		session = await Session.open(sqlite3, filename, vfs, flags, slot)
	} catch (e) {
		freeSlots.push(slot)
		throw e
	}

	if (cancelView) Atomics.store(cancelView, slot, 0)
	const cancellable = session.installCancelHandler(cancelView)

	const dbId = nextDbId++
	sessions.set(dbId, session)

	const w = new FrameWriter(Op.OPENED, Flag.EOF, id, 16)
	w.u32(dbId)
	w.u32(slot)
	w.bool(cancellable)
	post(w.frame())
}

function closeDatabase(id: number, r: FrameReader): void {
	const dbId = r.u32()
	const session = sessions.get(dbId)
	if (session) {
		for (const [stmtId, entry] of statements) {
			if (entry.session === session) {
				session.discard(entry.stmt)
				statements.delete(stmtId)
			}
		}
		session.close()
		sessions.delete(dbId)
		freeSlots.push(session.cancelSlot)
	}
	postOK(id)
}

async function shutdown(id: number): Promise<void> {
	for (const [dbId, session] of sessions) {
		session.close()
		sessions.delete(dbId)
	}
	statements.clear()
	postOK(id)
}

// -- statements -------------------------------------------------------------

function prepareStatement(id: number, r: FrameReader): void {
	const dbId = r.u32()
	const session = sessionOf(dbId)
	const stmt = session.prepareOne(r.str())
	const stmtId = nextStmtId++
	statements.set(stmtId, { session, stmt, dbId })

	const w = new FrameWriter(Op.PREPARED, Flag.EOF, id, 128)
	w.u32(stmtId)
	w.u32(stmt.paramCount)
	w.bool(stmt.paramCountExact)
	w.u32(stmt.tailOffset)
	w.bool(stmt.readOnly)
	writeColumns(w, stmt)
	post(w.frame())
}

function finalizeStatement(id: number, r: FrameReader): void {
	const stmtId = r.u32()
	const entry = statements.get(stmtId)
	if (entry) {
		entry.session.release(entry.stmt)
		statements.delete(stmtId)
	}
	postOK(id)
}

function writeColumns(w: FrameWriter, stmt: PreparedStatement): void {
	writeColumnList(w, stmt.columns)
}

function writeColumnList(w: FrameWriter, columns: Column[]): void {
	w.u32(columns.length)
	for (const c of columns) {
		w.str(c.name)
		w.nstr(c.declType)
	}
}

/** Binds every argument in the frame, in wire order. */
function bindArgs(session: Session, stmt: PreparedStatement, r: FrameReader): void {
	const count = r.u32()

	// An unbound parameter is NULL, so a caller who passes too few arguments
	// would silently insert NULLs rather than get an error. database/sql
	// cannot catch this for us: NumInput is -1 whenever the count is not
	// trustworthy, and the Execer/Queryer fast path skips the check entirely.
	// Named parameters are exempt, since one value can fill several slots.
	let positional = 0

	for (let k = 0; k < count; k++) {
		const name = r.nstr()
		const ordinal = r.u32()

		let i = ordinal
		if (name !== null) {
			i = session.paramIndex(stmt.pStmt, name)
			if (!i) {
				throw new SqliteError(1, 1, -1, `sqlite3-wasm: no such parameter: ${name}`)
			}
		} else {
			positional++
		}

		let rc: number
		switch (r.tag()) {
			case Tag.NULL:
				rc = session.raw.sqlite3_bind_null(stmt.pStmt, i)
				break
			case Tag.INT:
				rc = session.raw.sqlite3_bind_int64(stmt.pStmt, i, r.i64())
				break
			case Tag.REAL:
				rc = session.raw.sqlite3_bind_double(stmt.pStmt, i, r.f64())
				break
			case Tag.TEXT:
				rc = session.bindBytes(stmt.pStmt, i, r.bytes(), false)
				break
			default:
				rc = session.bindBytes(stmt.pStmt, i, r.bytes(), true)
				break
		}
		session.check(rc)
	}

	if (positional === count && count < stmt.paramCount) {
		throw new SqliteError(
			1,
			1,
			-1,
			`sqlite3-wasm: statement has ${stmt.paramCount} parameter(s), but ${count} argument(s) were given`,
		)
	}
}

// -- queries ----------------------------------------------------------------

async function runQuery(
	id: number,
	session: Session,
	stmt: PreparedStatement,
	r: FrameReader,
	ownsStatement: boolean,
): Promise<void> {
	const { raw, constants: C } = session

	const stream: Stream = {
		id,
		credit: CREDIT_WINDOW,
		// An abort may have arrived while this request was still queued.
		aborted: takeEarlyAbort(id),
		wake: null,
	}
	streams.set(id, stream)
	session.runningRequestId = id

	let interrupted = false
	try {
		raw.sqlite3_reset(stmt.pStmt)
		raw.sqlite3_clear_bindings(stmt.pStmt)
		bindArgs(session, stmt, r)

		// The first step is taken before anything is described: sqlite3
		// transparently recompiles a statement whose schema changed, so the
		// column list is only trustworthy once execution has started.
		let rc = raw.sqlite3_step(stmt.pStmt)
		const columns = session.describeColumns(stmt.pStmt)
		const nCol = columns.length

		let firstBatch = true
		let stage = 0
		let rows = 0
		let batch = new FrameWriter(Op.ROWS, 0, id, 8192)
		writeColumnList(batch, columns)
		let rowCountAt = batch.reserveU32()

		const flush = async (eof: boolean): Promise<void> => {
			batch.patchU32(rowCountAt, rows)
			batch.setFlags((firstBatch ? Flag.HAS_COLUMNS : 0) | (eof ? Flag.EOF : 0))
			post(batch.frame())
			firstBatch = false
			if (eof) return

			// The terminal frame is exempt from the window; everything else
			// spends a credit and may have to wait for one.
			stream.credit--
			if (stream.credit <= 0 && !stream.aborted) {
				await new Promise<void>((resolve) => {
					stream.wake = () => {
						stream.wake = null
						resolve()
					}
				})
			}
			await macrotask()

			if (stage < BATCH_ROW_TARGETS.length - 1) stage++
			rows = 0
			batch = new FrameWriter(Op.ROWS, 0, id, 8192)
			rowCountAt = batch.reserveU32()
		}

		for (;;) {
			if (stream.aborted) {
				post(new FrameWriter(Op.ABORTED, Flag.EOF, id).frame())
				return
			}

			if (rc === C.SQLITE_ROW) {
				encodeRow(session, stmt.pStmt, nCol, batch)
				rows++
				if (rows >= BATCH_ROW_TARGETS[stage] || batch.length >= MAX_BATCH_BYTES) {
					await flush(false)
				}
				rc = raw.sqlite3_step(stmt.pStmt)
				continue
			}
			if (rc === C.SQLITE_DONE) {
				await flush(true)
				return
			}
			if (rc === C.SQLITE_INTERRUPT) {
				interrupted = true
				post(new FrameWriter(Op.ABORTED, Flag.EOF, id).frame())
				return
			}
			throw session.error(rc)
		}
	} finally {
		streams.delete(id)
		session.runningRequestId = 0
		if (ownsStatement) {
			// An interrupted or abandoned statement carries error state that is
			// not worth reasoning about; drop it rather than cache it.
			if (interrupted || stream.aborted) session.discard(stmt)
			else session.release(stmt)
		} else {
			raw.sqlite3_reset(stmt.pStmt)
			raw.sqlite3_clear_bindings(stmt.pStmt)
		}
	}
}

/**
 * Encodes one row straight off the wasm heap.
 *
 * sqlite3_column_type is consulted first, always: sqlite3_column_blob on a
 * numeric column silently rewrites the value in place to its text rendering
 * while column_type keeps reporting SQLITE_INTEGER. It is also the only thing
 * that separates NULL from '' and x'' -- all three report a null pointer and
 * zero length.
 */
function encodeRow(session: Session, pStmt: number, nCol: number, w: FrameWriter): void {
	const { raw, constants: C } = session
	for (let i = 0; i < nCol; i++) {
		switch (raw.sqlite3_column_type(pStmt, i)) {
			case C.SQLITE_INTEGER:
				w.valueInt(raw.sqlite3_column_int64(pStmt, i))
				break
			case C.SQLITE_FLOAT:
				w.valueReal(raw.sqlite3_column_double(pStmt, i))
				break
			case C.SQLITE_TEXT: {
				const p = raw.sqlite3_column_blob(pStmt, i)
				const n = raw.sqlite3_column_bytes(pStmt, i)
				// heap() is re-fetched here because the calls above may have
				// grown wasm memory and detached any earlier view.
				w.valueTextBytes(n ? session.heap().subarray(p, p + n) : EMPTY)
				break
			}
			case C.SQLITE_BLOB: {
				const p = raw.sqlite3_column_blob(pStmt, i)
				const n = raw.sqlite3_column_bytes(pStmt, i)
				w.valueBlob(n ? session.heap().subarray(p, p + n) : EMPTY)
				break
			}
			default:
				w.valueNull()
				break
		}
	}
}

const EMPTY = new Uint8Array(0)

// -- exec -------------------------------------------------------------------

function runExec(id: number, session: Session, sql: string, r: FrameReader): void {
	// Arguments are consumed by the statements that have parameters, in order,
	// which is what mattn/go-sqlite3 does for multi-statement Exec.
	const args = readArgs(r)
	let at = 0

	session.runningRequestId = id
	try {
		const n = session.forEachStatement(sql, (stmt) => {
			const take = stmt.paramCount
			if (at + take > args.length) {
				throw new SqliteError(
					1,
					1,
					-1,
					`sqlite3-wasm: statement needs ${take} argument(s), only ${args.length - at} left`,
				)
			}
			bindDecoded(session, stmt, args.slice(at, at + take))
			at += take
			stepToCompletion(session, stmt)
		})
		if (n === 0) {
			// A statement-free script (all comments) still has to answer.
			postExecResult(id, 0n, session.lastInsertRowId())
			return
		}
		if (at < args.length) {
			throw new SqliteError(1, 1, -1, `sqlite3-wasm: ${args.length - at} unused argument(s)`)
		}
		postExecResult(id, session.changes(), session.lastInsertRowId())
	} finally {
		session.runningRequestId = 0
	}
}

function runExecStatement(
	id: number,
	session: Session,
	stmt: PreparedStatement,
	r: FrameReader,
): void {
	session.runningRequestId = id
	try {
		session.raw.sqlite3_reset(stmt.pStmt)
		session.raw.sqlite3_clear_bindings(stmt.pStmt)
		bindArgs(session, stmt, r)
		stepToCompletion(session, stmt)
		postExecResult(id, session.changes(), session.lastInsertRowId())
	} finally {
		session.raw.sqlite3_reset(stmt.pStmt)
		session.raw.sqlite3_clear_bindings(stmt.pStmt)
		session.runningRequestId = 0
	}
}

function stepToCompletion(session: Session, stmt: PreparedStatement): void {
	const { raw, constants: C } = session
	for (;;) {
		const rc = raw.sqlite3_step(stmt.pStmt)
		if (rc === C.SQLITE_DONE) return
		// A SELECT run through Exec still has rows; discard them.
		if (rc === C.SQLITE_ROW) continue
		throw session.error(rc)
	}
}

function postExecResult(id: number, changes: bigint, rowid: bigint): void {
	const w = new FrameWriter(Op.EXEC_RESULT, Flag.EOF, id, 16)
	w.i64(changes)
	w.i64(rowid)
	post(w.frame())
}

type DecodedArg = {
	name: string | null
	ordinal: number
	tag: number
	int?: bigint
	real?: number
	bytes?: Uint8Array
}

/** Reads the whole argument block, so Exec can hand slices to each statement. */
function readArgs(r: FrameReader): DecodedArg[] {
	const count = r.u32()
	const out: DecodedArg[] = new Array(count)
	for (let k = 0; k < count; k++) {
		const name = r.nstr()
		const ordinal = r.u32()
		const tag = r.tag()
		const a: DecodedArg = { name, ordinal, tag }
		switch (tag) {
			case Tag.INT:
				a.int = r.i64()
				break
			case Tag.REAL:
				a.real = r.f64()
				break
			case Tag.TEXT:
			case Tag.BLOB:
				// A copy, because the frame outlives this call only by
				// accident and the bytes go into wasm memory later.
				a.bytes = r.bytes().slice()
				break
		}
		out[k] = a
	}
	return out
}

function bindDecoded(session: Session, stmt: PreparedStatement, args: DecodedArg[]): void {
	for (let k = 0; k < args.length; k++) {
		const a = args[k]
		let i = a.name === null ? k + 1 : session.paramIndex(stmt.pStmt, a.name)
		if (!i) throw new SqliteError(1, 1, -1, `sqlite3-wasm: no such parameter: ${a.name}`)

		let rc: number
		switch (a.tag) {
			case Tag.NULL:
				rc = session.raw.sqlite3_bind_null(stmt.pStmt, i)
				break
			case Tag.INT:
				rc = session.raw.sqlite3_bind_int64(stmt.pStmt, i, a.int as bigint)
				break
			case Tag.REAL:
				rc = session.raw.sqlite3_bind_double(stmt.pStmt, i, a.real as number)
				break
			case Tag.TEXT:
				rc = session.bindBytes(stmt.pStmt, i, a.bytes as Uint8Array, false)
				break
			default:
				rc = session.bindBytes(stmt.pStmt, i, a.bytes as Uint8Array, true)
				break
		}
		session.check(rc)
	}
}

// -- startup ----------------------------------------------------------------

function capabilities(s: Sqlite3): number {
	let caps = 0
	if (ctx.crossOriginIsolated) caps |= Cap.CROSS_ORIGIN_ISOLATED
	if (typeof SharedArrayBuffer === 'function') caps |= Cap.SHARED_ARRAY_BUFFER
	if (s.wasm.bigIntEnabled) caps |= Cap.BIGINT
	if (s.capi.sqlite3_vfs_find('opfs')) caps |= Cap.VFS_OPFS
	if (typeof s.installOpfsSAHPoolVfs === 'function') caps |= Cap.VFS_OPFS_SAHPOOL
	if (s.capi.sqlite3_vfs_find('memdb')) caps |= Cap.VFS_MEMDB
	return caps
}

/**
 * Probes whether a progress handler can be installed at all. jsFuncToWasm
 * builds a WebAssembly.Module at runtime, so a CSP without 'wasm-unsafe-eval'
 * refuses — and the driver needs to know that up front rather than discovering
 * it mid-query.
 */
function probeProgressHandler(s: Sqlite3): boolean {
	const stack = s.wasm.pstack.pointer
	let pDb = 0
	try {
		const ppDb = s.wasm.pstack.allocPtr()
		if (s.capi.sqlite3_open_v2(':memory:', ppDb, s.capi.SQLITE_OPEN_READWRITE, null)) return false
		pDb = s.wasm.peekPtr(ppDb)
		s.capi.sqlite3_progress_handler(pDb, 1_000_000, () => 0, 0)
		return true
	} catch {
		return false
	} finally {
		if (pDb) s.capi.sqlite3_close_v2(pDb)
		s.wasm.pstack.restore(stack)
	}
}

async function main(): Promise<void> {
	sqlite3 = await sqlite3InitModule({ print() {}, printErr() {} })

	let caps = capabilities(sqlite3)
	if (probeProgressHandler(sqlite3)) caps |= Cap.PROGRESS_HANDLER

	// Every 64-bit accessor the wire format is built on throws without it.
	if (!(caps & Cap.BIGINT)) {
		throw new Error(
			'sqlite3-wasm: this sqlite3 build has BigInt support disabled, ' +
				'so 64-bit integers cannot be read or written',
		)
	}

	if (caps & Cap.SHARED_ARRAY_BUFFER) {
		cancelBuffer = new SharedArrayBuffer(CANCEL_SLOTS * 4)
		cancelView = new Int32Array(cancelBuffer)
	}
	for (let i = CANCEL_SLOTS - 1; i >= 0; i--) freeSlots.push(i)

	const w = new FrameWriter(Op.READY, Flag.EOF, 0, 256)
	w.u16(PROTOCOL_VERSION)
	w.str(sqlite3.version.libVersion)
	w.u32(caps)
	const vfsList: string[] = sqlite3.capi.sqlite3_js_vfs_list()
	w.u32(vfsList.length)
	for (const name of vfsList) w.str(name)

	// The one message that is not a bare Uint8Array: a SharedArrayBuffer can
	// neither be embedded in a byte buffer nor placed in a transfer list.
	const frame = w.frame()
	ctx.postMessage({ p: frame, cancel: cancelBuffer }, [frame.buffer as ArrayBuffer])

	setSink(dispatch)
}

main().catch((e) => {
	// The handshake never completes; surface it as an error frame with id 0 so
	// the Go side fails with a real message instead of a timeout.
	postError(0, e)
})
