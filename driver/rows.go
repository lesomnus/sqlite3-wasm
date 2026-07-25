//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

// Rows streams a result set.
type Rows struct {
	ctx  context.Context
	c    *Conn
	rows *binding.Rows

	names     []string
	declTypes []string
	classes   []declClass
}

var (
	_ driver.Rows                           = (*Rows)(nil)
	_ driver.RowsColumnTypeDatabaseTypeName = (*Rows)(nil)
	_ driver.RowsColumnTypeScanType         = (*Rows)(nil)
)

// RowsColumnTypeNullable is deliberately not implemented. Returning ok=false is
// byte-for-byte identical to omitting the method, so it would be dead code —
// and the reason it cannot be supported is not that
// sqlite3_table_column_metadata is missing (it is present and works) but that
// sqlite3_column_origin_name, _table_name and _database_name are absent from
// this wasm build, so a result column cannot be traced back to a base table.

func newRows(ctx context.Context, c *Conn, rows *binding.Rows) *Rows {
	cols := rows.Columns()
	r := &Rows{
		ctx:       ctx,
		c:         c,
		rows:      rows,
		names:     make([]string, len(cols)),
		declTypes: make([]string, len(cols)),
		classes:   make([]declClass, len(cols)),
	}
	for i, col := range cols {
		r.names[i] = col.Name
		// Normalised once, here, and used for both value conversion and
		// ColumnTypeScanType.
		r.declTypes[i] = normalizeDeclType(col.DeclType)
		r.classes[i] = classifyDeclType(r.declTypes[i])
	}
	return r
}

func (r *Rows) Columns() []string { return r.names }

// ColumnTypeDatabaseTypeName returns the declared type, upper case and without
// any length, as database/sql documents. It is empty for an expression column.
func (r *Rows) ColumnTypeDatabaseTypeName(i int) string { return r.declTypes[i] }

// ColumnTypeScanType is derived from the declared type alone, never from a
// value: ColumnTypes may be called before the first Next, when every column
// still looks like NULL.
func (r *Rows) ColumnTypeScanType(i int) reflect.Type {
	return scanTypeForDeclType(r.declTypes[i])
}

// Next decodes one row directly into dest.
//
// Nothing is materialised ahead of it: the batch stays a byte slice and each
// cell is boxed once, straight into the slice database/sql reuses across calls.
func (r *Rows) Next(dest []driver.Value) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}

	rd, err := r.rows.Next(r.ctx)
	if err != nil {
		if errors.Is(err, io.EOF) {
			if r.rows.Aborted() {
				if ctxErr := r.ctx.Err(); ctxErr != nil {
					return ctxErr
				}
			}
			// Bare io.EOF: database/sql compares it with != rather than
			// errors.Is, so a wrapped one would surface as a driver error.
			return io.EOF
		}
		return r.c.classify(r.ctx, err)
	}

	for i := range r.classes {
		v := decodeValue(rd, r.classes[i], r.c.cfg)
		if i < len(dest) {
			dest[i] = v
		}
	}
	return rd.Err()
}

// Close ends the stream. It never blocks: database/sql calls it from the
// context watcher while holding the connection mutex.
func (r *Rows) Close() error { return r.rows.Close() }
