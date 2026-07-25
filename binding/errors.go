package binding

import (
	"errors"
	"fmt"

	"github.com/lesomnus/sqlite3-wasm/binding/wire"
)

// Primary SQLite result codes the driver reasons about by name.
const (
	SQLITE_OK         = 0
	SQLITE_ERROR      = 1
	SQLITE_BUSY       = 5
	SQLITE_LOCKED     = 6
	SQLITE_INTERRUPT  = 9
	SQLITE_SCHEMA     = 17
	SQLITE_CONSTRAINT = 19
)

// Error is a SQLite error carried across the wire.
//
// It keeps the extended result code as well as the primary one, so a caller can
// tell SQLITE_CONSTRAINT_UNIQUE from SQLITE_CONSTRAINT_FOREIGNKEY, and it keeps
// sqlite3_error_offset so a syntax error can point at the offending byte.
type Error struct {
	Code         int32
	ExtendedCode int32
	// Offset is the byte offset in the SQL that caused the error, or -1.
	Offset  int32
	Message string
}

func (e *Error) Error() string {
	if e.Offset >= 0 {
		return fmt.Sprintf("sqlite3: %s (code %d, at offset %d)", e.Message, e.ExtendedCode, e.Offset)
	}
	return fmt.Sprintf("sqlite3: %s (code %d)", e.Message, e.ExtendedCode)
}

// Is lets callers match on a primary result code with errors.Is:
//
//	errors.Is(err, &binding.Error{Code: binding.SQLITE_BUSY})
//
// Only the primary code is compared, so a sentinel does not have to know which
// extended code SQLite chose.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// Sentinels for the codes worth handling structurally.
var (
	ErrBusy       = &Error{Code: SQLITE_BUSY, Message: "database is locked"}
	ErrLocked     = &Error{Code: SQLITE_LOCKED, Message: "database table is locked"}
	ErrConstraint = &Error{Code: SQLITE_CONSTRAINT, Message: "constraint failed"}
	ErrInterrupt  = &Error{Code: SQLITE_INTERRUPT, Message: "interrupted"}
)

// IsBusy reports whether err is a SQLITE_BUSY or SQLITE_LOCKED, both of which
// mean "try again once the worker is free".
func IsBusy(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == SQLITE_BUSY || e.Code == SQLITE_LOCKED
}

func readError(r *wire.Reader) error {
	e := &Error{
		Code:         r.I32(),
		ExtendedCode: r.I32(),
		Offset:       r.I32(),
		Message:      r.String(),
	}
	if err := r.Err(); err != nil {
		return err
	}
	return e
}
