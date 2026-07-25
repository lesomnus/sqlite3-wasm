//go:build js && wasm

package driver

import (
	"context"
	"crypto/rand"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

// Connector owns one database worker and mints connections on it.
//
// One worker per Connector rather than per connection: a worker commits 16 MiB
// of linear memory and a full wasm compile, and connections to :memory: would
// be mutually invisible across workers. Sharing also keeps every connection on
// one sqlite3 instance, which is what lets several of them address the same
// OPFS file without contending for its access handle.
type Connector struct {
	dsn string
	cfg *Config

	// id names an anonymous in-memory database so every connection in a pool
	// sees the same one.
	idOnce sync.Once
	id     string

	// The worker is created once, and its failure is memoised: the pool's
	// opener goroutine can call Connect concurrently, and each attempt must not
	// spawn another worker.
	workerOnce sync.Once
	worker     *binding.Worker
	workerErr  error
}

var (
	_ driver.Connector = (*Connector)(nil)
	_ io.Closer        = (*Connector)(nil)
)

func (c *Connector) Driver() driver.Driver { return Driver{} }

func (c *Connector) databaseID() string {
	c.idOnce.Do(func() {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			// crypto/rand does not fail under js/wasm, but a predictable name
			// is still better than a panic in a database driver.
			c.id = "sqlite3-wasm-memdb"
			return
		}
		c.id = "sqlite3-wasm-" + hex.EncodeToString(b[:])
	})
	return c.id
}

func (c *Connector) ensureWorker(ctx context.Context) (*binding.Worker, error) {
	c.workerOnce.Do(func() {
		c.worker, c.workerErr = binding.Spawn(ctx)
	})
	return c.worker, c.workerErr
}

func (c *Connector) Connect(ctx context.Context) (driver.Conn, error) {
	w, err := c.ensureWorker(ctx)
	if err != nil {
		return nil, err
	}

	// Note: several connections to the same OPFS file are fine here, and were
	// measured to be. Every connection on a connector shares one worker and so
	// one sqlite3 instance, which keeps its own file state; the opfs VFS also
	// releases its sync access handle when idle, so even two separate workers
	// interleave without stalling. (opfs-sahpool does not — it holds its
	// handles for the pool's lifetime — but it fails immediately and says so,
	// and one connector never creates a second worker.)
	//
	// What extra connections still cannot buy is parallelism: there is one
	// JavaScript thread behind all of them. See sqlitewasm.OpenDB.
	if err := c.checkVFS(w); err != nil {
		return nil, err
	}

	db, err := w.Open(ctx, c.cfg.FilenameFor(c.databaseID()), c.cfg.VFS, 0)
	if err != nil {
		return nil, err
	}

	conn := &Conn{db: db, cfg: c.cfg}
	if c.cfg.ForeignKeys {
		if _, err := db.Exec(ctx, "PRAGMA foreign_keys = ON", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// checkVFS turns a missing VFS into an actionable error instead of letting the
// database silently fall back to a transient one.
func (c *Connector) checkVFS(w *binding.Worker) error {
	if c.cfg.VFS == "" {
		return nil
	}
	info := w.Info()
	if info.HasVFS(c.cfg.VFS) {
		return nil
	}
	if c.cfg.VFS == "opfs-sahpool" && info.Capabilities.OPFSSAHPool() {
		// Installed lazily by the worker during OPEN.
		return nil
	}
	if c.cfg.VFS == "opfs" && !info.Capabilities.CrossOriginIsolated() {
		return fmt.Errorf(
			"sqlite3-wasm: the opfs VFS needs cross-origin isolation, which this page does not have; " +
				"serve it with Cross-Origin-Opener-Policy: same-origin and " +
				"Cross-Origin-Embedder-Policy: require-corp, or use vfs=opfs-sahpool")
	}
	return fmt.Errorf("sqlite3-wasm: no such vfs: %s (available: %v)", c.cfg.VFS, info.VFSList)
}

// Close terminates the worker. database/sql calls it from DB.Close.
func (c *Connector) Close() error {
	forgetConnector(c.dsn)
	w, _ := c.ensureWorker(context.Background())
	if w != nil {
		w.Close()
	}
	return nil
}
