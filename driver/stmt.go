//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

// Stmt is a statement compiled on the worker.
type Stmt struct {
	c     *Conn
	s     *binding.Statement
	query string
}

var (
	_ driver.Stmt             = (*Stmt)(nil)
	_ driver.StmtExecContext  = (*Stmt)(nil)
	_ driver.StmtQueryContext = (*Stmt)(nil)
)

func (s *Stmt) Close() error {
	return s.c.db.Finalize(context.Background(), s.s.ID)
}

// NumInput reports the parameter count only when SQLite's answer can be trusted.
//
// sqlite3_bind_parameter_count returns the largest index used, so `SELECT ?5`
// reports 5 for a single parameter — and database/sql turns a wrong count into
// "sql: expected N arguments, got M" before the driver is ever called. The
// worker therefore only marks the count exact when every slot is an anonymous
// '?' and nothing follows the statement; otherwise this returns -1 and the
// arity is checked by SQLite itself, which can say something accurate.
func (s *Stmt) NumInput() int {
	if !s.s.ParamCountExact {
		return -1
	}
	return int(s.s.ParamCount)
}

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(args))
}

func (s *Stmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	w, err := argsWriter(s.c.cfg, args)
	if err != nil {
		return nil, err
	}
	// Safe to retry: a prepared statement is a single statement, and
	// SQLITE_BUSY means it did not run.
	return retry(ctx, func() (driver.Result, error) {
		res, err := s.c.db.ExecStatement(ctx, s.s.ID, w)
		if err != nil {
			return nil, s.c.classify(ctx, err)
		}
		return newResult(res), nil
	})
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(args))
}

func (s *Stmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	w, err := argsWriter(s.c.cfg, args)
	if err != nil {
		return nil, err
	}
	return retry(ctx, func() (driver.Rows, error) {
		rows, err := s.c.db.QueryStatement(ctx, s.s.ID, w)
		if err != nil {
			return nil, s.c.classify(ctx, err)
		}
		return newRows(ctx, s.c, rows), nil
	})
}

// namedValues adapts the deprecated positional entry points.
func namedValues(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return named
}
