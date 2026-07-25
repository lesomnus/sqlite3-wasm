//go:build js && wasm

// Package sqlitewasm registers a database/sql driver that runs SQLite in a
// dedicated Web Worker.
//
// It is only buildable for GOOS=js GOARCH=wasm; a stub for other platforms
// registers the same name with an Open that explains why.
package sqlitewasm

import (
	"database/sql"

	"github.com/lesomnus/sqlite3-wasm/driver"
)

// DriverName is the name this package registers with database/sql.
const DriverName = "sqlite3-wasm"

func init() {
	sql.Register(DriverName, driver.Driver{})
}
