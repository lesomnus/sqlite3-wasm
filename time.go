package sqlitewasm

import (
	"fmt"
	"time"

	"github.com/lesomnus/sqlite3-wasm/driver"
)

// Time scans a timestamp from a column whose declared type SQLite does not
// report.
//
// The driver converts DATE, DATETIME and TIMESTAMP columns to time.Time
// automatically, but sqlite3_column_decltype is null for expressions and
// aggregates — `SELECT MAX(created_at)` has no declared type at all. Rather
// than guess-parse every string that looks like a date, which would silently
// turn a user named "2024-01-02" into a timestamp, conversion is opt-in per
// destination:
//
//	var t sqlitewasm.Time
//	err := db.QueryRow("SELECT MAX(created_at) FROM events").Scan(&t)
//
// Loc selects the location naive timestamps are read in; nil means UTC.
type Time struct {
	time.Time
	Loc *time.Location
}

// Scan implements sql.Scanner.
func (t *Time) Scan(src any) error {
	loc := t.Loc
	if loc == nil {
		loc = time.UTC
	}
	switch v := src.(type) {
	case nil:
		t.Time = time.Time{}
		return nil
	case time.Time:
		t.Time = v.In(loc)
		return nil
	case string:
		parsed, ok := driver.ParseTime(v, loc)
		if !ok {
			return fmt.Errorf("sqlite3-wasm: cannot parse %q as a time", v)
		}
		t.Time = parsed
		return nil
	case []byte:
		return t.Scan(string(v))
	case int64:
		t.Time = driver.TimeFromUnix(v, loc)
		return nil
	}
	return fmt.Errorf("sqlite3-wasm: cannot scan %T into a Time", src)
}

// NullTime is Time that tolerates NULL.
type NullTime struct {
	Time  time.Time
	Valid bool
	Loc   *time.Location
}

// Scan implements sql.Scanner.
func (t *NullTime) Scan(src any) error {
	if src == nil {
		t.Time, t.Valid = time.Time{}, false
		return nil
	}
	inner := Time{Loc: t.Loc}
	if err := inner.Scan(src); err != nil {
		return err
	}
	t.Time, t.Valid = inner.Time, true
	return nil
}
