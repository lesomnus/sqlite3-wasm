package driver

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/sqlite3-wasm/binding/wire"
)

func utcConfig() *Config { return &Config{Loc: time.UTC} }

// encodeThenDecode runs a value through the wire codec so the test exercises
// the same path the driver does, not a shortcut around it.
func encodeThenDecode(t *testing.T, class declClass, cfg *Config, write func(*wire.Writer)) driver.Value {
	t.Helper()
	w := wire.NewWriter(wire.OpRows, wire.FlagEOF, 1)
	write(w)
	_, r, err := wire.ReadHeader(w.Frame())
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	v := decodeValue(r, class, cfg)
	if err := r.Err(); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return v
}

func TestDecodeStorageClasses(t *testing.T) {
	cfg := utcConfig()

	t.Run("NULL", func(t *testing.T) {
		got := encodeThenDecode(t, classOther, cfg, func(w *wire.Writer) { w.ValueNull() })
		if got != nil {
			t.Errorf("got %#v, want nil", got)
		}
	})

	// The whole point of the rewrite: an INTEGER stays an int64 even past 2^53,
	// and a REAL stays a float64 even when it happens to be integral.
	t.Run("INTEGER past 2^53", func(t *testing.T) {
		const v = int64(9007199254740993)
		got := encodeThenDecode(t, classOther, cfg, func(w *wire.Writer) { w.ValueInt(v) })
		if got != driver.Value(v) {
			t.Errorf("got %#v, want int64(%d)", got, v)
		}
	})

	t.Run("REAL that is integral stays a float64", func(t *testing.T) {
		got := encodeThenDecode(t, classOther, cfg, func(w *wire.Writer) { w.ValueReal(1) })
		f, ok := got.(float64)
		if !ok {
			t.Fatalf("got %T, want float64", got)
		}
		if f != 1 {
			t.Errorf("got %v, want 1", f)
		}
	})

	t.Run("negative zero survives", func(t *testing.T) {
		got := encodeThenDecode(t, classOther, cfg, func(w *wire.Writer) { w.ValueReal(math.Copysign(0, -1)) })
		if !math.Signbit(got.(float64)) {
			t.Error("-0 came back as +0")
		}
	})

	t.Run("TEXT with an embedded NUL is not truncated", func(t *testing.T) {
		const v = "a\x00b"
		got := encodeThenDecode(t, classOther, cfg, func(w *wire.Writer) { w.ValueText(v) })
		if got != driver.Value(v) {
			t.Errorf("got %q, want %q", got, v)
		}
	})

	t.Run("BLOB", func(t *testing.T) {
		v := []byte{0xde, 0xad, 0x00, 0xbe}
		got := encodeThenDecode(t, classOther, cfg, func(w *wire.Writer) { w.ValueBlob(v) })
		if !bytes.Equal(got.([]byte), v) {
			t.Errorf("got %x, want %x", got, v)
		}
	})

	// A typed nil would reach the caller through Scan(&any) and behave
	// differently from an empty slice.
	t.Run("empty BLOB is non-nil", func(t *testing.T) {
		got := encodeThenDecode(t, classOther, cfg, func(w *wire.Writer) { w.ValueBlob(nil) })
		b, ok := got.([]byte)
		if !ok {
			t.Fatalf("got %T, want []byte", got)
		}
		if b == nil {
			t.Error("empty blob decoded to a nil []byte")
		}
		if len(b) != 0 {
			t.Errorf("got %x, want empty", b)
		}
	})
}

func TestDecodeBooleanColumns(t *testing.T) {
	cfg := utcConfig()
	for _, tc := range []struct {
		in   int64
		want bool
	}{{0, false}, {1, true}, {2, true}, {-1, true}} {
		got := encodeThenDecode(t, classBool, cfg, func(w *wire.Writer) { w.ValueInt(tc.in) })
		if got != driver.Value(tc.want) {
			// mattn uses > 0 here, which reads -1 as false.
			t.Errorf("BOOLEAN %d -> %#v, want %v", tc.in, got, tc.want)
		}
	}

	// Only INTEGER converts; TEXT in a BOOLEAN column stays a string, for
	// parity with mattn.
	got := encodeThenDecode(t, classBool, cfg, func(w *wire.Writer) { w.ValueText("true") })
	if got != driver.Value("true") {
		t.Errorf("BOOLEAN 'true' -> %#v, want the string", got)
	}
}

func TestDecodeTimeColumns(t *testing.T) {
	cfg := utcConfig()

	t.Run("REAL is never converted", func(t *testing.T) {
		// A Julian day number is indistinguishable from any other float.
		got := encodeThenDecode(t, classTime, cfg, func(w *wire.Writer) { w.ValueReal(2451545.0) })
		if _, ok := got.(float64); !ok {
			t.Errorf("got %T, want float64", got)
		}
	})

	t.Run("INTEGER seconds", func(t *testing.T) {
		got := encodeThenDecode(t, classTime, cfg, func(w *wire.Writer) { w.ValueInt(1704173045) })
		tm, ok := got.(time.Time)
		if !ok {
			t.Fatalf("got %T, want time.Time", got)
		}
		if !tm.Equal(time.Unix(1704173045, 0)) {
			t.Errorf("got %v", tm)
		}
	})

	t.Run("INTEGER above 1e12 is read as milliseconds", func(t *testing.T) {
		const ms = int64(1704173045123)
		got := encodeThenDecode(t, classTime, cfg, func(w *wire.Writer) { w.ValueInt(ms) })
		if !got.(time.Time).Equal(time.UnixMilli(ms)) {
			t.Errorf("got %v, want %v", got, time.UnixMilli(ms))
		}
	})

	t.Run("explicit integer format beats the heuristic", func(t *testing.T) {
		c := &Config{Loc: time.UTC, IntegerTime: IntegerTimeUnixMilli}
		// Below 1e12, so the heuristic would call this seconds.
		const ms = int64(1000)
		got := encodeThenDecode(t, classTime, c, func(w *wire.Writer) { w.ValueInt(ms) })
		if !got.(time.Time).Equal(time.UnixMilli(ms)) {
			t.Errorf("got %v, want %v", got, time.UnixMilli(ms))
		}
	})

	// A failed parse must keep the original string. A zero time is
	// unrecoverable; a string produces a debuggable "unsupported Scan".
	t.Run("an unparseable value stays a string", func(t *testing.T) {
		for _, s := range []string{"15:04:05", "2451545.0", "not a time", ""} {
			got := encodeThenDecode(t, classTime, cfg, func(w *wire.Writer) { w.ValueText(s) })
			if got != driver.Value(s) {
				t.Errorf("%q -> %#v, want the original string", s, got)
			}
		}
	})
}

// The layouts this driver accepts, checked against the behaviour of
// mattn/go-sqlite3 and modernc.org/sqlite.
func TestParseTimeString(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	tests := []struct {
		in   string
		ok   bool
		want string // RFC3339Nano in UTC, when ok
	}{
		{"2024-01-02 15:04:05", true, "2024-01-02T15:04:05Z"},
		{"2024-01-02 15:04:05.123", true, "2024-01-02T15:04:05.123Z"},
		{"2024-01-02T15:04:05", true, "2024-01-02T15:04:05Z"},
		{"2024-01-02T15:04:05Z", true, "2024-01-02T15:04:05Z"},
		{"2024-01-02T15:04:05+09:00", true, "2024-01-02T06:04:05Z"},
		{"2024-01-02 15:04:05+09:00", true, "2024-01-02T06:04:05Z"},
		{"2024-01-02 15:04", true, "2024-01-02T15:04:00Z"},
		{"2024-01-02T15:04", true, "2024-01-02T15:04:00Z"},
		{"2024-01-02", true, "2024-01-02T00:00:00Z"},
		{"2024-01-02 15:04:05.999999999-07:00", true, "2024-01-02T22:04:05.999999999Z"},

		// SQLite's time() output, a Julian day, and an offset without a colon
		// all fail in both incumbents too.
		{"15:04:05", false, ""},
		{"2451545.0", false, ""},
		{"2024-01-02T15:04:05-0700", false, ""},
		{"", false, ""},
	}

	for _, tc := range tests {
		got, ok := parseTimeString(tc.in, time.UTC)
		if ok != tc.ok {
			t.Errorf("parseTimeString(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if s := got.UTC().Format(time.RFC3339Nano); s != tc.want {
			t.Errorf("parseTimeString(%q) = %s, want %s", tc.in, s, tc.want)
		}
	}

	// A naive timestamp keeps its wall clock and is read in the connection's
	// location — modernc's semantics. mattn instead reads it as UTC and then
	// shifts it, which changes the wall clock.
	t.Run("naive timestamps are read in the connection location", func(t *testing.T) {
		got, ok := parseTimeString("2024-01-02 15:04:05", seoul)
		if !ok {
			t.Fatal("did not parse")
		}
		if h := got.Hour(); h != 15 {
			t.Errorf("hour = %d, want 15 (wall clock must be preserved)", h)
		}
		if s := got.UTC().Format(time.RFC3339); s != "2024-01-02T06:04:05Z" {
			t.Errorf("UTC = %s, want 2024-01-02T06:04:05Z", s)
		}
	})

	// A trailing Z is load-bearing: Go's -07:00 layout does not accept it.
	t.Run("a trailing Z is UTC even with a non-UTC connection location", func(t *testing.T) {
		got, ok := parseTimeString("2024-01-02T15:04:05Z", seoul)
		if !ok {
			t.Fatal("did not parse")
		}
		if s := got.UTC().Format(time.RFC3339); s != "2024-01-02T15:04:05Z" {
			t.Errorf("UTC = %s, want 2024-01-02T15:04:05Z", s)
		}
	})
}

// Whatever this driver writes must be readable by its own first layout.
func TestTimeRoundTrip(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	instants := []time.Time{
		time.Date(2024, 1, 2, 15, 4, 5, 123456789, time.UTC),
		time.Date(2024, 1, 2, 15, 4, 5, 0, seoul),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	for _, format := range []TimeFormat{TimeFormatOffset, TimeFormatUTC} {
		for _, loc := range []*time.Location{time.UTC, seoul} {
			cfg := &Config{Loc: loc, TimeFormat: format}
			for _, want := range instants {
				s := formatTimeString(want, format, loc)
				got, ok := parseTimeString(s, loc)
				if !ok {
					t.Errorf("format %v loc %v: %q did not parse back", format, loc, s)
					continue
				}
				if !got.Equal(want) {
					t.Errorf("format %v loc %v: %v -> %q -> %v", format, loc, want, s, got)
				}
				_ = cfg
			}
		}
	}

	// The datetime format drops sub-second precision by design.
	t.Run("datetime truncates to the second", func(t *testing.T) {
		want := time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)
		s := formatTimeString(want, TimeFormatDatetime, time.UTC)
		if s != "2024-01-02T15:04:05" {
			t.Fatalf("got %q", s)
		}
		got, ok := parseTimeString(s, time.UTC)
		if !ok || !got.Equal(want) {
			t.Errorf("%q -> %v, %v", s, got, ok)
		}
	})

	// The offset layout carries whole minutes only, so a zone with a
	// sub-minute offset (LMT, before standard time was adopted) cannot round
	// trip through it. mattn/go-sqlite3 has the same limitation; the UTC
	// format does not.
	t.Run("sub-minute zone offsets need the UTC format", func(t *testing.T) {
		v := time.Date(1900, 6, 15, 12, 0, 0, 0, time.UTC)

		s := formatTimeString(v, TimeFormatOffset, seoul)
		got, ok := parseTimeString(s, seoul)
		if !ok {
			t.Fatalf("%q did not parse back", s)
		}
		if got.Equal(v) {
			t.Errorf("offset format unexpectedly round-tripped %v via %q; "+
				"update the documented limitation", v, s)
		}

		s = formatTimeString(v, TimeFormatUTC, seoul)
		got, ok = parseTimeString(s, seoul)
		if !ok {
			t.Fatalf("%q did not parse back", s)
		}
		if !got.Equal(v) {
			t.Errorf("UTC format: %v -> %q -> %v", v, s, got)
		}
	})

	// A UTC-format value must survive being read by a connection in any
	// location, which is why it carries an explicit Z.
	t.Run("the UTC format is self-describing", func(t *testing.T) {
		v := time.Date(2024, 1, 2, 15, 4, 5, 0, time.UTC)
		s := formatTimeString(v, TimeFormatUTC, time.UTC)
		if !strings.HasSuffix(s, "Z") {
			t.Fatalf("got %q, want a trailing Z", s)
		}
		for _, loc := range []*time.Location{time.UTC, seoul} {
			got, ok := parseTimeString(s, loc)
			if !ok || !got.Equal(v) {
				t.Errorf("loc %v: %q -> %v, %v", loc, s, got, ok)
			}
		}
	})

	t.Run("integer formats", func(t *testing.T) {
		v := time.Date(2024, 1, 2, 15, 4, 5, 123456789, time.UTC)
		for f, want := range map[IntegerTimeFormat]int64{
			IntegerTimeUnix:      v.Unix(),
			IntegerTimeUnixMilli: v.UnixMilli(),
			IntegerTimeUnixMicro: v.UnixMicro(),
			IntegerTimeUnixNano:  v.UnixNano(),
		} {
			if got := integerFromTime(v, f); got != want {
				t.Errorf("format %d: got %d, want %d", f, got, want)
			}
			back := timeFromInteger(want, f, time.UTC)
			if f == IntegerTimeUnixNano && !back.Equal(v) {
				t.Errorf("nano round trip: got %v, want %v", back, v)
			}
		}
	})
}

func TestEncodeValue(t *testing.T) {
	cfg := utcConfig()

	roundTrip := func(t *testing.T, v driver.Value, class declClass) driver.Value {
		t.Helper()
		w := wire.NewWriter(wire.OpQuery, 0, 1)
		if err := encodeValue(w, v, cfg); err != nil {
			t.Fatalf("encodeValue(%#v): %v", v, err)
		}
		_, r, err := wire.ReadHeader(w.Frame())
		if err != nil {
			t.Fatalf("ReadHeader: %v", err)
		}
		got := decodeValue(r, class, cfg)
		if err := r.Err(); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	t.Run("scalars", func(t *testing.T) {
		for _, v := range []driver.Value{
			nil,
			int64(math.MinInt64),
			int64(9007199254740993),
			float64(0.1),
			"héllo☃",
			[]byte{1, 2, 3},
		} {
			got := roundTrip(t, v, classOther)
			if !reflect.DeepEqual(got, v) {
				t.Errorf("%#v -> %#v", v, got)
			}
		}
	})

	t.Run("bool becomes INTEGER", func(t *testing.T) {
		if got := roundTrip(t, true, classOther); got != driver.Value(int64(1)) {
			t.Errorf("true -> %#v, want int64(1)", got)
		}
		if got := roundTrip(t, false, classBool); got != driver.Value(false) {
			t.Errorf("false -> %#v via a BOOLEAN column, want false", got)
		}
	})

	// mattn parity: a nil []byte is NULL, an empty one is a zero-length blob.
	t.Run("nil and empty byte slices", func(t *testing.T) {
		if got := roundTrip(t, []byte(nil), classOther); got != nil {
			t.Errorf("nil []byte -> %#v, want nil", got)
		}
		got := roundTrip(t, []byte{}, classOther)
		b, ok := got.([]byte)
		if !ok || b == nil || len(b) != 0 {
			t.Errorf("empty []byte -> %#v, want an empty non-nil slice", got)
		}
	})

	t.Run("time", func(t *testing.T) {
		want := time.Date(2024, 1, 2, 15, 4, 5, 123456789, time.UTC)
		got := roundTrip(t, want, classTime)
		if !got.(time.Time).Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("an unbindable type is an error, not a panic", func(t *testing.T) {
		w := wire.NewWriter(wire.OpQuery, 0, 1)
		if err := encodeValue(w, struct{}{}, cfg); err == nil {
			t.Error("expected an error")
		}
	})
}

func TestNormalizeAndClassifyDeclType(t *testing.T) {
	for in, want := range map[string]string{
		"":                 "",
		"integer":          "INTEGER",
		"  TEXT  ":         "TEXT",
		"VARCHAR(255)":     "VARCHAR",
		"DECIMAL(10, 2)":   "DECIMAL",
		"datetime":         "DATETIME",
		"NATIVE CHARACTER": "NATIVE CHARACTER",
	} {
		if got := normalizeDeclType(in); got != want {
			t.Errorf("normalizeDeclType(%q) = %q, want %q", in, got, want)
		}
	}

	for in, want := range map[string]declClass{
		"DATE":      classTime,
		"DATETIME":  classTime,
		"TIMESTAMP": classTime,
		"BOOLEAN":   classBool,
		// TIME would turn SQLite's "HH:MM:SS" into a failed parse.
		"TIME": classOther,
		// BOOL yields int64 in mattn, so widening would break migrations.
		"BOOL":    classOther,
		"INTEGER": classOther,
		"":        classOther,
	} {
		if got := classifyDeclType(in); got != want {
			t.Errorf("classifyDeclType(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestScanTypeForDeclType(t *testing.T) {
	for in, want := range map[string]reflect.Type{
		"INTEGER":   typeNullInt64,
		"BIGINT":    typeNullInt64,
		"INT8":      typeNullInt64,
		"TEXT":      typeNullString,
		"CLOB":      typeNullString,
		"VARCHAR":   typeNullString,
		"BLOB":      typeRawBytes,
		"REAL":      typeNullFloat64,
		"DOUBLE":    typeNullFloat64,
		"FLOAT":     typeNullFloat64,
		"NUMERIC":   typeNullFloat64,
		"DECIMAL":   typeNullFloat64,
		"DATE":      typeNullTime,
		"DATETIME":  typeNullTime,
		"TIMESTAMP": typeNullTime,
		"BOOLEAN":   typeNullBool,
		"":          typeAny,
		"GEOMETRY":  typeAny,
	} {
		if got := scanTypeForDeclType(in); got != want {
			t.Errorf("scanTypeForDeclType(%q) = %v, want %v", in, got, want)
		}
	}

	// Callers are entitled to a non-nil type; modernc returns nil here and
	// crashes ColumnType.ScanType().
	for _, in := range []string{"", "WHATEVER", "INTEGER", "BLOB"} {
		if scanTypeForDeclType(in) == nil {
			t.Errorf("scanTypeForDeclType(%q) returned nil", in)
		}
	}

	// Sanity: these are the types database/sql documents for ColumnType.
	if typeNullTime != reflect.TypeOf(sql.NullTime{}) {
		t.Error("NullTime mismatch")
	}
}
