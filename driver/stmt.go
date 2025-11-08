//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

// Stmt is a thin client-side prepared statement shim. The underlying
// sqlite wasm worker Promiser does not offer a prepare/statement handle
// in the Worker API used here, so we store the SQL text and simply send
// it to the worker on Exec/Query. This sacrifices performance and true
// parameter binding but provides basic functionality while the worker
// API is limited.
type Stmt struct {
	c Conn
	q string
}

var (
	_ driver.Stmt             = &Stmt{}
	_ driver.StmtExecContext  = &Stmt{}
	_ driver.StmtQueryContext = &Stmt{}
)

func (s Stmt) Close() error {
	return nil
}

// NumInput returns -1 because we don't parse or bind placeholders here.
func (s Stmt) NumInput() int { return -1 }

func (s Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

func (s Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	// Perform simple client-side parameter substitution and send SQL to worker.
	final, err := substituteParams(s.q, args)
	if err != nil {
		return nil, err
	}
	ch := s.c.p.Exec(ctx, final)

	// Consume results until the terminal RowResult (RowNumber == 0) or an error.
	for rr := range ch {
		if rr.Error != nil {
			return nil, rr.Error
		}
		if rr.RowNumber == 0 {
			break
		}
		// ignore row data for Exec
	}

	// We don't have reliable last-insert-id or rows-affected info from the
	// worker here, so return zeros. This can be improved later when the
	// worker API exposes that information.
	return Result{0, 0}, nil
}

func (s Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

func (s Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	// Perform simple client-side parameter substitution and send SQL to worker.
	final, err := substituteParams(s.q, args)
	if err != nil {
		return nil, err
	}
	ch := s.c.p.Exec(ctx, final)

	// Read the first RowResult to initialize columns / first row.
	var first binding.RowResult
	select {
	case rr, ok := <-ch:
		if !ok {
			return &Rows{cols: nil, ch: ch, closed: true}, nil
		} else {
			first = rr
		}
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if first.Error != nil {
		return nil, first.Error
	}

	r := &Rows{ch: ch}
	if len(first.ColumnNames) > 0 {
		r.cols = first.ColumnNames
	}

	if first.RowNumber == 0 {
		// No rows; return rows object which will immediately EOF on Next.
		r.closed = true
		return r, nil
	}

	r.cur = first.Row
	return r, nil
}
