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
	t.c.inTx = false
	if t.readOnly {
		if _, e := t.c.db.Exec(ctx, "PRAGMA query_only = OFF", nil); err == nil {
			err = e
		}
	}
	return err
}
