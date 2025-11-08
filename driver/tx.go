//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"
	"time"
)

type Tx struct {
	c Conn
}

var _ driver.Tx = Tx{}

func (x Tx) Commit() error {
	// Send COMMIT to the worker. Use a short timeout to avoid blocking
	// indefinitely if the promiser is unresponsive.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := x.c.ExecContext(ctx, "COMMIT", nil)
	return err
}

func (x Tx) Rollback() error {
	// Send ROLLBACK to the worker.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := x.c.ExecContext(ctx, "ROLLBACK", nil)
	return err
}
