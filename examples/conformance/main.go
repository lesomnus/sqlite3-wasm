//go:build js && wasm

// Command conformance exercises the behaviours that motivated the rewrite's
// harder design decisions: context cancellation, abandoning a result set,
// read-only transactions, pooled databases, and the DSN forms that used to be
// accepted and then quietly do the wrong thing.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlitewasm "github.com/lesomnus/sqlite3-wasm"
	"github.com/lesomnus/sqlite3-wasm/internal/assert"
)

func main() {
	testCancellation()
	testAbandonedRows()
	testReadOnlyTx()
	testPooledMemory()
	testPooledPersistent()
	testUnsupportedDSNs()

	fmt.Println("conformance ok")
}

// A cancelled context must return promptly and leave the connection usable.
// Without the SharedArrayBuffer word and the progress handler this could only
// be answered when the statement finished on its own.
func testCancellation() {
	db, err := sqlitewasm.OpenDB("file:/conformance-cancel?vfs=memdb")
	assert.NoErr(err)
	defer db.Close()

	// Warm up first. The very first database access also carries the worker
	// handshake, so timing a cancellation against a cold connection would
	// measure worker startup instead of statement interruption.
	assert.NoErr(db.Ping())

	const forever = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 1000000000)
	                 SELECT count(*) FROM c`

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	started := time.Now()
	var n int64
	err = db.QueryRowContext(ctx, forever).Scan(&n)
	elapsed := time.Since(started)

	assert.True(err != nil, "a cancelled query must fail")
	assert.True(errors.Is(err, context.DeadlineExceeded),
		fmt.Sprintf("want DeadlineExceeded, got %v", err))
	// Interrupting is best-effort, but it must not wait for a billion rows.
	assert.True(elapsed < 20*time.Second, fmt.Sprintf("cancellation took %v", elapsed))

	// And the connection still works afterwards.
	var one int
	assert.NoErr(db.QueryRow(`SELECT 1`).Scan(&one))
	assert.Eq(one, 1, "the connection survives a cancellation")
}

// Breaking out of a range over rows must not wedge anything: Rows.Close is
// called from database/sql's context watcher while it holds the connection
// mutex, so it can never block.
func testAbandonedRows() {
	db, err := sqlitewasm.OpenDB("file:/conformance-abandon?vfs=memdb")
	assert.NoErr(err)
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE big(i INTEGER);
	INSERT INTO big WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 50000)
	SELECT x FROM c`)
	assert.NoErr(err)

	rows, err := db.Query(`SELECT i FROM big ORDER BY i`)
	assert.NoErr(err)

	assert.True(rows.Next(), "at least one row")
	var first int64
	assert.NoErr(rows.Scan(&first))
	assert.Eq(first, int64(1))

	// Abandon the other 49 999.
	assert.NoErr(rows.Close())

	var n int64
	assert.NoErr(db.QueryRow(`SELECT count(*) FROM big`).Scan(&n))
	assert.Eq(n, int64(50000), "the connection is reusable after an abandoned scan")
}

func testReadOnlyTx() {
	db, err := sqlitewasm.OpenDB("file:/conformance-readonly?vfs=memdb")
	assert.NoErr(err)
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE t(x); INSERT INTO t VALUES (1)`)
	assert.NoErr(err)

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	assert.NoErr(err)

	var x int
	assert.NoErr(tx.QueryRow(`SELECT x FROM t`).Scan(&x))
	assert.Eq(x, 1)

	// query_only makes the promise enforceable rather than advisory.
	_, err = tx.Exec(`INSERT INTO t VALUES (2)`)
	assert.True(err != nil, "a read-only transaction must reject a write")
	assert.NoErr(tx.Rollback())

	// Writes work again once the transaction is over.
	_, err = db.Exec(`INSERT INTO t VALUES (3)`)
	assert.NoErr(err, "query_only is reset after the transaction")

	// A non-default isolation level is refused rather than silently ignored.
	_, err = db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	assert.True(err != nil, "a non-default isolation level must be refused")
}

// The bug this guards: SQLITE_OMIT_SHARED_CACHE makes `cache=shared` a silent
// no-op, so a pool on ":memory:" used to hand out mutually invisible databases.
// The DSN is rewritten onto the memdb VFS so they all see one.
func testPooledMemory() {
	db, err := sql.Open("sqlite3-wasm", ":memory:")
	assert.NoErr(err)
	defer db.Close()
	db.SetMaxOpenConns(2)

	ctx := context.Background()

	// Hold one connection so the second statement has to use another.
	first, err := db.Conn(ctx)
	assert.NoErr(err)
	defer first.Close()

	_, err = first.ExecContext(ctx, `CREATE TABLE shared(x); INSERT INTO shared VALUES (42)`)
	assert.NoErr(err)

	second, err := db.Conn(ctx)
	assert.NoErr(err)
	defer second.Close()

	var x int
	err = second.QueryRowContext(ctx, `SELECT x FROM shared`).Scan(&x)
	assert.NoErr(err, "a pooled in-memory database is shared between connections")
	assert.Eq(x, 42)
}

// Several connections to one persistent database are fine, and measured to be:
// they share a worker and therefore one sqlite3 instance, and the opfs VFS
// releases its sync access handle when idle. What they cannot buy is
// parallelism — there is one JavaScript thread behind all of them — which is
// why sqlitewasm.OpenDB pins the pool to one.
func testPooledPersistent() {
	db, err := sql.Open("sqlite3-wasm", "file:/conformance-pooled?vfs=memdb")
	assert.NoErr(err)
	defer db.Close()
	db.SetMaxOpenConns(2)

	ctx := context.Background()
	first, err := db.Conn(ctx)
	assert.NoErr(err)
	defer first.Close()
	_, err = first.ExecContext(ctx, `CREATE TABLE t(x); INSERT INTO t VALUES (7)`)
	assert.NoErr(err)

	started := time.Now()
	second, err := db.Conn(ctx)
	assert.NoErr(err, "a second connection must not be refused")
	defer second.Close()
	assert.True(time.Since(started) < 2*time.Second, "opening a second connection is prompt")

	var x int
	assert.NoErr(second.QueryRowContext(ctx, `SELECT x FROM t`).Scan(&x))
	assert.Eq(x, 7, "both connections see the same database")
}

func testUnsupportedDSNs() {
	// Both of these used to be accepted and then quietly do the wrong thing.
	for _, dsn := range []string{
		"file:x?mode=memory&cache=shared",
		"file:/y?vfs=memdb&_busy_timeout=5000",
	} {
		db, err := sql.Open("sqlite3-wasm", dsn)
		if err == nil {
			err = db.Ping()
			db.Close()
		}
		assert.True(err != nil, "DSN must be rejected: "+dsn)
	}
}
