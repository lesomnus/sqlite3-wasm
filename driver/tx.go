//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"
)

// Tx is an explicit transaction.
type Tx struct {
	c        *Conn
	readOnly bool
}

var _ driver.Tx = (*Tx)(nil)

func (t *Tx) Commit() error   { return t.finish("COMMIT") }
func (t *Tx) Rollback() error { return t.finish("ROLLBACK") }

func (t *Tx) finish(stmt string) error {
	// database/sql may call this from its context watcher, so the statement
	// must not inherit a context that is already cancelled.
	ctx := context.Background()
	_, err := t.c.db.Exec(ctx, stmt, nil)

	// Only a COMMIT that succeeded actually ended the transaction. SQLite
	// keeps it open when COMMIT fails — a deferred foreign-key violation or
	// SQLITE_BUSY — precisely so the caller can still roll back. database/sql
	// returns the connection to the pool on any non-ErrBadConn error from
	// Commit, so clearing the flag here would hand the next caller a
	// connection sitting inside an open write transaction, with the
	// uncommitted rows visible to it.
	if err == nil {
		t.c.inTx = false
	}

	if t.readOnly {
		if _, e := t.c.db.Exec(ctx, "PRAGMA query_only = OFF", nil); err == nil {
			err = e
		}
	}
	return err
}
