/**
 * One sqlite3* handle and everything that hangs off it: prepared-statement
 * caching, binding, and the raw-export handles used by the row loop.
 *
 * The split between `capi.*` and `wasm.exports.*` is deliberate and measured.
 * `capi.*` functions are xWrap-generated wrappers that run per-argument
 * adapters on every call; on a 50k-row 4-column scan the full extract-and-encode
 * loop costs 634 ns/cell through them and 86 ns/cell through the raw exports.
 * So the hot loop uses raw exports, and `capi.*` is kept for the cold path —
 * open, prepare, decltype, errors, binding text and blobs — where the adapters
 * earn their keep.
 *
 * `wasm.exports` is documented public surface (index.d.ts: "available for those
 * who want to make use of it"), not a private hook.
 */

// biome-ignore lint/suspicious/noExplicitAny: the sqlite3 namespace is untyped
type Sqlite3 = any

export type Raw = {
	sqlite3_step(pStmt: number): number
	sqlite3_reset(pStmt: number): number
	sqlite3_clear_bindings(pStmt: number): number
	sqlite3_finalize(pStmt: number): number
	sqlite3_column_count(pStmt: number): number
	sqlite3_column_type(pStmt: number, i: number): number
	sqlite3_column_int64(pStmt: number, i: number): bigint
	sqlite3_column_double(pStmt: number, i: number): number
	sqlite3_column_blob(pStmt: number, i: number): number
	sqlite3_column_bytes(pStmt: number, i: number): number
	sqlite3_bind_int64(pStmt: number, i: number, v: bigint): number
	sqlite3_bind_double(pStmt: number, i: number, v: number): number
	sqlite3_bind_null(pStmt: number, i: number): number
	sqlite3_bind_parameter_count(pStmt: number): number
	sqlite3_changes64(pDb: number): bigint
	sqlite3_last_insert_rowid(pDb: number): bigint
	sqlite3_get_autocommit(pDb: number): number
	sqlite3_stmt_readonly(pStmt: number): number
}

export type Column = {
	name: string
	declType: string | null
}

/** A SQLite error carrying everything the ERROR frame needs. */
export class SqliteError extends Error {
	readonly code: number
	readonly extendedCode: number
	readonly offset: number

	constructor(code: number, extendedCode: number, offset: number, message: string) {
		super(message)
		this.name = 'SqliteError'
		this.code = code
		this.extendedCode = extendedCode
		this.offset = offset
	}
}

export type PreparedStatement = {
	pStmt: number
	/** The SQL this statement was compiled from; also its cache key. */
	sql: string
	/** Byte offset of the unconsumed tail within `sql`. */
	tailOffset: number
	/** Total byte length of `sql`, so callers can test for a tail. */
	sqlLength: number
	/**
	 * The column list as of prepare time. Only the PREPARED reply uses it; the
	 * row loop re-reads columns after the first step, because `SELECT *` and a
	 * transparent recompile can both change them. See describeColumns.
	 */
	columns: Column[]
	paramCount: number
	/** Safe to report as database/sql's NumInput; see PROTOCOL.md §4.5. */
	paramCountExact: boolean
	readOnly: boolean
	/** Whether this statement may go back into the cache when released. */
	cacheable: boolean
}

const STATEMENT_CACHE_LIMIT = 64

/**
 * VDBE operations between progress-handler callbacks. Each firing costs a
 * wasm->JS call plus an Atomics.load, so this trades cancellation latency
 * against throughput; 10k keeps a check well under a millisecond of work.
 */
export const PROGRESS_HANDLER_OPS = 10_000

const utf8 = new TextEncoder()

export class Session {
	readonly raw: Raw

	private readonly capi: Sqlite3['capi']
	private readonly wasm: Sqlite3['wasm']
	/** sql -> statement. Insertion order is the LRU order. */
	private readonly cache = new Map<string, PreparedStatement>()
	private closed = false

	readonly pDb: number
	readonly cancelSlot: number

	private constructor(sqlite3: Sqlite3, pDb: number, cancelSlot: number) {
		this.pDb = pDb
		this.cancelSlot = cancelSlot
		this.capi = sqlite3.capi
		this.wasm = sqlite3.wasm
		this.raw = sqlite3.wasm.exports as Raw
	}

	static async open(
		sqlite3: Sqlite3,
		filename: string,
		vfs: string,
		flags: number,
		cancelSlot: number,
	): Promise<Session> {
		const { capi, wasm } = sqlite3

		// opfs-sahpool is never installed automatically — unlike opfs, which
		// installs during sqlite3InitModule when the preconditions hold. It
		// needs no SharedArrayBuffer and no cross-origin isolation, which makes
		// it the persistence tier for pages that cannot set COOP/COEP.
		if (vfs === 'opfs-sahpool' && !capi.sqlite3_vfs_find('opfs-sahpool')) {
			if (typeof sqlite3.installOpfsSAHPoolVfs !== 'function') {
				throw new Error('sqlite3-wasm: the opfs-sahpool VFS is not available in this build')
			}
			await sqlite3.installOpfsSAHPoolVfs({})
		}

		// Never substitute a different VFS: falling back to the transient one
		// would hand the caller a working database that loses everything on
		// reload.
		if (vfs && !capi.sqlite3_vfs_find(vfs)) {
			throw new Error(
				`sqlite3-wasm: no such vfs: ${vfs}` +
					(vfs === 'opfs'
						? ' (it requires cross-origin isolation: serve with ' +
							'Cross-Origin-Opener-Policy: same-origin and ' +
							'Cross-Origin-Embedder-Policy: require-corp, or use vfs=opfs-sahpool)'
						: ''),
			)
		}

		const openFlags =
			flags || capi.SQLITE_OPEN_READWRITE | capi.SQLITE_OPEN_CREATE | capi.SQLITE_OPEN_URI

		let pDb = 0
		const stack = wasm.pstack.pointer
		try {
			const ppDb = wasm.pstack.allocPtr()
			const rc = capi.sqlite3_open_v2(filename, ppDb, openFlags, vfs || null)
			pDb = wasm.peekPtr(ppDb)
			if (rc) {
				// sqlite3_open_v2 leaves a handle behind even on failure.
				const message = pDb ? capi.sqlite3_errmsg(pDb) : capi.sqlite3_errstr(rc)
				if (pDb) capi.sqlite3_close_v2(pDb)
				throw new SqliteError(rc & 0xff, rc, -1, message)
			}
		} finally {
			wasm.pstack.restore(stack)
		}

		// Extended codes make SQLITE_CONSTRAINT_UNIQUE distinguishable from
		// SQLITE_CONSTRAINT_FOREIGNKEY at the call site.
		capi.sqlite3_extended_result_codes(pDb, 1)

		// Pinned to zero, and _busy_timeout is rejected by the DSN parser.
		// SQLite's busy handler sleeps with Atomics.wait on this very thread,
		// and the COMMIT that would release the lock can only arrive when this
		// thread returns to its event loop.
		capi.sqlite3_busy_timeout(pDb, 0)

		return new Session(sqlite3, pDb, cancelSlot)
	}

	/**
	 * Installs the cancellation progress handler. Exactly one closure per
	 * database, installed once: the FuncPtrAdapter caches by function identity,
	 * so a fresh closure per query would churn the wasm function table.
	 *
	 * Returns false when installation is refused — `jsFuncToWasm` builds a
	 * WebAssembly.Module at runtime, so a CSP without 'wasm-unsafe-eval' makes
	 * cancellation unavailable rather than broken.
	 */
	installCancelHandler(view: Int32Array | null): boolean {
		if (!view) return false
		try {
			this.capi.sqlite3_progress_handler(
				this.pDb,
				PROGRESS_HANDLER_OPS,
				// The slot holds the id of the request to abort, not a flag: a
				// boolean would let a late cancellation kill the next,
				// unrelated statement.
				() => (Atomics.load(view, this.cancelSlot) === this.runningRequestId ? 1 : 0),
				0,
			)
			return true
		} catch {
			return false
		}
	}

	/** The request id currently executing on this handle, or 0. */
	runningRequestId = 0

	close(): void {
		if (this.closed) return
		this.closed = true
		this.clearCache()
		// sqlite3_close_v2 also uninstalls the progress handler.
		this.capi.sqlite3_close_v2(this.pDb)
	}

	inAutocommit(): boolean {
		return this.raw.sqlite3_get_autocommit(this.pDb) !== 0
	}

	changes(): bigint {
		return this.raw.sqlite3_changes64(this.pDb)
	}

	lastInsertRowId(): bigint {
		return this.raw.sqlite3_last_insert_rowid(this.pDb)
	}

	/** Builds a SqliteError from the handle's current error state. */
	error(rc: number): SqliteError {
		return new SqliteError(
			rc & 0xff,
			this.capi.sqlite3_extended_errcode(this.pDb) || rc,
			this.capi.sqlite3_error_offset(this.pDb),
			// The binding copies the string out of wasm memory, so this is
			// safe to hold across later calls.
			this.capi.sqlite3_errmsg(this.pDb),
		)
	}

	check(rc: number): void {
		if (rc !== this.capi.SQLITE_OK) throw this.error(rc)
	}

	// -- statements ---------------------------------------------------------

	/**
	 * Compiles the single statement in `sql`, reusing a cached one when
	 * possible. A cache hit costs no capi calls at all: the description is
	 * cached alongside the pointer.
	 *
	 * Throws when `sql` holds more than one statement — silently executing only
	 * the first is how the pre-rewrite driver lost trailing statements.
	 */
	prepareOne(sql: string): PreparedStatement {
		const hit = this.cache.get(sql)
		if (hit !== undefined) {
			this.cache.delete(sql)
			return hit
		}

		const stmt = this.compile(sql, 0, true)
		if (!stmt) {
			throw new SqliteError(1, 1, -1, 'sqlite3-wasm: no statement in the given SQL')
		}
		if (stmt.tailOffset < stmt.sqlLength) {
			// Only a real statement in the tail is an error; trailing
			// whitespace and comments are fine. Ask SQLite rather than guess.
			const next = this.compile(sql, stmt.tailOffset, false)
			if (next) {
				this.raw.sqlite3_finalize(next.pStmt)
				this.discard(stmt)
				throw new SqliteError(
					1,
					1,
					stmt.tailOffset,
					'sqlite3-wasm: multiple statements are not allowed here; use Exec',
				)
			}
		}
		return stmt
	}

	/**
	 * Runs `fn` over every statement in `sql`, in order. Used by Exec, which
	 * must not drop anything after the first semicolon.
	 *
	 * The SQL is copied into wasm memory once for the whole walk rather than
	 * once per statement.
	 */
	forEachStatement(sql: string, fn: (stmt: PreparedStatement) => void): number {
		const { capi, wasm } = this
		const bytes = utf8.encode(sql)

		let count = 0
		const stack = wasm.pstack.pointer
		const scope = wasm.scopedAllocPush()
		try {
			const ppStmt = wasm.pstack.allocPtr()
			const pzTail = wasm.pstack.allocPtr()
			const pBegin = wasm.scopedAlloc(bytes.length + 1)
			wasm.heap8u().set(bytes, pBegin)
			wasm.poke(pBegin + bytes.length, 0, 'i8')

			let pSql = pBegin
			const pEnd = pBegin + bytes.length

			while (pSql && pSql < pEnd && wasm.peek(pSql, 'i8')) {
				wasm.pokePtr(ppStmt, 0)
				wasm.pokePtr(pzTail, 0)
				const rc = capi.sqlite3_prepare_v3(this.pDb, pSql, pEnd - pSql + 1, 0, ppStmt, pzTail)
				if (rc) throw this.error(rc)

				const pStmt = wasm.peekPtr(ppStmt)
				const next = wasm.peekPtr(pzTail)
				if (next <= pSql) break
				pSql = next
				if (!pStmt) continue // whitespace or a comment

				const stmt = this.describe(pStmt, sql, next - pBegin, bytes.length, false)
				try {
					fn(stmt)
					count++
				} finally {
					this.raw.sqlite3_finalize(pStmt)
				}
			}
		} finally {
			wasm.scopedAllocPop(scope)
			wasm.pstack.restore(stack)
		}
		return count
	}

	/** Compiles one statement starting at byte offset `from`. */
	private compile(sql: string, from: number, persistent: boolean): PreparedStatement | null {
		const { capi, wasm } = this
		const bytes = utf8.encode(sql)
		if (from >= bytes.length) return null

		const flags = persistent ? capi.SQLITE_PREPARE_PERSISTENT : 0
		const stack = wasm.pstack.pointer
		const scope = wasm.scopedAllocPush()
		try {
			const ppStmt = wasm.pstack.allocPtr()
			const pzTail = wasm.pstack.allocPtr()

			// The SQL has to live in wasm memory: the string form of
			// sqlite3_prepare_v3 hard-codes a null pzTail, so the tail offset
			// is unavailable without a pointer. pstack has a 4 KiB quota, so
			// the text goes on the scoped heap.
			const pBegin = wasm.scopedAlloc(bytes.length + 1)
			wasm.heap8u().set(bytes, pBegin)
			wasm.poke(pBegin + bytes.length, 0, 'i8')

			let pSql = pBegin + from
			const pEnd = pBegin + bytes.length

			while (pSql && pSql < pEnd && wasm.peek(pSql, 'i8')) {
				wasm.pokePtr(ppStmt, 0)
				wasm.pokePtr(pzTail, 0)
				const rc = capi.sqlite3_prepare_v3(this.pDb, pSql, pEnd - pSql + 1, flags, ppStmt, pzTail)
				if (rc) throw this.error(rc)

				const pStmt = wasm.peekPtr(ppStmt)
				const next = wasm.peekPtr(pzTail)
				if (next <= pSql) return null
				pSql = next
				if (!pStmt) continue // whitespace or a comment

				return this.describe(pStmt, sql, next - pBegin, bytes.length, persistent)
			}
			return null
		} finally {
			wasm.scopedAllocPop(scope)
			wasm.pstack.restore(stack)
		}
	}

	/**
	 * Reads a statement's current column list.
	 *
	 * This is deliberately *not* cached alongside the statement. `SELECT *`
	 * expands at compile time, and sqlite3_prepare_v3 transparently recompiles
	 * a statement when the schema has changed — so a cached description goes
	 * stale the moment someone runs ALTER TABLE. The row loop calls this after
	 * the first sqlite3_step, when the recompile has already happened, so the
	 * columns on the wire always describe the rows on the wire.
	 *
	 * Everything else about a statement (parameter count and names, readonly)
	 * is syntactic and does stay cached.
	 */
	describeColumns(pStmt: number): Column[] {
		const { capi, raw } = this
		const n = raw.sqlite3_column_count(pStmt)
		const columns: Column[] = new Array(n)
		for (let i = 0; i < n; i++) {
			columns[i] = {
				name: capi.sqlite3_column_name(pStmt, i),
				// Null for expression columns; it survives views, joins, CTEs,
				// subqueries, aliases and RETURNING.
				declType: capi.sqlite3_column_decltype(pStmt, i) ?? null,
			}
		}
		return columns
	}

	private describe(
		pStmt: number,
		sql: string,
		tailOffset: number,
		sqlLength: number,
		persistent: boolean,
	): PreparedStatement {
		const { capi, raw } = this
		const columns = this.describeColumns(pStmt)

		const paramCount = raw.sqlite3_bind_parameter_count(pStmt)
		const wholeStatement = tailOffset >= sqlLength

		// sqlite3_bind_parameter_count returns the largest index used, so
		// `SELECT ?5` reports 5 for one real parameter. Reporting that as
		// NumInput makes database/sql reject the call before the driver is ever
		// reached, so the count is only trustworthy when every slot is an
		// anonymous '?' and nothing follows the statement.
		let paramCountExact = wholeStatement
		if (paramCountExact) {
			for (let i = 1; i <= paramCount; i++) {
				if (capi.sqlite3_bind_parameter_name(pStmt, i) != null) {
					paramCountExact = false
					break
				}
			}
		}

		return {
			pStmt,
			sql,
			tailOffset,
			sqlLength,
			columns,
			paramCount,
			paramCountExact,
			readOnly: raw.sqlite3_stmt_readonly(pStmt) !== 0,
			// A statement compiled from SQL with a tail must never be cached:
			// the cache key is the whole string, but the statement is only its
			// first fragment.
			cacheable: persistent && wholeStatement,
		}
	}

	/** Returns a statement to the cache, or finalizes it. */
	release(stmt: PreparedStatement): void {
		const { raw } = this
		if (!stmt.cacheable || this.closed) {
			raw.sqlite3_finalize(stmt.pStmt)
			return
		}
		// Measured at 390 ns, against 4589 ns to prepare and finalize again.
		raw.sqlite3_reset(stmt.pStmt)
		raw.sqlite3_clear_bindings(stmt.pStmt)

		const existing = this.cache.get(stmt.sql)
		if (existing !== undefined) {
			// The same SQL was compiled twice because both copies were live at
			// once; keep the one already cached.
			raw.sqlite3_finalize(stmt.pStmt)
			return
		}
		this.cache.set(stmt.sql, stmt)

		while (this.cache.size > STATEMENT_CACHE_LIMIT) {
			const oldest = this.cache.keys().next()
			if (oldest.done) break
			const victim = this.cache.get(oldest.value)
			this.cache.delete(oldest.value)
			if (victim) raw.sqlite3_finalize(victim.pStmt)
		}
	}

	/** Finalizes a statement outright, bypassing the cache. */
	discard(stmt: PreparedStatement): void {
		this.cache.delete(stmt.sql)
		this.raw.sqlite3_finalize(stmt.pStmt)
	}

	clearCache(): void {
		for (const stmt of this.cache.values()) this.raw.sqlite3_finalize(stmt.pStmt)
		this.cache.clear()
	}

	// -- binding ------------------------------------------------------------

	/** Resolves a named parameter, trying each sigil SQLite accepts. */
	paramIndex(pStmt: number, name: string): number {
		for (const sigil of [':', '@', '$']) {
			const i = this.capi.sqlite3_bind_parameter_index(pStmt, sigil + name)
			if (i) return i
		}
		return 0
	}

	/**
	 * Binds raw bytes as TEXT or BLOB. The wire already carries UTF-8, so
	 * nothing is transcoded and an embedded NUL does not truncate.
	 */
	bindBytes(pStmt: number, i: number, bytes: Uint8Array, asBlob: boolean): number {
		const { capi, wasm } = this
		// alloc can grow wasm memory and detach any existing heap view, so the
		// view is taken *after* the allocation.
		const p = wasm.alloc(bytes.length || 1)
		if (bytes.length) wasm.heap8u().set(bytes, p)
		const bind = asBlob ? capi.sqlite3_bind_blob : capi.sqlite3_bind_text
		return bind(pStmt, i, p, bytes.length, capi.SQLITE_WASM_DEALLOC)
	}

	/** A fresh view of the wasm heap; never hold one across a wasm call. */
	heap(): Uint8Array {
		return this.wasm.heap8u()
	}

	get constants(): Sqlite3['capi'] {
		return this.capi
	}
}
