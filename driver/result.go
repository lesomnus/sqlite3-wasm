//go:build js && wasm

package driver

import (
	dsql "database/sql/driver"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

// Result carries what a statement changed.
//
// The values are captured at execution time rather than fetched lazily:
// database/sql calls LastInsertId and RowsAffected at an arbitrary later point,
// under the connection lock, by which time sqlite3_changes would describe some
// other statement.
type Result struct {
	rowsAffected int64
	lastInsertID int64
}

var _ dsql.Result = Result{}

func newResult(r binding.Result) Result {
	return Result{rowsAffected: r.RowsAffected, lastInsertID: r.LastInsertID}
}

// LastInsertId never fails. Returning an error here is legal, but many ORMs
// call it unconditionally, so DDL reports 0 rather than refusing.
func (r Result) LastInsertId() (int64, error) { return r.lastInsertID, nil }

func (r Result) RowsAffected() (int64, error) { return r.rowsAffected, nil }
