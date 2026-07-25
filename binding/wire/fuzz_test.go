package wire

import (
	"bytes"
	"math"
	"testing"
)

// Frames arrive from another JavaScript realm, so the decoder is the trust
// boundary: on malformed input it must report an error, never panic and never
// hand back a value it did not actually read.
func FuzzReader(f *testing.F) {
	for _, v := range []string{
		"", "01", "0181000001000000", "018100000100000003000000616263",
		"01850100010000000100000003ffffffff", // TEXT claiming 4 GiB
		"018100000100000004ffffffff",         // absent marker as a length
		"01810000010000000500000000",         // unknown value tag
	} {
		f.Add(mustHexBytes(v))
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		h, r, err := ReadHeader(b)
		if err != nil {
			return
		}
		if h.Version != Version {
			t.Fatalf("ReadHeader accepted version %d", h.Version)
		}

		// Walk the payload as a value stream. Any shape of garbage is fine as
		// long as it fails cleanly and never reads past the frame.
		for r.Err() == nil && r.Remaining() > 0 {
			before := r.Remaining()
			tag := r.Tag()
			if r.Err() != nil {
				break
			}
			r.Skip(tag)
			if r.Err() == nil && r.Remaining() >= before {
				t.Fatalf("tag %v consumed nothing: %d -> %d", tag, before, r.Remaining())
			}
		}
		if r.Remaining() < 0 {
			t.Fatalf("reader ran past the end of the frame: %d", r.Remaining())
		}
	})
}

// Every value the driver can produce must survive a round trip byte for byte.
func FuzzValueRoundTrip(f *testing.F) {
	f.Add(int64(0), 0.0, "", []byte(nil))
	f.Add(int64(9007199254740993), math.MaxFloat64, "héllo☃", []byte{0, 255})
	f.Add(int64(math.MinInt64), math.SmallestNonzeroFloat64, "a\x00b", []byte{})

	f.Fuzz(func(t *testing.T, i int64, fl float64, s string, blob []byte) {
		w := NewWriter(OpRows, FlagEOF, 1)
		w.ValueNull()
		w.ValueInt(i)
		w.ValueReal(fl)
		w.ValueText(s)
		w.ValueBlob(blob)

		_, r, err := ReadHeader(w.Frame())
		if err != nil {
			t.Fatalf("ReadHeader: %v", err)
		}

		if got := r.Tag(); got != TagNull {
			t.Fatalf("tag = %v, want NULL", got)
		}
		if got := r.Tag(); got != TagInt {
			t.Fatalf("tag = %v, want INT", got)
		}
		if got := r.I64(); got != i {
			t.Fatalf("int = %d, want %d", got, i)
		}
		if got := r.Tag(); got != TagReal {
			t.Fatalf("tag = %v, want REAL", got)
		}
		// Bit-exact, so -0 stays -0. NaN payloads are not part of the wire
		// contract (docs/PROTOCOL.md 4.2), so any NaN satisfies any NaN.
		if got := r.F64(); math.Float64bits(got) != math.Float64bits(fl) &&
			!(math.IsNaN(got) && math.IsNaN(fl)) {
			t.Fatalf("real = %v, want %v", got, fl)
		}
		if got := r.Tag(); got != TagText {
			t.Fatalf("tag = %v, want TEXT", got)
		}
		if got := r.String(); got != s {
			t.Fatalf("text = %q, want %q", got, s)
		}
		if got := r.Tag(); got != TagBlob {
			t.Fatalf("tag = %v, want BLOB", got)
		}
		if got := r.Bytes(); !bytes.Equal(got, blob) {
			t.Fatalf("blob = %x, want %x", got, blob)
		}

		if err := r.Err(); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if n := r.Remaining(); n != 0 {
			t.Fatalf("%d bytes left over", n)
		}
	})
}

func mustHexBytes(s string) []byte {
	b := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		b = append(b, unhex(s[i])<<4|unhex(s[i+1]))
	}
	return b
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	panic("bad hex digit")
}
