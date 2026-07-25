package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// ErrShort reports a frame that ended in the middle of a field.
var ErrShort = errors.New("wire: frame truncated")

// ---------------------------------------------------------------------------
// Writer

// Writer builds a frame. Methods never fail; the buffer simply grows.
type Writer struct {
	b []byte
}

// NewWriter starts a frame with the given header. Payload fields are appended
// by the methods below in the order given by docs/PROTOCOL.md.
func NewWriter(op Op, flags uint16, id uint32) *Writer {
	w := &Writer{b: make([]byte, HeaderSize, 64)}
	w.b[0] = Version
	w.b[1] = byte(op)
	binary.LittleEndian.PutUint16(w.b[2:], flags)
	binary.LittleEndian.PutUint32(w.b[4:], id)
	return w
}

// NewWriterSize is NewWriter with a hint for the eventual payload size.
func NewWriterSize(op Op, flags uint16, id uint32, hint int) *Writer {
	w := &Writer{b: make([]byte, HeaderSize, HeaderSize+hint)}
	w.b[0] = Version
	w.b[1] = byte(op)
	binary.LittleEndian.PutUint16(w.b[2:], flags)
	binary.LittleEndian.PutUint32(w.b[4:], id)
	return w
}

// SetFlags rewrites the header flags. Batch encoders do not know whether a
// frame is the last one until they have filled it.
func (w *Writer) SetFlags(flags uint16) {
	binary.LittleEndian.PutUint16(w.b[2:], flags)
}

// Len is the current frame size including the header.
func (w *Writer) Len() int { return len(w.b) }

// Frame returns the encoded frame. The Writer must not be used afterwards.
func (w *Writer) Frame() []byte { return w.b }

func (w *Writer) U8(v uint8)  { w.b = append(w.b, v) }
func (w *Writer) Bool(v bool) { w.b = append(w.b, b2u(v)) }

func (w *Writer) U16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *Writer) U32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *Writer) I32(v int32)  { w.U32(uint32(v)) }
func (w *Writer) I64(v int64)  { w.b = binary.LittleEndian.AppendUint64(w.b, uint64(v)) }

func (w *Writer) F64(v float64) {
	w.b = binary.LittleEndian.AppendUint64(w.b, math.Float64bits(v))
}

// Bytes writes a length-prefixed byte string.
func (w *Writer) Bytes(v []byte) {
	w.U32(uint32(len(v)))
	w.b = append(w.b, v...)
}

// String writes a length-prefixed UTF-8 string.
func (w *Writer) String(v string) {
	w.U32(uint32(len(v)))
	w.b = append(w.b, v...)
}

// NullString writes a nullable string. An absent string is distinct from an
// empty one: decltype is absent for expression columns but empty for a column
// declared with no type.
func (w *Writer) NullString(v string, present bool) {
	if !present {
		w.U32(absentLen)
		return
	}
	w.String(v)
}

func (w *Writer) ValueNull()          { w.U8(uint8(TagNull)) }
func (w *Writer) ValueInt(v int64)    { w.U8(uint8(TagInt)); w.I64(v) }
func (w *Writer) ValueReal(v float64) { w.U8(uint8(TagReal)); w.F64(v) }
func (w *Writer) ValueText(v string)  { w.U8(uint8(TagText)); w.String(v) }

// ValueTextBytes writes a TEXT value whose bytes are already UTF-8 encoded.
// SQLite does not guarantee that TEXT is valid UTF-8, so the bytes are passed
// through untouched.
func (w *Writer) ValueTextBytes(v []byte) { w.U8(uint8(TagText)); w.Bytes(v) }
func (w *Writer) ValueBlob(v []byte)      { w.U8(uint8(TagBlob)); w.Bytes(v) }

func b2u(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// Reader

// Reader decodes a frame. Errors are sticky: the first failure poisons every
// subsequent read, so a caller may decode a whole payload and check Err once.
//
// Bytes and TextBytes return sub-slices of the underlying frame rather than
// copies. Rows.Next decodes one row at a time straight into the destination
// slice, so the frame must stay alive for as long as the batch is in use.
type Reader struct {
	b   []byte
	i   int
	err error
}

// ReadHeader decodes the frame header and returns a Reader positioned at the
// payload.
func ReadHeader(b []byte) (Header, *Reader, error) {
	if len(b) < HeaderSize {
		return Header{}, nil, ErrShort
	}
	h := Header{
		Version: b[0],
		Op:      Op(b[1]),
		Flags:   binary.LittleEndian.Uint16(b[2:]),
		ID:      binary.LittleEndian.Uint32(b[4:]),
	}
	if h.Version != Version {
		return h, nil, fmt.Errorf("wire: unsupported frame version %d, want %d", h.Version, Version)
	}
	return h, &Reader{b: b, i: HeaderSize}, nil
}

// NewReader decodes a payload-only buffer. It exists for tests; frames from the
// worker always go through ReadHeader.
func NewReader(b []byte) *Reader { return &Reader{b: b} }

// Err reports the first decoding failure, if any.
func (r *Reader) Err() error { return r.err }

// Remaining reports how many payload bytes are left.
func (r *Reader) Remaining() int { return len(r.b) - r.i }

func (r *Reader) fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *Reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.b)-r.i < n {
		r.fail(ErrShort)
		return nil
	}
	s := r.b[r.i : r.i+n]
	r.i += n
	return s
}

func (r *Reader) U8() uint8 {
	s := r.take(1)
	if s == nil {
		return 0
	}
	return s[0]
}

func (r *Reader) Bool() bool { return r.U8() != 0 }

func (r *Reader) U16() uint16 {
	s := r.take(2)
	if s == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(s)
}

func (r *Reader) U32() uint32 {
	s := r.take(4)
	if s == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(s)
}

func (r *Reader) I32() int32 { return int32(r.U32()) }

func (r *Reader) I64() int64 {
	s := r.take(8)
	if s == nil {
		return 0
	}
	return int64(binary.LittleEndian.Uint64(s))
}

func (r *Reader) F64() float64 {
	s := r.take(8)
	if s == nil {
		return 0
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(s))
}

// Bytes returns a length-prefixed byte string as a sub-slice of the frame.
func (r *Reader) Bytes() []byte {
	n := r.U32()
	if r.err != nil {
		return nil
	}
	if n == absentLen {
		r.fail(fmt.Errorf("wire: absent marker in a non-nullable field"))
		return nil
	}
	return r.take(int(n))
}

// String returns a length-prefixed string. It allocates; prefer Bytes on hot
// paths where the frame outlives the read.
func (r *Reader) String() string { return string(r.Bytes()) }

// NullString returns a nullable string and whether it was present.
func (r *Reader) NullString() (string, bool) {
	n := r.U32()
	if r.err != nil {
		return "", false
	}
	if n == absentLen {
		return "", false
	}
	return string(r.take(int(n))), true
}

// Tag reads a value's storage-class tag.
func (r *Reader) Tag() Tag {
	t := Tag(r.U8())
	if r.err == nil && t > TagBlob {
		r.fail(fmt.Errorf("wire: unknown value tag 0x%02x", uint8(t)))
	}
	return t
}

// Skip advances past a value of the given tag without decoding it.
func (r *Reader) Skip(t Tag) {
	switch t {
	case TagNull:
	case TagInt, TagReal:
		r.take(8)
	case TagText, TagBlob:
		n := r.U32()
		r.take(int(n))
	default:
		r.fail(fmt.Errorf("wire: cannot skip tag 0x%02x", uint8(t)))
	}
}
