//go:build js && wasm

package main

import (
	"context"
	"time"

	"github.com/lesomnus/sqlite3-wasm/binding"
	"github.com/lesomnus/sqlite3-wasm/internal/assert"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	p, err := binding.NewPromiser(ctx)
	assert.NoErr(err)

	_, err = p.Open("file::memory:")
	assert.NoErr(err)
	defer p.Close()

	c := p.Exec(ctx, `SELECT 1 + 1 AS foo;`)

	res := <-c
	assert.NoErr(res.Error)
	assert.X(len(res.ColumnNames) == 1)
	assert.X(res.ColumnNames[0] == "foo")
	assert.X(len(res.Row) == 1)
	assert.X(res.Row[0] == float64(2))
	assert.X(res.RowNumber == 1)

	res = <-c
	assert.NoErr(res.Error)
	assert.X(res.RowNumber == 0)
}
