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
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	p, err := binding.NewPromiser(ctx)
	assert.NoErr(err)

	res, err := p.Open("file::memory:")
	assert.NoErr(err)

	fmt.Printf("res: %v\n", res)

	err = p.Close()
	assert.NoErr(err)
}
