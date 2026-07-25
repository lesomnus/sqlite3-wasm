//go:build !(js && wasm)

// Package sqlitewasm registers a database/sql driver that runs SQLite in a
// dedicated Web Worker. See the js/wasm build for the real implementation.
package sqlitewasm

import (
	"database/sql"
	dsql "database/sql/driver"
	"errors"
)

// DriverName is the name this package registers with database/sql.
const DriverName = "sqlite3-wasm"

// ErrWrongPlatform is what every operation returns off js/wasm.
//
// Registering the name anyway matters: without it a program built for the wrong
// platform fails with database/sql's "unknown driver" and no hint about why.
var ErrWrongPlatform = errors.New("sqlite3-wasm: requires GOOS=js GOARCH=wasm")

type stubDriver struct{}

func (stubDriver) Open(string) (dsql.Conn, error) { return nil, ErrWrongPlatform }

func init() {
	sql.Register(DriverName, stubDriver{})
}

// OpenDB reports ErrWrongPlatform off js/wasm.
func OpenDB(string) (*sql.DB, error) { return nil, ErrWrongPlatform }
