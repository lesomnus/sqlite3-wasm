package driver

import (
	"database/sql"
	"reflect"
	"strings"
)

// declClass is what a column's declared type means for conversion.
type declClass uint8

const (
	classOther declClass = iota
	classTime
	classBool
)

// normalizeDeclType folds a declared type to the single form used for both
// value conversion and ColumnTypeScanType: upper case, trimmed, and cut at the
// first '(' so VARCHAR(255) and DECIMAL(10,2) classify like VARCHAR and
// DECIMAL. Doing it once avoids the lower/upper asymmetry mattn/go-sqlite3 has
// between its two paths.
func normalizeDeclType(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return strings.ToUpper(s)
}

// classifyDeclType maps a normalized declared type to its conversion class.
//
// The time class matches mattn/go-sqlite3 exactly. TIME is deliberately absent:
// SQLite's time() emits "HH:MM:SS", which no layout parses, so including it
// would turn a usable string into a failed parse.
//
// The bool class is only BOOLEAN, also for mattn parity. BOOL is common in the
// wild but yields int64 there, so widening the class would silently break
// Scan(&myInt) for anyone migrating.
func classifyDeclType(norm string) declClass {
	switch norm {
	case "DATE", "DATETIME", "TIMESTAMP":
		return classTime
	case "BOOLEAN":
		return classBool
	}
	return classOther
}

var (
	typeNullInt64   = reflect.TypeOf(sql.NullInt64{})
	typeNullString  = reflect.TypeOf(sql.NullString{})
	typeRawBytes    = reflect.TypeOf(sql.RawBytes{})
	typeNullFloat64 = reflect.TypeOf(sql.NullFloat64{})
	typeNullTime    = reflect.TypeOf(sql.NullTime{})
	typeNullBool    = reflect.TypeOf(sql.NullBool{})
	typeAny         = reflect.TypeOf((*any)(nil)).Elem()
)

// scanTypeForDeclType implements driver.RowsColumnTypeScanType.
//
// It is derived from the declared type alone, never from a value: ColumnTypes()
// may be called before the first Next(), when sqlite3_column_type still reports
// SQLITE_NULL. It never returns nil, which callers are entitled to assume.
//
// Note this is purely informational — database/sql reads it only for
// ColumnType.ScanType() and it has no effect on Scan.
func scanTypeForDeclType(norm string) reflect.Type {
	switch {
	case norm == "":
		return typeAny
	case strings.Contains(norm, "INT"):
		return typeNullInt64
	case norm == "CLOB", norm == "TEXT", strings.Contains(norm, "CHAR"):
		return typeNullString
	case norm == "BLOB":
		return typeRawBytes
	case norm == "REAL", norm == "FLOAT", strings.Contains(norm, "DOUBLE"):
		return typeNullFloat64
	case classifyDeclType(norm) == classTime:
		return typeNullTime
	case norm == "NUMERIC", strings.Contains(norm, "DECIMAL"):
		return typeNullFloat64
	case classifyDeclType(norm) == classBool:
		return typeNullBool
	}
	return typeAny
}
