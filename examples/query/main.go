//go:build js && wasm

package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/lesomnus/sqlite3-wasm/binding"
	"github.com/lesomnus/sqlite3-wasm/binding/wire"
	"github.com/lesomnus/sqlite3-wasm/internal/assert"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := binding.Spawn(ctx)
	assert.NoErr(err)
	defer w.Close()

	db, err := w.Open(ctx, "file:/example-query?vfs=memdb", "memdb", 0)
	assert.NoErr(err)
	defer db.Close(ctx)

	// The value the pre-rewrite driver could not carry: 2^53+1 arrived as a
	// BigInt and panicked syscall/js.
	rows, err := db.Query(ctx, "SELECT 9007199254740993 AS big, 1.0 AS real_one", nil)
	assert.NoErr(err)
	defer rows.Close()

	cols := rows.Columns()
	assert.Eq(len(cols), 2, "column count")
	assert.Eq(cols[0].Name, "big")

	r, err := rows.Next(ctx)
	assert.NoErr(err)

	assert.Eq(r.Tag(), wire.TagInt, "an INTEGER stays an INTEGER")
	assert.Eq(r.I64(), int64(9007199254740993), "int64 past 2^53")

	assert.Eq(r.Tag(), wire.TagReal, "a REAL stays a REAL even when integral")
	assert.Eq(r.F64(), 1.0)
	assert.NoErr(r.Err())

	_, err = rows.Next(ctx)
	assert.True(errors.Is(err, io.EOF), "end of rows")
}
