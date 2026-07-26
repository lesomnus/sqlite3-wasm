/**
 * A minimal protocol client used by the worker's own tests.
 *
 * It exists so the DB worker can be exercised end to end without Go in the
 * picture: everything here mirrors what `binding/` will do on the Go side, so a
 * failure points at the worker rather than at the transport.
 */

import { Cap, Flag, FrameReader, FrameWriter, type Header, Op, Tag } from '../wire'

export type Value = null | bigint | number | string | Uint8Array

export type DecodedColumn = { name: string; declType: string | null }

export type ResultSet = {
	columns: DecodedColumn[]
	rows: Value[][]
	/** Storage-class tag per cell, so tests can assert INTEGER vs REAL. */
	tags: number[][]
}

export type Ready = {
	protocolVersion: number
	sqliteVersion: string
	capabilities: number
	vfsList: string[]
	cancel: SharedArrayBuffer | null
}

export class ProtocolError extends Error {
	readonly code: number
	readonly extendedCode: number
	readonly offset: number

	constructor(code: number, extendedCode: number, offset: number, message: string) {
		super(message)
		this.name = 'ProtocolError'
		this.code = code
		this.extendedCode = extendedCode
		this.offset = offset
	}
}

type Pending = {
	resolve(v: { header: Header; reader: FrameReader }): void
	reject(e: unknown): void
	/** Set for streaming requests; returns true once the stream is finished. */
	onFrame?(header: Header, reader: FrameReader): boolean
}

export class Client {
	readonly ready: Promise<Ready>

	private readonly worker: Worker
	private readonly pending = new Map<number, Pending>()
	private nextId = 1
	private cancelView: Int32Array | null = null

	constructor(worker: Worker) {
		this.worker = worker

		this.ready = new Promise<Ready>((resolve, reject) => {
			const timer = setTimeout(() => reject(new Error('worker never became ready')), 15_000)
			worker.onerror = (e) => {
				clearTimeout(timer)
				reject(new Error(e.message ?? 'worker failed to construct'))
			}
			worker.onmessageerror = () => {
				clearTimeout(timer)
				reject(new Error('worker sent an undeserialisable message'))
			}
			worker.onmessage = (e) => {
				clearTimeout(timer)
				// The handshake is the one message that is not a bare
				// Uint8Array, because a SharedArrayBuffer cannot ride inside
				// one nor be transferred.
				const data = e.data as { p: Uint8Array; cancel: SharedArrayBuffer | null }
				if (!(data?.p instanceof Uint8Array)) {
					// An error before the handshake completed.
					try {
						this.receive(e.data)
					} catch (err) {
						reject(err)
					}
					reject(new Error('worker failed during startup'))
					return
				}
				const { header, reader } = FrameReader.header(data.p)
				if (header.op !== Op.READY) {
					reject(new Error(`expected READY, got op 0x${header.op.toString(16)}`))
					return
				}
				const info: Ready = {
					protocolVersion: reader.u16(),
					sqliteVersion: reader.str(),
					capabilities: reader.u32(),
					vfsList: [],
					cancel: data.cancel,
				}
				const n = reader.u32()
				for (let i = 0; i < n; i++) info.vfsList.push(reader.str())

				if (data.cancel) this.cancelView = new Int32Array(data.cancel)
				worker.onmessage = (ev) => this.receive(ev.data)
				resolve(info)
			}
		})
	}

	terminate(): void {
		this.worker.terminate()
	}

	has(cap: number): Promise<boolean> {
		return this.ready.then((r) => (r.capabilities & cap) !== 0)
	}

	private receive(data: unknown): void {
		if (!(data instanceof Uint8Array)) throw new Error('expected a Uint8Array frame')
		const { header, reader } = FrameReader.header(data)
		const p = this.pending.get(header.id)
		if (!p) return // a late frame for an abandoned request

		if (header.op === Op.ERROR) {
			this.pending.delete(header.id)
			p.reject(new ProtocolError(reader.i32(), reader.i32(), reader.i32(), reader.str()))
			return
		}
		if (p.onFrame) {
			if (p.onFrame(header, reader)) this.pending.delete(header.id)
			return
		}
		this.pending.delete(header.id)
		p.resolve({ header, reader })
	}

	private send(w: FrameWriter): void {
		const frame = w.frame()
		this.worker.postMessage(frame, [frame.buffer as ArrayBuffer])
	}

	private call(
		op: number,
		build: (w: FrameWriter, id: number) => void,
	): Promise<{ header: Header; reader: FrameReader }> {
		const id = this.nextId++
		const w = new FrameWriter(op, 0, id, 256)
		build(w, id)
		return new Promise((resolve, reject) => {
			this.pending.set(id, { resolve, reject })
			this.send(w)
		})
	}

	async open(filename: string, vfs = '', flags = 0): Promise<{ dbId: number; cancelSlot: number; cancellable: boolean }> {
		const { reader } = await this.call(Op.OPEN, (w) => {
			w.str(filename)
			w.str(vfs)
			w.i32(flags)
		})
		return { dbId: reader.u32(), cancelSlot: reader.u32(), cancellable: reader.bool() }
	}

	async close(dbId: number): Promise<void> {
		await this.call(Op.CLOSE, (w) => w.u32(dbId))
	}

	async exec(
		dbId: number,
		sql: string,
		args: Arg[] = [],
	): Promise<{ changes: bigint; lastInsertRowId: bigint }> {
		const { reader } = await this.call(Op.EXEC, (w) => {
			w.u32(dbId)
			w.str(sql)
			writeArgs(w, args)
		})
		return { changes: reader.i64(), lastInsertRowId: reader.i64() }
	}

	async prepare(dbId: number, sql: string) {
		const { reader } = await this.call(Op.PREPARE, (w) => {
			w.u32(dbId)
			w.str(sql)
		})
		const out = {
			stmtId: reader.u32(),
			paramCount: reader.u32(),
			paramCountExact: reader.bool(),
			tailOffset: reader.u32(),
			readOnly: reader.bool(),
			columns: readColumns(reader),
		}
		return out
	}

	async finalize(stmtId: number): Promise<void> {
		await this.call(Op.FINALIZE, (w) => w.u32(stmtId))
	}

	query(dbId: number, sql: string, args: Arg[] = []): Promise<ResultSet> {
		return this.stream(Op.QUERY, (w) => {
			w.u32(dbId)
			w.str(sql)
			writeArgs(w, args)
		})
	}

	queryStmt(stmtId: number, args: Arg[] = []): Promise<ResultSet> {
		return this.stream(Op.QUERY_STMT, (w) => {
			w.u32(stmtId)
			writeArgs(w, args)
		})
	}

	/** Aborts a streaming request the way Rows.Close does: fire and forget. */
	abort(id: number): void {
		this.send(new FrameWriter(Op.ABORT, 0, id))
	}

	/**
	 * Posts a QUERY with no consumer registered, so its rows are discarded and
	 * it is never granted credit.
	 *
	 * This is exactly the state Go leaves behind when Rows.Close tears the
	 * route down: the request is in flight, but nothing will ever read from it
	 * again. Tests need it to reproduce a stream that outlives its reader.
	 */
	postOrphanQuery(dbId: number, sql: string): number {
		const id = this.nextId++
		const w = new FrameWriter(Op.QUERY, 0, id, 256)
		w.u32(dbId)
		w.str(sql)
		writeArgs(w, [])
		this.send(w)
		return id
	}

	/** Sets the shared cancellation word, the way a ctx watcher does. */
	cancel(slot: number, requestId: number): void {
		if (!this.cancelView) throw new Error('no SharedArrayBuffer')
		Atomics.store(this.cancelView, slot, requestId)
	}

	/** The id the next request will use, so a test can cancel it. */
	get nextRequestId(): number {
		return this.nextId
	}

	private stream(op: number, build: (w: FrameWriter) => void): Promise<ResultSet> {
		const id = this.nextId++
		const w = new FrameWriter(op, 0, id, 256)
		build(w)

		const out: ResultSet = { columns: [], rows: [], tags: [] }
		return new Promise<ResultSet>((resolve, reject) => {
			this.pending.set(id, {
				resolve: () => {},
				reject,
				onFrame: (header, reader) => {
					if (header.op === Op.ABORTED) {
						resolve(out)
						return true
					}
					if (header.op !== Op.ROWS) {
						reject(new Error(`unexpected op 0x${header.op.toString(16)} in a result set`))
						return true
					}
					if (header.flags & Flag.HAS_COLUMNS) out.columns = readColumns(reader)

					const rowCount = reader.u32()
					for (let i = 0; i < rowCount; i++) {
						const row: Value[] = []
						const tags: number[] = []
						for (let c = 0; c < out.columns.length; c++) {
							const tag = reader.tag()
							tags.push(tag)
							row.push(readValue(reader, tag))
						}
						out.rows.push(row)
						out.tags.push(tags)
					}

					if (header.flags & Flag.EOF) {
						resolve(out)
						return true
					}
					// Grant credit from the consumer, exactly as Rows.Next will.
					const credit = new FrameWriter(Op.CREDIT, 0, id, 4)
					credit.u32(1)
					this.send(credit)
					return false
				},
			})
			this.send(w)
		})
	}
}

function readColumns(r: FrameReader): DecodedColumn[] {
	const n = r.u32()
	const out: DecodedColumn[] = new Array(n)
	for (let i = 0; i < n; i++) out[i] = { name: r.str(), declType: r.nstr() }
	return out
}

function readValue(r: FrameReader, tag: number): Value {
	switch (tag) {
		case Tag.NULL:
			return null
		case Tag.INT:
			return r.i64()
		case Tag.REAL:
			return r.f64()
		case Tag.TEXT:
			return new TextDecoder().decode(r.bytes())
		default:
			return r.bytes().slice()
	}
}

/** Named arguments are written as {name, ordinal, value}; see PROTOCOL.md §4.3. */
export type NamedValue = { name: string; value: Value }

export type Arg = Value | NamedValue

function writeArgs(w: FrameWriter, args: Arg[]): void {
	w.u32(args.length)
	for (let i = 0; i < args.length; i++) {
		const a = args[i]
		const named = isNamed(a)
		w.nstr(named ? a.name : null)
		w.u32(i + 1)
		writeValue(w, named ? a.value : (a as Value))
	}
}

function isNamed(v: unknown): v is NamedValue {
	return typeof v === 'object' && v !== null && 'name' in v && 'value' in v
}

function writeValue(w: FrameWriter, v: Value): void {
	if (v === null) w.valueNull()
	else if (typeof v === 'bigint') w.valueInt(v)
	else if (typeof v === 'number') w.valueReal(v)
	else if (typeof v === 'string') w.valueText(v)
	else w.valueBlob(v)
}

export { Cap }
