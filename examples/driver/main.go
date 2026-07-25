//go:build js && wasm

package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	sqlitewasm "github.com/lesomnus/sqlite3-wasm"
	"github.com/lesomnus/sqlite3-wasm/internal/assert"
)

func main() {
	// OpenDB pins the pool to one connection, which is what this architecture
	// can actually deliver: there is one JavaScript thread behind every
	// connection, and this build has no WAL.
	db, err := sqlitewasm.OpenDB("file:/example-driver?vfs=memdb")
	assert.NoErr(err)
	defer db.Close()

	assert.NoErr(db.Ping())

	// One Exec, several statements. The pre-rewrite driver silently dropped
	// everything after the first semicolon.
	_, err = db.Exec(`
CREATE TABLE users (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	name    TEXT NOT NULL,
	age     INTEGER,
	score   REAL,
	avatar  BLOB,
	active  BOOLEAN,
	created DATETIME
);
CREATE INDEX users_name ON users(name);`)
	assert.NoErr(err)

	created := time.Date(2024, 1, 2, 15, 4, 5, 123456789, time.UTC)
	res, err := db.Exec(
		`INSERT INTO users (name, age, score, avatar, active, created) VALUES (?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?)`,
		"Alice", int64(30), 1.0, []byte{0xde, 0xad}, true, created,
		"Bob", nil, 2.5, []byte{}, false, created.Add(time.Hour),
	)
	assert.NoErr(err)

	n, err := res.RowsAffected()
	assert.NoErr(err)
	assert.Eq(n, int64(2), "rows affected")

	id, err := res.LastInsertId()
	assert.NoErr(err)
	assert.Eq(id, int64(2), "last insert id")

	testTypes(db, created)
	testColumnTypes(db)
	testNamedParams(db)
	testTransaction(db)
	testInt64(db)
	testErrors(db)

	fmt.Println("driver example ok")
}

// A REAL stays a float64 even when its value is integral, and a BOOLEAN column
// scans as a bool. The old driver coerced every integral float to int64.
func testTypes(db *sql.DB, created time.Time) {
	var (
		name   string
		age    sql.NullInt64
		score  float64
		avatar []byte
		active bool
		at     time.Time
	)
	err := db.QueryRow(
		`SELECT name, age, score, avatar, active, created FROM users WHERE name = ?`, "Alice",
	).Scan(&name, &age, &score, &avatar, &active, &at)
	assert.NoErr(err)

	assert.Eq(name, "Alice")
	assert.True(age.Valid && age.Int64 == 30, "age")
	assert.Eq(score, 1.0, "an integral REAL stays a float64")
	assert.Eq(len(avatar), 2, "blob length")
	assert.Eq(active, true, "BOOLEAN scans as bool")
	assert.True(at.Equal(created), fmt.Sprintf("DATETIME round trip: got %v want %v", at, created))

	// NULL and a zero-length blob are different things.
	var bobAge sql.NullInt64
	var bobAvatar []byte
	err = db.QueryRow(`SELECT age, avatar FROM users WHERE name = ?`, "Bob").Scan(&bobAge, &bobAvatar)
	assert.NoErr(err)
	assert.Eq(bobAge.Valid, false, "NULL age")
	assert.True(bobAvatar != nil && len(bobAvatar) == 0, "an empty blob is not NULL")
}

func testColumnTypes(db *sql.DB) {
	rows, err := db.Query(`SELECT id, name, score, created FROM users LIMIT 0`)
	assert.NoErr(err)
	defer rows.Close()

	// ColumnTypes is answerable before the first Next, so it must come from the
	// declared type rather than from a value.
	types, err := rows.ColumnTypes()
	assert.NoErr(err)
	assert.Eq(len(types), 4, "column type count")
	assert.Eq(types[0].DatabaseTypeName(), "INTEGER")
	assert.Eq(types[1].DatabaseTypeName(), "TEXT")
	assert.Eq(types[2].DatabaseTypeName(), "REAL")
	assert.Eq(types[3].DatabaseTypeName(), "DATETIME")
	assert.Eq(types[3].ScanType().String(), "sql.NullTime")
}

func testNamedParams(db *sql.DB) {
	var name string
	err := db.QueryRow(`SELECT name FROM users WHERE age = :age`, sql.Named("age", 30)).Scan(&name)
	assert.NoErr(err)
	assert.Eq(name, "Alice")

	// A question mark inside a string literal is not a placeholder; the old
	// string-substitution shim rewrote it.
	var literal string
	assert.NoErr(db.QueryRow(`SELECT 'a?b'`).Scan(&literal))
	assert.Eq(literal, "a?b")
}

func testTransaction(db *sql.DB) {
	tx, err := db.Begin()
	assert.NoErr(err)
	_, err = tx.Exec(`INSERT INTO users (name) VALUES (?)`, "Carol")
	assert.NoErr(err)
	assert.NoErr(tx.Rollback())

	var n int
	assert.NoErr(db.QueryRow(`SELECT count(*) FROM users WHERE name = 'Carol'`).Scan(&n))
	assert.Eq(n, 0, "rollback discarded the insert")

	tx, err = db.Begin()
	assert.NoErr(err)
	_, err = tx.Exec(`INSERT INTO users (name) VALUES (?)`, "Dave")
	assert.NoErr(err)
	assert.NoErr(tx.Commit())

	assert.NoErr(db.QueryRow(`SELECT count(*) FROM users WHERE name = 'Dave'`).Scan(&n))
	assert.Eq(n, 1, "commit kept the insert")
}

// The value that used to panic the program.
func testInt64(db *sql.DB) {
	_, err := db.Exec(`CREATE TABLE big (v INTEGER)`)
	assert.NoErr(err)
	for _, v := range []int64{9007199254740993, math.MaxInt64, math.MinInt64} {
		_, err := db.Exec(`INSERT INTO big VALUES (?)`, v)
		assert.NoErr(err)
		var got int64
		assert.NoErr(db.QueryRow(`SELECT v FROM big WHERE v = ?`, v).Scan(&got))
		assert.Eq(got, v, "int64 round trip")
	}
}

func testErrors(db *sql.DB) {
	_, err := db.Exec(`INSERT INTO users (id, name) VALUES (1, 'dup')`)
	assert.True(err != nil, "a primary key conflict is an error")
	assert.True(errors.Is(err, sqlitewasm.ErrConstraint), fmt.Sprintf("errors.Is ErrConstraint: %v", err))

	var e *sqlitewasm.Error
	assert.True(errors.As(err, &e), "errors.As *sqlitewasm.Error")
	assert.Eq(e.ExtendedCode, int32(1555), "SQLITE_CONSTRAINT_PRIMARYKEY")

	err = db.QueryRow(`SELECT * FROM nope`).Scan()
	assert.True(err != nil, "querying a missing table is an error")

	// sql.ErrNoRows still works.
	var v int
	err = db.QueryRow(`SELECT id FROM users WHERE id = -1`).Scan(&v)
	assert.True(errors.Is(err, sql.ErrNoRows), "ErrNoRows")
}
