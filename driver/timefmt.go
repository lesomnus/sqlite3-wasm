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
// Index 0 is also the default write format, so a value written by this driver
// round-trips through the first layout tried.
var timeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

const (
	layoutOffset = "2006-01-02 15:04:05.999999999-07:00"
	// The trailing Z is a literal, and it is load-bearing. Without it the
	// value would be naive, and a naive value is read back in the *reader's*
	// location — so writing UTC and reading with _loc=Asia/Seoul would shift
	// by nine hours. With it the value is self-describing, still sorts
	// lexicographically across zones, and SQLite's own date functions accept
	// it (verified: datetime('2024-01-02 15:04:05.123Z') works).
	layoutUTC      = "2006-01-02 15:04:05.999999999Z"
	layoutDatetime = "2006-01-02 15:04:05"
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
// TimeFormatOffset is the default because it is byte-for-byte what
// mattn/go-sqlite3 writes, so a database file shared between a Go server and a
// browser reads the same either way. It has one limitation: the "-07:00" layout
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
