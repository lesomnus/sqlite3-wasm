//go:build js && wasm

package sqlitewasm

import (
	"database/sql"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

// OpenDB opens a database and configures the pool for how SQLite actually
// behaves here.
//
// It sets SetMaxOpenConns(1). More than one connection buys no parallelism —
// there is a single JavaScript thread behind them all — while it does enable
// SQLITE_BUSY, because this build has no WAL (neither OPFS VFS implements
// xShmMap) and no shared cache. For a file-backed database a second connection
// is worse still: it contends for the same exclusive OPFS access handle.
//
// Use sql.Open directly if you want to choose the pool settings yourself.
func OpenDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Error is a SQLite error, carrying the primary and extended result codes, the
// message, and the byte offset in the SQL when there is one.
type Error = binding.Error

// Sentinels for errors.Is. Only the primary result code is compared, so a
// sentinel does not have to know which extended code SQLite chose:
//
//	if errors.Is(err, sqlitewasm.ErrConstraint) { ... }
var (
	ErrBusy       = binding.ErrBusy
	ErrLocked     = binding.ErrLocked
	ErrConstraint = binding.ErrConstraint
	ErrInterrupt  = binding.ErrInterrupt
)
