package driver

import (
	"database/sql/driver"
	"fmt"
	"time"

	"github.com/lesomnus/sqlite3-wasm/binding/wire"
)

// decodeValue reads one encoded value and converts it to a driver.Value,
// keyed on the column's declared-type class.
//
// It reads straight from the batch buffer and is called once per cell from
// Rows.Next, so it deliberately avoids materialising anything it does not have
// to. The []byte it returns for a BLOB aliases the batch: database/sql clones
// on Scan into *[]byte, and sql.RawBytes is documented as valid only until the
// next Next, which is exactly this lifetime.
func decodeValue(r *wire.Reader, class declClass, cfg *Config) driver.Value {
	switch r.Tag() {
	case wire.TagNull:
		return nil

	case wire.TagInt:
		v := r.I64()
		switch class {
		case classTime:
			return timeFromInteger(v, cfg.IntegerTime, cfg.Loc)
		case classBool:
			// != 0, not > 0: SQLite stores -1 for some boolean expressions and
			// mattn's > 0 reads that as false.
			return v != 0
		}
		return v

	case wire.TagReal:
		// No time conversion for REAL, matching both incumbents: a Julian day
		// number is indistinguishable from an ordinary float, and guessing
		// would corrupt real data.
		return r.F64()

	case wire.TagText:
		b := r.Bytes()
		if class == classTime {
			if t, ok := parseTimeString(string(b), cfg.Loc); ok {
				return t
			}
			// Fall through to the raw string rather than the zero time.
		}
		return string(b)

	case wire.TagBlob:
		b := r.Bytes()
		if b == nil {
			// A zero-length blob must not become a nil []byte: Scan(&any)
			// would hand the caller a typed nil.
			return []byte{}
		}
		return b
	}
	return nil
}

// encodeValue writes a driver.Value as a bind argument.
//
// The input set is whatever survives database/sql's default converter plus
// whatever Conn.CheckNamedValue lets through, so anything outside this switch
// is a driver bug rather than user error.
func encodeValue(w *wire.Writer, v driver.Value, cfg *Config) error {
	switch t := v.(type) {
	case nil:
		w.ValueNull()
	case int64:
		w.ValueInt(t)
	case bool:
		if t {
			w.ValueInt(1)
		} else {
			w.ValueInt(0)
		}
	case float64:
		w.ValueReal(t)
	case string:
		w.ValueText(t)
	case []byte:
		// A nil []byte is NULL; an empty non-nil one is a zero-length blob.
		if t == nil {
			w.ValueNull()
		} else {
			w.ValueBlob(t)
		}
	case time.Time:
		if cfg.IntegerTime != IntegerTimeNone {
			w.ValueInt(integerFromTime(t, cfg.IntegerTime))
		} else {
			w.ValueText(formatTimeString(t, cfg.TimeFormat, cfg.Loc))
		}
	default:
		return fmt.Errorf("sqlite3-wasm: cannot bind %T", v)
	}
	return nil
}
