//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"time"

	"github.com/lesomnus/sqlite3-wasm/binding"
	"github.com/lesomnus/sqlite3-wasm/internal/assert"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	w, err := binding.Spawn(ctx)
	assert.NoErr(err)
	defer w.Close()

	db, err := w.Open(ctx, "file:/example-open?vfs=memdb", "memdb", 0)
	assert.NoErr(err)

	fmt.Println("opened; cancellable:", db.Cancellable())
	assert.NoErr(db.Close(ctx))
}
