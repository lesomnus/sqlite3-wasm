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

	info := w.Info()
	fmt.Printf("sqlite %s, protocol v%d, vfs %v\n", info.SQLiteVersion, info.ProtocolVersion, info.VFSList)

	assert.True(info.SQLiteVersion != "", "sqlite version")
	assert.Eq(int(info.ProtocolVersion), 1, "protocol version")
	// 64-bit accessors underpin the whole wire format, so the worker refuses to
	// start without them.
	assert.True(info.Capabilities.BigInt(), "BigInt support")
	assert.True(info.HasVFS("memdb"), "memdb VFS")
}
