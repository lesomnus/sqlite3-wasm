//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

type Conn struct {
	p *binding.Promiser
}

var (
	_ driver.Conn               = Conn{}
	_ driver.ConnBeginTx        = Conn{}
	_ driver.ConnPrepareContext = Conn{}
	_ driver.Execer             = Conn{}
	_ driver.ExecerContext      = Conn{}
)

func (c Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c Conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	// The worker Promiser API does not provide a server-side prepare/statement
	// handle. Implement a simple client-side prepared statement shim that
	// stores the SQL text and sends it to the worker on Exec/Query.
	return &Stmt{c, query}, nil
}

func (c Conn) Close() error {
	return c.p.Close()
}

func (c Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	// Start a transaction by sending a BEGIN statement to the worker.
	// We ignore TxOptions for now (isolation level / read-only) as the
	// worker API doesn't expose a transaction API beyond SQL commands.
	if _, err := c.ExecContext(ctx, "BEGIN", nil); err != nil {
		return nil, err
	}
	return Tx{c: c}, nil
}

func (c Conn) Exec(query string, args []driver.Value) (driver.Result, error) {
	return c.ExecContext(context.Background(), query, namedValues(args))
}

func (c Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	// Perform a simple client-side parameter substitution (shim) before
	// sending the SQL to the promiser.
	final, err := substituteParams(query, args)
	if err != nil {
		return Result{0, 0}, err
	}
	ch := c.p.Exec(ctx, final)

	for rr := range ch {
		if rr.Error != nil {
			return Result{0, 0}, rr.Error
		}
		if rr.RowNumber == 0 {
			break
		}
		// ignore row data for Exec
	}

	return Result{0, 0}, nil
}
