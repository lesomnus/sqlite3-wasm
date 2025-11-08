//go:build js && wasm

package driver

import (
	"database/sql/driver"
	"fmt"
	"io"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

// Rows implements driver.Rows by reading binding.RowResult values from a
// channel produced by binding.Promiser.Exec.
type Rows struct {
	cols   []string
	ch     <-chan binding.RowResult
	cur    []any
	closed bool
}

var _ driver.Rows = &Rows{}

func (r *Rows) Columns() []string { return r.cols }

func (r *Rows) Close() error {
	if r.closed {
		return nil
	}
	// drain channel to allow promiser callbacks to finish
	for rr := range r.ch {
		if rr.RowNumber == 0 {
			break
		}
	}
	r.closed = true
	return nil
}

func (r *Rows) Next(dest []driver.Value) error {
	if r.closed {
		return io.EOF
	}

	if r.cur == nil {
		rr, ok := <-r.ch
		if !ok {
			r.closed = true
			return io.EOF
		}
		if rr.Error != nil {
			r.closed = true
			return rr.Error
		}
		if rr.RowNumber == 0 {
			r.closed = true
			return io.EOF
		}
		// populate columns if not already set
		if r.cols == nil && len(rr.ColumnNames) > 0 {
			r.cols = rr.ColumnNames
		}
		r.cur = rr.Row
	}

	if len(dest) < len(r.cur) {
		return fmt.Errorf("destination len %d < row len %d", len(dest), len(r.cur))
	}

	for i := range r.cur {
		dest[i] = convertToDriverValue(r.cur[i])
	}
	// clear current row so next call reads the following one
	r.cur = nil
	return nil
}

func convertToDriverValue(v any) driver.Value {
	switch t := v.(type) {
	case nil:
		return nil
	case int:
		return int64(t)
	case int8:
		return int64(t)
	case int16:
		return int64(t)
	case int32:
		return int64(t)
	case int64:
		return t
	case uint:
		return int64(t)
	case uint8:
		return int64(t)
	case uint16:
		return int64(t)
	case uint32:
		return int64(t)
	case uint64:
		return int64(t)
	case float32:
		return float64(t)
	case float64:
		return t
	case bool:
		return t
	case string:
		return t
	case []byte:
		return t
	default:
		return fmt.Sprintf("%v", t)
	}
}
