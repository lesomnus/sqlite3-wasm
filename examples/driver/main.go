//go:build js && wasm

package main

import (
	"database/sql"

	_ "github.com/lesomnus/sqlite3-wasm"
	"github.com/lesomnus/sqlite3-wasm/internal/assert"
)

func main() {
	db, err := sql.Open("sqlite3-wasm", "file::memory:")
	assert.NoErr(err)

	defer db.Close()

	err = db.Ping()
	assert.NoErr(err)

	_, err = db.Exec(`
CREATE TABLE users (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	age INTEGER
);`,
	)
	assert.NoErr(err)

	_, err = db.Exec(`
INSERT INTO users (name, age) VALUES (?, 30), ('Bob', ?);`,
		"Alice", 25)
	assert.NoErr(err)

	rows, err := db.Query(`SELECT * FROM users`)
	assert.NoErr(err)
	defer rows.Close()

	var (
		id   int
		name string
		age  int
	)

	ok := rows.Next()
	assert.X(ok)

	err = rows.Scan(&id, &name, &age)
	assert.NoErr(err)
	assert.X(id == 1)
	assert.X(name == "Alice")
	assert.X(age == 30)

	ok = rows.Next()
	assert.X(ok)

	err = rows.Scan(&id, &name, &age)
	assert.NoErr(err)
	assert.X(id == 2)
	assert.X(name == "Bob")
	assert.X(age == 25)

	err = rows.Err()
	assert.NoErr(err)
}
