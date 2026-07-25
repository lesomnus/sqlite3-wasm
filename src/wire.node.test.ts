import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, test } from 'vitest'

import {
	Flag,
	FrameReader,
	FrameWriter,
	Op,
	PROTOCOL_VERSION,
	Tag,
	WireError,
} from './wire'

// The corpus is produced by scripts/gen-wire-vectors.py, a third
// implementation of docs/PROTOCOL.md written independently of this file and of
// binding/wire. Agreement between all three is what keeps the two halves of the
// protocol from drifting.

type VectorOp = [string] | [string, string | number | boolean | null]

type Vector = {
	name: string
	op: keyof typeof Op
	flags: number
	id: number
	ops: VectorOp[]
	hex: string
}

const corpus: { protocolVersion: number; vectors: Vector[] } = JSON.parse(
	readFileSync(fileURLToPath(new URL('../testdata/wire/vectors.json', import.meta.url)), 'utf8'),
)

function hexToBytes(s: string): Uint8Array {
	const out = new Uint8Array(s.length / 2)
	for (let i = 0; i < out.length; i++) out[i] = parseInt(s.substr(i * 2, 2), 16)
	return out
}

function bytesToHex(b: Uint8Array): string {
	let s = ''
	for (const v of b) s += v.toString(16).padStart(2, '0')
	return s
}

/** i64 and f64 travel through the corpus as strings so no vector loses precision. */
function f64(s: string): number {
	return Number(s)
}

test('corpus matches this protocol version', () => {
	expect(corpus.protocolVersion).toBe(PROTOCOL_VERSION)
	expect(corpus.vectors.length).toBeGreaterThan(0)
})

describe('encode', () => {
	for (const v of corpus.vectors) {
		test(v.name, () => {
			const w = new FrameWriter(Op[v.op], v.flags, v.id)
			for (const [kind, arg] of v.ops) {
				switch (kind) {
					case 'u8': w.u8(arg as number); break
					case 'bool': w.bool(arg as boolean); break
					case 'u16': w.u16(arg as number); break
					case 'u32': w.u32(arg as number); break
					case 'i32': w.i32(arg as number); break
					case 'i64': w.i64(BigInt(arg as string)); break
					case 'f64': w.f64(f64(arg as string)); break
					case 'str': w.str(arg as string); break
					case 'nstr': w.nstr(arg as string | null); break
					case 'bytes': w.bytes(hexToBytes(arg as string)); break
					case 'vnull': w.valueNull(); break
					case 'vint': w.valueInt(BigInt(arg as string)); break
					case 'vreal': w.valueReal(f64(arg as string)); break
					case 'vtext': w.valueText(arg as string); break
					case 'vtextb': w.valueTextBytes(hexToBytes(arg as string)); break
					case 'vblob': w.valueBlob(hexToBytes(arg as string)); break
					default: throw new Error(`unknown op kind ${kind}`)
				}
			}
			expect(bytesToHex(w.frame())).toBe(v.hex)
		})
	}
})

describe('decode', () => {
	for (const v of corpus.vectors) {
		test(v.name, () => {
			const { header, reader } = FrameReader.header(hexToBytes(v.hex))
			expect(header.op).toBe(Op[v.op])
			expect(header.flags).toBe(v.flags)
			expect(header.id).toBe(v.id)

			for (const [kind, arg] of v.ops) {
				switch (kind) {
					case 'u8': expect(reader.u8()).toBe(arg); break
					case 'bool': expect(reader.bool()).toBe(arg); break
					case 'u16': expect(reader.u16()).toBe(arg); break
					case 'u32': expect(reader.u32()).toBe(arg); break
					case 'i32': expect(reader.i32()).toBe(arg); break
					case 'i64': expect(reader.i64()).toBe(BigInt(arg as string)); break
					// Object.is keeps -0 distinct from +0 and treats NaN as
					// equal to NaN, which is exactly the contract in
					// docs/PROTOCOL.md 4.2.
					case 'f64': expect(Object.is(reader.f64(), f64(arg as string))).toBe(true); break
					case 'str': expect(reader.str()).toBe(arg); break
					case 'nstr': expect(reader.nstr()).toBe(arg ?? null); break
					case 'bytes': expect(bytesToHex(reader.bytes())).toBe(arg); break
					case 'vnull': expect(reader.tag()).toBe(Tag.NULL); break
					case 'vint':
						expect(reader.tag()).toBe(Tag.INT)
						expect(reader.i64()).toBe(BigInt(arg as string))
						break
					case 'vreal':
						expect(reader.tag()).toBe(Tag.REAL)
						expect(Object.is(reader.f64(), f64(arg as string))).toBe(true)
						break
					case 'vtext':
						expect(reader.tag()).toBe(Tag.TEXT)
						expect(reader.str()).toBe(arg)
						break
					case 'vtextb':
						expect(reader.tag()).toBe(Tag.TEXT)
						expect(bytesToHex(reader.bytes())).toBe(arg)
						break
					case 'vblob':
						expect(reader.tag()).toBe(Tag.BLOB)
						expect(bytesToHex(reader.bytes())).toBe(arg)
						break
					default: throw new Error(`unknown op kind ${kind}`)
				}
			}

			expect(reader.remaining).toBe(0)
		})
	}
})

describe('rejections', () => {
	test('a frame from another protocol version', () => {
		const b = new Uint8Array([PROTOCOL_VERSION + 1, Op.OK, 0, 0, 0, 0, 0, 0])
		expect(() => FrameReader.header(b)).toThrow(WireError)
	})

	test('a frame shorter than its header', () => {
		for (let n = 0; n < 8; n++) {
			expect(() => FrameReader.header(new Uint8Array(n))).toThrow(WireError)
		}
	})

	test('an unknown value tag', () => {
		expect(() => new FrameReader(new Uint8Array([0x05])).tag()).toThrow(WireError)
	})

	test('the absent marker in a non-nullable field', () => {
		const r = new FrameReader(new Uint8Array([0xff, 0xff, 0xff, 0xff]))
		expect(() => r.bytes()).toThrow(WireError)
	})

	test('reading past the end of a frame', () => {
		const r = new FrameReader(new Uint8Array([1, 2, 3]))
		r.u8()
		expect(() => r.i64()).toThrow(WireError)
	})
})

describe('writer', () => {
	// The row encoder writes into one buffer for a whole batch, so growth has
	// to preserve everything already written.
	test('growing the buffer preserves earlier writes', () => {
		const w = new FrameWriter(Op.ROWS, 0, 1, 0)
		const want: number[] = []
		for (let i = 0; i < 5000; i++) {
			w.u8(i & 0xff)
			want.push(i & 0xff)
		}
		const { reader } = FrameReader.header(w.frame())
		for (const v of want) expect(reader.u8()).toBe(v)
		expect(reader.remaining).toBe(0)
	})

	test('flags stay rewritable after the payload is written', () => {
		const w = new FrameWriter(Op.ROWS, 0, 1)
		w.u32(3)
		w.setFlags(Flag.EOF | Flag.HAS_COLUMNS)
		const { header } = FrameReader.header(w.frame())
		expect(header.flags).toBe(Flag.EOF | Flag.HAS_COLUMNS)
	})

	// frame() returns a view onto a larger buffer; posting the view and
	// transferring view.buffer is what lets Go copy it out with no wrapper.
	test('frame() is a view, and reading it back honours byteOffset', () => {
		const w = new FrameWriter(Op.OK, 0, 42, 1024)
		w.str('hello')
		const f = w.frame()
		expect(f.byteLength).toBeLessThan(f.buffer.byteLength)

		const moved = new Uint8Array(f.byteLength + 3)
		moved.set(f, 3)
		const { header, reader } = FrameReader.header(moved.subarray(3))
		expect(header.id).toBe(42)
		expect(reader.str()).toBe('hello')
	})

	test('a string longer than the initial capacity', () => {
		const w = new FrameWriter(Op.OK, 0, 1, 0)
		const s = '☃'.repeat(10_000)
		w.str(s)
		const { reader } = FrameReader.header(w.frame())
		expect(reader.str()).toBe(s)
	})
})
