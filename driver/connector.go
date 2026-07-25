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
// of linear memory and a full wasm compile, connections to :memory: would be
// mutually invisible across workers, and two workers on one OPFS file contend
// for the same exclusive sync access handle.
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

	mu sync.Mutex
	// open counts live connections, so a file-backed database can refuse a
	// second one rather than stall inside the OPFS VFS.
	open int
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

	// A second handle on the same file is not slow, it is pathological: the
	// OPFS proxy retries createSyncAccessHandle six times with escalating
	// backoff, which is roughly 4.5 s of Atomics.wait inside the worker,
	// freezing every other connection, and then fails anyway.
	c.mu.Lock()
	if c.cfg.Persistent && c.open > 0 {
		c.mu.Unlock()
		return nil, fmt.Errorf(
			"sqlite3-wasm: %s is already open on this connector; "+
				"a file-backed database supports one connection at a time — call db.SetMaxOpenConns(1) "+
				"(sqlitewasm.OpenDB does this for you)", c.cfg.Filename)
	}
	c.open++
	c.mu.Unlock()

	release := func() {
		c.mu.Lock()
		c.open--
		c.mu.Unlock()
	}

	if err := c.checkVFS(w); err != nil {
		release()
		return nil, err
	}

	db, err := w.Open(ctx, c.cfg.FilenameFor(c.databaseID()), c.cfg.VFS, 0)
	if err != nil {
		release()
		return nil, err
	}

	conn := &Conn{db: db, cfg: c.cfg, release: release}
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
