package driver

import (
	"strings"
	"time"
)

// timeLayouts are tried in order when a TEXT value lands in a DATE, DATETIME or
// TIMESTAMP column.
//
// This is mattn/go-sqlite3's list minus two entries that can never match:
// "2006-01-02 15:04:05" and its 'T' form are shadowed by the fractional-second
// layouts above them, because .999999999 is optional.
//
// Both separators are read and always were, so a database written by any
// version of this driver — or by mattn, or by anything that writes RFC 3339 —
// is read the same. `.999999999` accepts any number of fractional digits
// including none, so what is written below round-trips through the first
// layout tried even though it is written at a fixed width.
var timeLayouts = []string{
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

// The separator is 'T', which is RFC 3339's and what the rest of the world
// writes: `time.RFC3339Nano`, `ncruces/go-sqlite3`, every JSON timestamp.
//
// It used to be a space, byte-for-byte what mattn/go-sqlite3 writes, so that a
// file shared with a Go server read the same either way. Reading was never the
// problem — both separators are in the list above. **Comparing** was: SQLite
// compares TEXT by bytes, and 'T' is 0x54 where a space is 0x20, so a row
// written with one separator and a bound argument written with the other are
// ordered by their separators rather than by their instants. A keyset cursor
// over such a column stops advancing: every row is "after" the cursor, so the
// same page comes back for ever.
//
// That is what happened. A sandbox seeded from a database `ncruces` had
// written, then read here, paged the same fifty rows until the tab ran out of
// memory — and nothing else was wrong, so nothing else said so.
//
// Sharing a file with mattn is the thing given up, and it is the lesser of the
// two: mattn reads this form, and what it loses is byte-identical output.
// # And the fraction is nine digits, always
//
// `.999999999` drops trailing zeros, which is what `time.RFC3339Nano` does and
// what this wrote until it was measured. It breaks the same ordering the
// separator does, and less visibly: `.1Z` and `.15Z` are a tenth and fifteen
// hundredths, and byte order puts the tenth *after*, because 'Z' is 0x5A where
// '5' is 0x35. Every pair whose fractional parts differ in length is a pair
// SQLite orders by their lengths.
//
// A keyset cursor lands on one of those pairs at a page boundary and hands back
// a row it already gave. Measured on a list of 201: four came twice.
//
// `.000000000` is nine digits whatever the instant, so every value is the same
// width and byte order is time order again. Reading is unaffected -- the parse
// layouts above take any width.
const (
	layoutOffset = "2006-01-02T15:04:05.000000000-07:00"
	// The trailing Z is a literal, and it is load-bearing. Without it the
	// value would be naive, and a naive value is read back in the *reader's*
	// location — so writing UTC and reading with _loc=Asia/Seoul would shift
	// by nine hours. With it the value is self-describing, still sorts
	// lexicographically across zones, and SQLite's own date functions accept
	// it (verified: datetime('2024-01-02T15:04:05.123Z') works).
	layoutUTC      = "2006-01-02T15:04:05.000000000Z"
	layoutDatetime = "2006-01-02T15:04:05"
)

// parseTimeString parses a TEXT timestamp, reporting whether any layout matched.
//
// A caller that gets false must keep the original string rather than
// substituting the zero time: a silent 0001-01-01 is unrecoverable, while a
// string surfaces as a debuggable "unsupported Scan" at the call site.
func parseTimeString(s string, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}

	// Go's "-07:00" does not accept a literal Z (only "Z07:00" does), so a
	// trailing Z is stripped and the value read as UTC.
	ts, hadZ := strings.CutSuffix(s, "Z")
	in := loc
	if hadZ {
		in = time.UTC
	}

	for _, layout := range timeLayouts {
		// ParseInLocation, not Parse: a naive timestamp should keep its wall
		// clock and be interpreted in the connection's location. An explicit
		// offset in the value still wins.
		if t, err := time.ParseInLocation(layout, ts, in); err == nil {
			return t.In(loc), true
		}
	}
	return time.Time{}, false
}

// timeFromInteger converts an INTEGER in a time column.
//
// With no explicit _time_integer_format this uses mattn's heuristic: a
// magnitude above 1e12 is too large to be seconds, so it is read as
// milliseconds. The heuristic misreads millisecond timestamps from before
// 2001-09-09, which is why the explicit form exists.
func timeFromInteger(v int64, f IntegerTimeFormat, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	var t time.Time
	switch f {
	case IntegerTimeUnix:
		t = time.Unix(v, 0)
	case IntegerTimeUnixMilli:
		t = time.UnixMilli(v)
	case IntegerTimeUnixMicro:
		t = time.UnixMicro(v)
	case IntegerTimeUnixNano:
		t = time.Unix(0, v)
	default:
		if v > 1e12 || v < -1e12 {
			t = time.UnixMilli(v)
		} else {
			t = time.Unix(v, 0)
		}
	}
	return t.UTC().In(loc)
}

// formatTimeString renders a time.Time for a TEXT column.
//
// TimeFormatOffset is the default because it is what a naive reader expects of
// a timestamp column and what SQLite's own datetime() accepts. It has one
// limitation: the "-07:00" layout
// carries only whole minutes, so a zone whose offset has seconds — LMT before
// standard time was adopted, e.g. Asia/Seoul before 1912 — does not round-trip
// exactly. Use TimeFormatUTC when that matters; it also sorts correctly across
// zones, which the offset form does not.
func formatTimeString(t time.Time, f TimeFormat, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	switch f {
	case TimeFormatUTC:
		return t.UTC().Format(layoutUTC)
	case TimeFormatDatetime:
		return t.In(loc).Format(layoutDatetime)
	default:
		return t.In(loc).Format(layoutOffset)
	}
}

// integerFromTime renders a time.Time for an INTEGER column.
func integerFromTime(t time.Time, f IntegerTimeFormat) int64 {
	switch f {
	case IntegerTimeUnixMilli:
		return t.UnixMilli()
	case IntegerTimeUnixMicro:
		return t.UnixMicro()
	case IntegerTimeUnixNano:
		return t.UnixNano()
	default:
		return t.Unix()
	}
}

// ParseTime parses a TEXT timestamp with the driver's layouts, reporting
// whether any of them matched. It is exported for sqlitewasm.Time, which lets a
// caller opt in to time conversion for a column that has no declared type.
func ParseTime(s string, loc *time.Location) (time.Time, bool) {
	return parseTimeString(s, loc)
}

// TimeFromUnix converts an integer timestamp using the same heuristic the
// driver applies to a DATE, DATETIME or TIMESTAMP column.
func TimeFromUnix(v int64, loc *time.Location) time.Time {
	return timeFromInteger(v, IntegerTimeNone, loc)
}
