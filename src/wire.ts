/**
 * The binary protocol spoken between the Go worker and the DB worker.
 *
 * This is the TypeScript half of docs/PROTOCOL.md; `binding/wire` is the Go
 * half. Both are checked against `testdata/wire/vectors.json`, which is
 * produced by a third, independent implementation
 * (`scripts/gen-wire-vectors.py`) so that a bug in either half cannot be
 * blessed as the specification.
 *
 * The encoder sits in the row loop, so it avoids per-value allocation: one
 * growable buffer per frame, written through a single DataView.
 */

export const PROTOCOL_VERSION = 1
export const HEADER_SIZE = 8

export const Op = {
	OPEN: 0x01,
	CLOSE: 0x02,
	PREPARE: 0x03,
	FINALIZE: 0x04,
	QUERY: 0x05,
	QUERY_STMT: 0x06,
	EXEC: 0x07,
	EXEC_STMT: 0x08,
	CREDIT: 0x09,
	ABORT: 0x0a,
	SHUTDOWN: 0x0b,

	READY: 0x80,
	OK: 0x81,
	ERROR: 0x82,
	OPENED: 0x83,
	PREPARED: 0x84,
	ROWS: 0x85,
	EXEC_RESULT: 0x86,
	ABORTED: 0x87,
} as const

export type Op = (typeof Op)[keyof typeof Op]

export const OP_NAME: Record<number, string> = Object.fromEntries(
	Object.entries(Op).map(([k, v]) => [v, k]),
)

export const Flag = {
	EOF: 1 << 0,
	HAS_COLUMNS: 1 << 1,
} as const

/** Storage classes, mirroring SQLite's without depending on its constants. */
export const Tag = {
	NULL: 0x00,
	INT: 0x01,
	REAL: 0x02,
	TEXT: 0x03,
	BLOB: 0x04,
} as const

export type Tag = (typeof Tag)[keyof typeof Tag]

/** Marks a nullable string as absent, which is distinct from length 0. */
const ABSENT = 0xffffffff

export const Cap = {
	CROSS_ORIGIN_ISOLATED: 1 << 0,
	SHARED_ARRAY_BUFFER: 1 << 1,
	BIGINT: 1 << 2,
	PROGRESS_HANDLER: 1 << 3,
	VFS_OPFS: 1 << 4,
	VFS_OPFS_SAHPOOL: 1 << 5,
	VFS_MEMDB: 1 << 6,
} as const

export type Header = {
	version: number
	op: number
	flags: number
	id: number
}

export class WireError extends Error {
	constructor(message: string) {
		super(message)
		this.name = 'WireError'
	}
}

const encoder = new TextEncoder()
const decoder = new TextDecoder()

export class FrameWriter {
	private buf: Uint8Array
	private view: DataView
	private n = HEADER_SIZE

	constructor(op: number, flags = 0, id = 0, hint = 64) {
		this.buf = new Uint8Array(HEADER_SIZE + hint)
		this.view = new DataView(this.buf.buffer)
		this.view.setUint8(0, PROTOCOL_VERSION)
		this.view.setUint8(1, op)
		this.view.setUint16(2, flags, true)
		this.view.setUint32(4, id, true)
	}

	/** Current frame size, header included. */
	get length(): number {
		return this.n
	}

	/**
	 * Batch encoders do not know whether a frame is the last one until they
	 * have filled it, so flags stay rewritable.
	 */
	setFlags(flags: number): void {
		this.view.setUint16(2, flags, true)
	}

	/**
	 * The encoded frame, as a view onto the (possibly larger) backing buffer.
	 * Post the view and transfer `view.buffer`: Go's js.CopyBytesToGo accepts
	 * only Uint8Array, and it honours byteOffset, so no copy is needed here.
	 * Transferring detaches the buffer, so the writer must not be reused.
	 */
	frame(): Uint8Array {
		return this.buf.subarray(0, this.n)
	}

	private grow(need: number): void {
		if (this.n + need <= this.buf.length) return
		let cap = this.buf.length * 2
		while (cap < this.n + need) cap *= 2
		const next = new Uint8Array(cap)
		next.set(this.buf.subarray(0, this.n))
		this.buf = next
		this.view = new DataView(next.buffer)
	}

	/**
	 * Reserves a u32 and returns its offset. The row encoder cannot know a
	 * batch's row count until the batch is full, but the count is specified to
	 * come before the rows, so it is patched in at flush time.
	 */
	reserveU32(): number {
		const at = this.n
		this.u32(0)
		return at
	}

	patchU32(at: number, v: number): void {
		this.view.setUint32(at, v, true)
	}

	u8(v: number): void {
		this.grow(1)
		this.view.setUint8(this.n, v)
		this.n += 1
	}

	bool(v: boolean): void {
		this.u8(v ? 1 : 0)
	}

	u16(v: number): void {
		this.grow(2)
		this.view.setUint16(this.n, v, true)
		this.n += 2
	}

	u32(v: number): void {
		this.grow(4)
		this.view.setUint32(this.n, v, true)
		this.n += 4
	}

	i32(v: number): void {
		this.grow(4)
		this.view.setInt32(this.n, v, true)
		this.n += 4
	}

	/**
	 * Accepts a bigint so `sqlite3_column_int64` can be written straight
	 * through: a BigInt must never reach Go, where syscall/js panics on it.
	 */
	i64(v: bigint | number): void {
		this.grow(8)
		this.view.setBigInt64(this.n, typeof v === 'bigint' ? v : BigInt(v), true)
		this.n += 8
	}

	f64(v: number): void {
		this.grow(8)
		this.view.setFloat64(this.n, v, true)
		this.n += 8
	}

	/** Length-prefixed raw bytes. */
	bytes(v: Uint8Array): void {
		this.u32(v.length)
		this.grow(v.length)
		this.buf.set(v, this.n)
		this.n += v.length
	}

	/** Length-prefixed UTF-8 string. */
	str(v: string): void {
		// A UTF-8 encoding is at most 3 bytes per UTF-16 code unit, and
		// surrogate pairs cost 4 bytes for 2 units, so 3x is a safe bound.
		this.grow(4 + v.length * 3)
		const at = this.n
		this.n += 4
		const { written } = encoder.encodeInto(v, this.buf.subarray(this.n))
		this.view.setUint32(at, written, true)
		this.n += written
	}

	/** Nullable string; absent is distinct from empty. */
	nstr(v: string | null | undefined): void {
		if (v == null) {
			this.u32(ABSENT)
			return
		}
		this.str(v)
	}

	valueNull(): void {
		this.u8(Tag.NULL)
	}

	valueInt(v: bigint | number): void {
		this.u8(Tag.INT)
		this.i64(v)
	}

	valueReal(v: number): void {
		this.u8(Tag.REAL)
		this.f64(v)
	}

	valueText(v: string): void {
		this.u8(Tag.TEXT)
		this.str(v)
	}

	/**
	 * TEXT whose bytes came straight off the wasm heap. SQLite does not
	 * guarantee TEXT is valid UTF-8, and reading it as bytes also avoids a
	 * UTF-8 -> UTF-16 -> UTF-8 round trip and truncation at an embedded NUL.
	 */
	valueTextBytes(v: Uint8Array): void {
		this.u8(Tag.TEXT)
		this.bytes(v)
	}

	valueBlob(v: Uint8Array): void {
		this.u8(Tag.BLOB)
		this.bytes(v)
	}
}

export class FrameReader {
	private view: DataView
	private i = 0

	private buf: Uint8Array

	constructor(buf: Uint8Array, start = 0) {
		this.buf = buf
		this.view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength)
		this.i = start
	}

	/** Decodes the header and returns a reader positioned at the payload. */
	static header(buf: Uint8Array): { header: Header; reader: FrameReader } {
		if (buf.length < HEADER_SIZE) throw new WireError('frame truncated')
		const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength)
		const header: Header = {
			version: view.getUint8(0),
			op: view.getUint8(1),
			flags: view.getUint16(2, true),
			id: view.getUint32(4, true),
		}
		if (header.version !== PROTOCOL_VERSION) {
			throw new WireError(
				`unsupported frame version ${header.version}, want ${PROTOCOL_VERSION}`,
			)
		}
		return { header, reader: new FrameReader(buf, HEADER_SIZE) }
	}

	get remaining(): number {
		return this.buf.length - this.i
	}

	private need(n: number): number {
		if (this.remaining < n) throw new WireError('frame truncated')
		const at = this.i
		this.i += n
		return at
	}

	u8(): number {
		return this.view.getUint8(this.need(1))
	}

	bool(): boolean {
		return this.u8() !== 0
	}

	u16(): number {
		return this.view.getUint16(this.need(2), true)
	}

	u32(): number {
		return this.view.getUint32(this.need(4), true)
	}

	i32(): number {
		return this.view.getInt32(this.need(4), true)
	}

	i64(): bigint {
		return this.view.getBigInt64(this.need(8), true)
	}

	f64(): number {
		return this.view.getFloat64(this.need(8), true)
	}

	/** Length-prefixed bytes, as a subarray of the frame — not a copy. */
	bytes(): Uint8Array {
		const n = this.u32()
		if (n === ABSENT) {
			throw new WireError('absent marker in a non-nullable field')
		}
		const at = this.need(n)
		return this.buf.subarray(at, at + n)
	}

	str(): string {
		return decoder.decode(this.bytes())
	}

	nstr(): string | null {
		const n = this.u32()
		if (n === ABSENT) return null
		const at = this.need(n)
		return decoder.decode(this.buf.subarray(at, at + n))
	}

	tag(): Tag {
		const t = this.u8()
		if (t > Tag.BLOB) {
			throw new WireError(`unknown value tag 0x${t.toString(16).padStart(2, '0')}`)
		}
		return t as Tag
	}
}
