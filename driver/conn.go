//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/lesomnus/sqlite3-wasm/binding"
	"github.com/lesomnus/sqlite3-wasm/binding/wire"
)

// Conn is one sqlite3 handle on the connector's worker.
type Conn struct {
	db  *binding.DB
	cfg *Config

	// inTx tracks explicit transactions so a connection returned to the pool
	// while one is open gets rolled back rather than silently enrolling the
	// next caller's statements.
	inTx bool

	// poisoned marks a connection whose request was abandoned — a cancelled
	// context on a build that cannot interrupt. The worker may still be
	// producing for it, so the pool must discard rather than reuse it.
	poisoned bool
}

var (
	_ driver.Conn               = (*Conn)(nil)
	_ driver.ConnBeginTx        = (*Conn)(nil)
	_ driver.ConnPrepareContext = (*Conn)(nil)
	_ driver.ExecerContext      = (*Conn)(nil)
	_ driver.QueryerContext     = (*Conn)(nil)
	_ driver.Pinger             = (*Conn)(nil)
	_ driver.SessionResetter    = (*Conn)(nil)
	_ driver.Validator          = (*Conn)(nil)
	_ driver.NamedValueChecker  = (*Conn)(nil)
)

func (c *Conn) Close() error {
	return c.db.Close(context.Background())
}

func (c *Conn) Ping(ctx context.Context) error {
	_, err := c.db.Exec(ctx, "SELECT 1", nil)
	return err
}

// IsValid reports whether the connection may be reused. A poisoned connection
// or a dead worker means no.
func (c *Conn) IsValid() bool {
	return !c.poisoned && c.db.Worker().Err() == nil
}

// ResetSession runs before a pooled connection is handed out again.
func (c *Conn) ResetSession(ctx context.Context) error {
	if c.poisoned || c.db.Worker().Err() != nil {
		// Discards the connection without re-running anything, which returning
		// ErrBadConn from an operation would not.
		return driver.ErrBadConn
	}
	if c.inTx {
		// A leaked transaction would hold a write lock and enrol the next
		// caller's statements.
		c.inTx = false
		if _, err := c.db.Exec(ctx, "ROLLBACK", nil); err != nil {
			return driver.ErrBadConn
		}
	}
	return nil
}

// CheckNamedValue widens what the driver accepts beyond database/sql's default
// converter, which rejects a uint64 with the high bit set outright.
func (c *Conn) CheckNamedValue(nv *driver.NamedValue) error {
	switch v := nv.Value.(type) {
	case nil, bool, int64, float64, string, []byte, time.Time:
		return nil
	case uint64:
		if v > math.MaxInt64 {
			return fmt.Errorf("sqlite3-wasm: uint64 %d does not fit in SQLite's 64-bit signed INTEGER", v)
		}
		nv.Value = int64(v)
		return nil
	}
	var err error
	nv.Value, err = driver.DefaultParameterConverter.ConvertValue(nv.Value)
	return err
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

func (c *Conn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	s, err := c.db.Prepare(ctx, query)
	if err != nil {
		return nil, c.classify(ctx, err)
	}
	return &Stmt{c: c, s: s, query: query}, nil
}

// ExecContext runs the query as a script. Implementing it is not an
// optimisation: without it database/sql falls back to one prepare plus one
// exec, and sqlite3_prepare compiles only the first statement, so everything
// after the first semicolon would be silently dropped.
//
// It is deliberately not retried on SQLITE_BUSY: a script whose third statement
// is busy has already run the first two, and re-running them would duplicate
// their effects.
func (c *Conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	w, err := argsWriter(c.cfg, args)
	if err != nil {
		return nil, err
	}
	res, err := c.db.Exec(ctx, query, w)
	if err != nil {
		return nil, c.classify(ctx, err)
	}
	return newResult(res), nil
}

func (c *Conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	w, err := argsWriter(c.cfg, args)
	if err != nil {
		return nil, err
	}
	return retry(ctx, func() (driver.Rows, error) {
		rows, err := c.db.Query(ctx, query, w)
		if err != nil {
			return nil, c.classify(ctx, err)
		}
		return newRows(ctx, c, rows), nil
	})
}

func (c *Conn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *Conn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.Isolation != driver.IsolationLevel(0) {
		return nil, fmt.Errorf(
			"sqlite3-wasm: isolation level %d is not supported; SQLite offers serialisable transactions only",
			opts.Isolation)
	}

	begin := c.cfg.txBeginStatement()
	if opts.ReadOnly {
		// A read-only transaction takes no write lock, and query_only makes the
		// promise enforceable rather than advisory.
		begin = "BEGIN DEFERRED"
		if _, err := c.db.Exec(ctx, "PRAGMA query_only = ON", nil); err != nil {
			return nil, c.classify(ctx, err)
		}
	}
	if _, err := c.db.Exec(ctx, begin, nil); err != nil {
		if opts.ReadOnly {
			_, _ = c.db.Exec(context.WithoutCancel(ctx), "PRAGMA query_only = OFF", nil)
		}
		return nil, c.classify(ctx, err)
	}
	c.inTx = true
	return &Tx{c: c, readOnly: opts.ReadOnly}, nil
}

// classify turns a transport failure into the error database/sql should see.
//
// A cancelled context reports ctx.Err(), never driver.ErrBadConn: ErrBadConn
// makes database/sql retry the statement on a fresh connection up to three
// times, and a cancelled statement may already have run.
func (c *Conn) classify(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		if !c.db.Cancellable() {
			// The request was abandoned rather than interrupted; the worker may
			// still be producing for it.
			c.poisoned = true
		}
		return ctxErr
	}
	if werr := c.db.Worker().Err(); werr != nil {
		c.poisoned = true
	}
	return err
}

// argsWriter validates every argument up front, so encoding cannot fail
// half-way through a frame, and returns the closure that writes them.
func argsWriter(cfg *Config, args []driver.NamedValue) (func(*wire.Writer), error) {
	for _, a := range args {
		if err := encodableValue(a.Value); err != nil {
			return nil, err
		}
	}
	return func(m *wire.Writer) {
		m.U32(uint32(len(args)))
		for _, a := range args {
			m.NullString(a.Name, a.Name != "")
			m.U32(uint32(a.Ordinal))
			// Cannot fail: every value was checked above.
			_ = encodeValue(m, a.Value, cfg)
		}
	}, nil
}

func encodableValue(v driver.Value) error {
	switch v.(type) {
	case nil, int64, bool, float64, string, []byte, time.Time:
		return nil
	}
	return fmt.Errorf("sqlite3-wasm: cannot bind %T", v)
}

// retryAttempts bounds the SQLITE_BUSY retry loop.
const retryAttempts = 8

// retry re-runs a single-statement operation that lost a lock race.
//
// It is only ever applied where re-running is harmless. SQLITE_BUSY means the
// statement did not execute, so a read or a lone statement can simply be tried
// again — but the wait has to happen here, on the Go side, because SQLite's own
// busy handler sleeps with Atomics.wait on the worker's thread and would block
// the very message that releases the lock.
func retry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	var zero T
	backoff := 200 * time.Microsecond
	for attempt := 0; ; attempt++ {
		v, err := fn()
		if err == nil || !binding.IsBusy(err) || attempt == retryAttempts-1 {
			return v, err
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return zero, ctx.Err()
		}
		backoff *= 2
	}
}

var errClosedConn = errors.New("sqlite3-wasm: connection is closed")
