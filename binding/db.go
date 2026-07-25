//go:build js && wasm

package binding

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/lesomnus/sqlite3-wasm/binding/wire"
)

// Column is a result column's name and declared type. DeclType is absent for
// expression columns.
type Column struct {
	Name     string
	DeclType string
	HasDecl  bool
}

// Result is what a statement changed.
type Result struct {
	RowsAffected int64
	LastInsertID int64
}

// Statement describes a compiled statement.
type Statement struct {
	ID uint32
	// ParamCount is sqlite3_bind_parameter_count, which reports the largest
	// index used rather than the number of parameters.
	ParamCount uint32
	// ParamCountExact reports whether ParamCount is safe to hand to
	// database/sql as NumInput; see docs/PROTOCOL.md §4.5.
	ParamCountExact bool
	TailOffset      uint32
	ReadOnly        bool
	Columns         []Column
}

// DB is one open database on the worker.
type DB struct {
	w           *Worker
	id          uint32
	cancelSlot  int
	cancellable bool
}

// Open opens a database. vfs may be empty for the default (transient) VFS.
func (w *Worker) Open(ctx context.Context, filename, vfs string, flags int32) (*DB, error) {
	r, err := w.call(ctx, wire.OpOpen, func(m *wire.Writer) {
		m.String(filename)
		m.String(vfs)
		m.I32(flags)
	})
	if err != nil {
		return nil, err
	}
	db := &DB{w: w, id: r.U32(), cancelSlot: int(r.U32()), cancellable: r.Bool()}
	return db, r.Err()
}

// Cancellable reports whether a running statement on this database can actually
// be interrupted. It requires a SharedArrayBuffer and an installable progress
// handler; without both, a cancelled context can only abandon the request.
func (d *DB) Cancellable() bool { return d.cancellable && d.w.CancelSupported() }

// Worker returns the transport this database belongs to.
func (d *DB) Worker() *Worker { return d.w }

func (d *DB) Close(ctx context.Context) error {
	_, err := d.w.call(ctx, wire.OpClose, func(m *wire.Writer) { m.U32(d.id) })
	return err
}

// Exec runs one or more statements. Arguments are consumed by the statements
// that take parameters, in order.
func (d *DB) Exec(ctx context.Context, sql string, args func(*wire.Writer)) (Result, error) {
	return d.exec(ctx, wire.OpExec, func(m *wire.Writer) {
		m.U32(d.id)
		m.String(sql)
		writeArgs(m, args)
	})
}

// ExecStatement runs a previously prepared statement.
func (d *DB) ExecStatement(ctx context.Context, stmtID uint32, args func(*wire.Writer)) (Result, error) {
	return d.exec(ctx, wire.OpExecStmt, func(m *wire.Writer) {
		m.U32(stmtID)
		writeArgs(m, args)
	})
}

func (d *DB) exec(ctx context.Context, op wire.Op, build func(*wire.Writer)) (Result, error) {
	id, route, err := d.w.begin()
	if err != nil {
		return Result{}, err
	}
	defer d.w.end(id)

	stop := d.armCancel(ctx, id)
	defer stop()

	msg := wire.NewWriter(op, 0, id)
	build(msg)
	d.w.post(msg.Frame())

	frame, err := route.next(ctx)
	if err != nil {
		return Result{}, err
	}
	h, r, err := wire.ReadHeader(frame)
	if err != nil {
		return Result{}, err
	}
	if h.Op == wire.OpError {
		return Result{}, readError(r)
	}
	if h.Op != wire.OpExecResult {
		return Result{}, fmt.Errorf("sqlite3-wasm: expected EXEC_RESULT, got %v", h.Op)
	}
	res := Result{RowsAffected: r.I64(), LastInsertID: r.I64()}
	return res, r.Err()
}

// Prepare compiles a single statement and keeps it on the worker.
func (d *DB) Prepare(ctx context.Context, sql string) (*Statement, error) {
	r, err := d.w.call(ctx, wire.OpPrepare, func(m *wire.Writer) {
		m.U32(d.id)
		m.String(sql)
	})
	if err != nil {
		return nil, err
	}
	s := &Statement{
		ID:              r.U32(),
		ParamCount:      r.U32(),
		ParamCountExact: r.Bool(),
		TailOffset:      r.U32(),
		ReadOnly:        r.Bool(),
	}
	s.Columns = readColumns(r)
	return s, r.Err()
}

// Finalize releases a prepared statement.
func (d *DB) Finalize(ctx context.Context, stmtID uint32) error {
	_, err := d.w.call(ctx, wire.OpFinalize, func(m *wire.Writer) { m.U32(stmtID) })
	return err
}

// Query runs a statement and streams its rows.
func (d *DB) Query(ctx context.Context, sql string, args func(*wire.Writer)) (*Rows, error) {
	return d.query(ctx, wire.OpQuery, func(m *wire.Writer) {
		m.U32(d.id)
		m.String(sql)
		writeArgs(m, args)
	})
}

// QueryStatement runs a previously prepared statement and streams its rows.
func (d *DB) QueryStatement(ctx context.Context, stmtID uint32, args func(*wire.Writer)) (*Rows, error) {
	return d.query(ctx, wire.OpQueryStmt, func(m *wire.Writer) {
		m.U32(stmtID)
		writeArgs(m, args)
	})
}

func (d *DB) query(ctx context.Context, op wire.Op, build func(*wire.Writer)) (*Rows, error) {
	id, route, err := d.w.begin()
	if err != nil {
		return nil, err
	}

	rows := &Rows{db: d, id: id, route: route, stopCancel: d.armCancel(ctx, id)}

	msg := wire.NewWriter(op, 0, id)
	build(msg)
	d.w.post(msg.Frame())

	// The first frame carries the column list, so it is read eagerly: the
	// caller needs Columns() before it ever calls Next.
	if err := rows.pull(ctx); err != nil {
		rows.Close()
		return nil, err
	}
	return rows, nil
}

// armCancel points the shared cancellation word at this request for as long as
// ctx is live. The watcher is always torn down, so a late Done cannot abort an
// unrelated later request — which is also why the word holds a request id
// rather than a flag.
func (d *DB) armCancel(ctx context.Context, id uint32) func() {
	if ctx.Done() == nil || !d.Cancellable() {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			d.w.setCancel(d.cancelSlot, id)
		case <-done:
		}
	}()
	return func() { close(done) }
}

// Rows streams a result set.
//
// A batch's bytes stay alive for as long as the batch is being read, and Next
// hands back a reader positioned at the row so the caller can decode straight
// into its destination rather than materialising the whole batch.
type Rows struct {
	db    *DB
	id    uint32
	route *route

	columns    []Column
	frame      []byte
	reader     *wire.Reader
	pending    uint32 // rows left in the current batch
	eof        bool
	closed     bool
	aborted    bool
	lastErr    error
	stopCancel func()
}

// Columns returns the result columns. They are read from the first frame, so
// they are available before the first Next.
func (r *Rows) Columns() []Column { return r.columns }

// pull reads the next batch into the reader.
func (r *Rows) pull(ctx context.Context) error {
	frame, err := r.route.next(ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			r.eof = true
			return io.EOF
		}
		return err
	}

	h, rd, err := wire.ReadHeader(frame)
	if err != nil {
		return err
	}
	switch h.Op {
	case wire.OpError:
		r.eof = true
		return readError(rd)
	case wire.OpAborted:
		r.eof = true
		r.aborted = true
		return io.EOF
	case wire.OpRows:
	default:
		return fmt.Errorf("sqlite3-wasm: expected ROWS, got %v", h.Op)
	}

	if h.HasColumns() {
		r.columns = readColumns(rd)
	}
	r.frame = frame
	r.reader = rd
	r.pending = rd.U32()
	if h.EOF() {
		r.eof = true
	}
	return rd.Err()
}

// Next positions the reader at the next row and returns it, or io.EOF.
//
// The returned reader points into the batch buffer, which stays valid until the
// following Next.
func (r *Rows) Next(ctx context.Context) (*wire.Reader, error) {
	if r.closed {
		return nil, io.EOF
	}
	for r.pending == 0 {
		if r.eof {
			return nil, io.EOF
		}
		// Credit is granted from the consumer, never from the message
		// callback: the worker may have only a bounded number of batches in
		// flight, and a slow reader must not be able to buffer a whole table.
		r.grantCredit()
		if err := r.pull(ctx); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			r.lastErr = err
			return nil, err
		}
	}
	r.pending--
	return r.reader, nil
}

func (r *Rows) grantCredit() {
	m := wire.NewWriter(wire.OpCredit, 0, r.id)
	m.U32(1)
	r.db.w.post(m.Frame())
}

// Aborted reports whether the stream ended because it was interrupted rather
// than exhausted.
func (r *Rows) Aborted() bool { return r.aborted }

// Close ends the stream. It never blocks: database/sql calls it from the
// context watcher while holding the connection mutex, so waiting for the worker
// here would pin a pooled connection for as long as the worker takes.
func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.stopCancel()

	if !r.eof {
		// Fire and forget. Frames already in flight are discarded by the
		// transport once the route is gone.
		r.db.w.post(wire.NewWriter(wire.OpAbort, 0, r.id).Frame())
	}
	r.db.w.end(r.id)
	r.frame = nil
	r.reader = nil
	return nil
}

func readColumns(r *wire.Reader) []Column {
	n := r.U32()
	if r.Err() != nil {
		return nil
	}
	cols := make([]Column, n)
	for i := range cols {
		cols[i].Name = r.String()
		cols[i].DeclType, cols[i].HasDecl = r.NullString()
	}
	return cols
}

// writeArgs frames the argument block. A nil builder writes an empty block.
func writeArgs(m *wire.Writer, args func(*wire.Writer)) {
	if args == nil {
		m.U32(0)
		return
	}
	args(m)
}
