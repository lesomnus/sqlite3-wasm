//go:build js && wasm

package driver

import "database/sql/driver"

type Result struct {
	rowsAffected int64
	lastInsertID int64
}

var _ driver.Result = &Result{}

func (r Result) LastInsertId() (int64, error) {
	return r.lastInsertID, nil
}

func (r Result) RowsAffected() (int64, error) {
	return r.rowsAffected, nil
}
