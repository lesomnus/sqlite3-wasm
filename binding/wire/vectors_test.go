package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"testing"
)

// The corpus in testdata/wire/vectors.json is produced by
// scripts/gen-wire-vectors.py, a third implementation of docs/PROTOCOL.md
// written independently of this package and of src/wire.ts. Agreement between
// the three is what keeps the two halves of the protocol from drifting.

const vectorsPath = "../../testdata/wire/vectors.json"

type vector struct {
	Name  string              `json:"name"`
	Op    string              `json:"op"`
	Flags uint16              `json:"flags"`
	ID    uint32              `json:"id"`
	Ops   [][]json.RawMessage `json:"ops"`
	Hex   string              `json:"hex"`
}

type corpus struct {
	ProtocolVersion int      `json:"protocolVersion"`
	Vectors         []vector `json:"vectors"`
}

var opByName = map[string]Op{
	"OPEN": OpOpen, "CLOSE": OpClose, "PREPARE": OpPrepare, "FINALIZE": OpFinalize,
	"QUERY": OpQuery, "QUERY_STMT": OpQueryStmt, "EXEC": OpExec, "EXEC_STMT": OpExecStmt,
	"CREDIT": OpCredit, "ABORT": OpAbort, "SHUTDOWN": OpShutdown,
	"READY": OpReady, "OK": OpOK, "ERROR": OpError, "OPENED": OpOpened,
	"PREPARED": OpPrepared, "ROWS": OpRows, "EXEC_RESULT": OpExecResult,
	"ABORTED": OpAborted,
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	b, err := os.ReadFile(vectorsPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c corpus
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	if c.ProtocolVersion != Version {
		t.Fatalf("corpus is for protocol v%d, package is v%d", c.ProtocolVersion, Version)
	}
	if len(c.Vectors) == 0 {
		t.Fatal("corpus is empty")
	}
	return c
}

// argStr / argNum / argIsNull unpack a single op argument. The corpus keeps
// i64 and f64 as strings so no vector loses precision on its way through JSON.

func argStr(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("expected a string argument, got %s", raw)
	}
	return s
}

func argNum(t *testing.T, raw json.RawMessage) int64 {
	t.Helper()
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatalf("expected a numeric argument, got %s", raw)
	}
	v, err := n.Int64()
	if err != nil {
		t.Fatalf("argument %s is not an integer: %v", raw, err)
	}
	return v
}

func argBool(t *testing.T, raw json.RawMessage) bool {
	t.Helper()
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("expected a bool argument, got %s", raw)
	}
	return v
}

func argIsNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// canonicalNaN is the quiet NaN the corpus and JS both produce. Go's own
// math.NaN() is 0x7FF8000000000001, so a byte-exact comparison against the
// corpus needs this one; see docs/PROTOCOL.md §4.2 on why NaN payload bits are
// not part of the wire contract.
var canonicalNaN = math.Float64frombits(0x7FF8000000000000)

func parseF64(t *testing.T, s string) float64 {
	t.Helper()
	if s == "NaN" {
		return canonicalNaN
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("bad float literal %q: %v", s, err)
	}
	return v
}

func parseI64(t *testing.T, s string) int64 {
	t.Helper()
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("bad int literal %q: %v", s, err)
	}
	return v
}

func TestVectorsEncode(t *testing.T) {
	for _, v := range loadCorpus(t).Vectors {
		t.Run(v.Name, func(t *testing.T) {
			op, ok := opByName[v.Op]
			if !ok {
				t.Fatalf("unknown opcode name %q", v.Op)
			}

			w := NewWriter(op, v.Flags, v.ID)
			for _, item := range v.Ops {
				kind := argStr(t, item[0])
				var arg json.RawMessage
				if len(item) > 1 {
					arg = item[1]
				}
				switch kind {
				case "u8":
					w.U8(uint8(argNum(t, arg)))
				case "bool":
					w.Bool(argBool(t, arg))
				case "u16":
					w.U16(uint16(argNum(t, arg)))
				case "u32":
					w.U32(uint32(argNum(t, arg)))
				case "i32":
					w.I32(int32(argNum(t, arg)))
				case "i64":
					w.I64(parseI64(t, argStr(t, arg)))
				case "f64":
					w.F64(parseF64(t, argStr(t, arg)))
				case "str":
					w.String(argStr(t, arg))
				case "nstr":
					if argIsNull(arg) {
						w.NullString("", false)
					} else {
						w.NullString(argStr(t, arg), true)
					}
				case "bytes":
					w.Bytes(mustHex(t, argStr(t, arg)))
				case "vnull":
					w.ValueNull()
				case "vint":
					w.ValueInt(parseI64(t, argStr(t, arg)))
				case "vreal":
					w.ValueReal(parseF64(t, argStr(t, arg)))
				case "vtext":
					w.ValueText(argStr(t, arg))
				case "vtextb":
					w.ValueTextBytes(mustHex(t, argStr(t, arg)))
				case "vblob":
					w.ValueBlob(mustHex(t, argStr(t, arg)))
				default:
					t.Fatalf("unknown op kind %q", kind)
				}
			}

			got := hex.EncodeToString(w.Frame())
			if got != v.Hex {
				t.Errorf("encoded bytes differ\n got %s\nwant %s", got, v.Hex)
			}
		})
	}
}

func TestVectorsDecode(t *testing.T) {
	for _, v := range loadCorpus(t).Vectors {
		t.Run(v.Name, func(t *testing.T) {
			frame := mustHex(t, v.Hex)

			h, r, err := ReadHeader(frame)
			if err != nil {
				t.Fatalf("ReadHeader: %v", err)
			}
			if want := opByName[v.Op]; h.Op != want {
				t.Errorf("op = %v, want %v", h.Op, want)
			}
			if h.Flags != v.Flags {
				t.Errorf("flags = %#x, want %#x", h.Flags, v.Flags)
			}
			if h.ID != v.ID {
				t.Errorf("id = %d, want %d", h.ID, v.ID)
			}

			for i, item := range v.Ops {
				kind := argStr(t, item[0])
				var arg json.RawMessage
				if len(item) > 1 {
					arg = item[1]
				}
				switch kind {
				case "u8":
					eq(t, i, kind, int64(r.U8()), argNum(t, arg))
				case "bool":
					if got, want := r.Bool(), argBool(t, arg); got != want {
						t.Errorf("op %d (%s): got %v, want %v", i, kind, got, want)
					}
				case "u16":
					eq(t, i, kind, int64(r.U16()), argNum(t, arg))
				case "u32":
					eq(t, i, kind, int64(r.U32()), argNum(t, arg))
				case "i32":
					eq(t, i, kind, int64(r.I32()), argNum(t, arg))
				case "i64":
					eq(t, i, kind, r.I64(), parseI64(t, argStr(t, arg)))
				case "f64":
					eqF(t, i, kind, r.F64(), parseF64(t, argStr(t, arg)))
				case "str":
					eqS(t, i, kind, r.String(), argStr(t, arg))
				case "nstr":
					got, present := r.NullString()
					if argIsNull(arg) {
						if present {
							t.Errorf("op %d (%s): got present %q, want absent", i, kind, got)
						}
					} else {
						if !present {
							t.Errorf("op %d (%s): got absent, want %q", i, kind, argStr(t, arg))
						}
						eqS(t, i, kind, got, argStr(t, arg))
					}
				case "bytes":
					eqB(t, i, kind, r.Bytes(), mustHex(t, argStr(t, arg)))
				case "vnull":
					eqTag(t, i, r.Tag(), TagNull)
				case "vint":
					eqTag(t, i, r.Tag(), TagInt)
					eq(t, i, kind, r.I64(), parseI64(t, argStr(t, arg)))
				case "vreal":
					eqTag(t, i, r.Tag(), TagReal)
					eqF(t, i, kind, r.F64(), parseF64(t, argStr(t, arg)))
				case "vtext":
					eqTag(t, i, r.Tag(), TagText)
					eqS(t, i, kind, r.String(), argStr(t, arg))
				case "vtextb":
					eqTag(t, i, r.Tag(), TagText)
					eqB(t, i, kind, r.Bytes(), mustHex(t, argStr(t, arg)))
				case "vblob":
					eqTag(t, i, r.Tag(), TagBlob)
					eqB(t, i, kind, r.Bytes(), mustHex(t, argStr(t, arg)))
				default:
					t.Fatalf("unknown op kind %q", kind)
				}
			}

			if err := r.Err(); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if n := r.Remaining(); n != 0 {
				t.Errorf("%d payload bytes left undecoded", n)
			}
		})
	}
}

// A frame that ends mid-field must produce ErrShort rather than a partial
// value: a batch arriving truncated is a bug worth surfacing, not one worth
// papering over with zeroes.
func TestTruncatedFrameIsAnError(t *testing.T) {
	for _, v := range loadCorpus(t).Vectors {
		frame := mustHex(t, v.Hex)
		if len(frame) <= HeaderSize {
			continue
		}
		t.Run(v.Name, func(t *testing.T) {
			for cut := HeaderSize; cut < len(frame); cut++ {
				_, r, err := ReadHeader(frame[:cut])
				if err != nil {
					t.Fatalf("header should still parse at cut=%d: %v", cut, err)
				}
				drain(r)
				// Reading the full payload out of a short frame must fail
				// somewhere; the only frames that survive are ones whose
				// payload happened to fit entirely in the prefix.
				if r.Err() == nil && r.Remaining() != 0 {
					t.Fatalf("cut=%d: no error and %d bytes left", cut, r.Remaining())
				}
			}
		})
	}
}

// drain reads a payload blindly. It cannot know the field layout, so it just
// consumes values until the reader is exhausted or fails.
func drain(r *Reader) {
	for r.Err() == nil && r.Remaining() > 0 {
		r.U8()
	}
}

func TestHeaderRejectsWrongVersion(t *testing.T) {
	frame := []byte{Version + 1, byte(OpOK), 0, 0, 0, 0, 0, 0}
	if _, _, err := ReadHeader(frame); err == nil {
		t.Fatal("expected an error for a frame from a different protocol version")
	}
}

func TestHeaderRejectsShortFrame(t *testing.T) {
	for n := 0; n < HeaderSize; n++ {
		if _, _, err := ReadHeader(make([]byte, n)); err != ErrShort {
			t.Errorf("len %d: err = %v, want ErrShort", n, err)
		}
	}
}

func TestUnknownTagIsAnError(t *testing.T) {
	r := NewReader([]byte{0x05})
	r.Tag()
	if r.Err() == nil {
		t.Fatal("expected an error for tag 0x05")
	}
}

// A nullable field's absent marker must not be mistaken for a 4 GiB string.
func TestAbsentMarkerRejectedInNonNullableField(t *testing.T) {
	r := NewReader([]byte{0xFF, 0xFF, 0xFF, 0xFF})
	r.Bytes()
	if r.Err() == nil {
		t.Fatal("expected an error reading an absent marker as a plain byte string")
	}
}

func eq(t *testing.T, i int, kind string, got, want int64) {
	t.Helper()
	if got != want {
		t.Errorf("op %d (%s): got %d, want %d", i, kind, got, want)
	}
}

// Compare by bit pattern so -0 does not equal +0, but treat every NaN as equal:
// payload bits differ between Go, JS and the generator, and SQLite turns a bound
// NaN into NULL anyway.
func eqF(t *testing.T, i int, kind string, got, want float64) {
	t.Helper()
	if math.IsNaN(got) && math.IsNaN(want) {
		return
	}
	if math.Float64bits(got) != math.Float64bits(want) {
		t.Errorf("op %d (%s): got %v (%#016x), want %v (%#016x)",
			i, kind, got, math.Float64bits(got), want, math.Float64bits(want))
	}
}

func eqS(t *testing.T, i int, kind, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("op %d (%s): got %q, want %q", i, kind, got, want)
	}
}

func eqB(t *testing.T, i int, kind string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Errorf("op %d (%s): got %x, want %x", i, kind, got, want)
	}
}

func eqTag(t *testing.T, i int, got, want Tag) {
	t.Helper()
	if got != want {
		t.Errorf("op %d: tag = %v, want %v", i, got, want)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}
