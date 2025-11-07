//go:build js && wasm

package main

import (
	"database/sql"
	"fmt"

	_ "github.com/lesomnus/sqlite3-wasm"
	"github.com/lesomnus/sqlite3-wasm/internal/assert"
)

func main() {
	db, err := sql.Open("sqlite-wasm", "file::memory:")
	assert.NoErr(err)

	defer db.Close()

	err = db.Ping()
	assert.NoErr(err)

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, name TEXT)`)
	assert.NoErr(err)

	_, err = db.Exec(`INSERT INTO users (name) VALUES (?)`, "Alice")
	assert.NoErr(err)

	rows, err := db.Query(`SELECT id, name FROM users`)
	assert.NoErr(err)
	defer rows.Close()

	for rows.Next() {
		var id int
		var name string
		err := rows.Scan(&id, &name)
		assert.NoErr(err)
		fmt.Printf("id=%d name=%s\n", id, name)
	}

	err = rows.Err()
	assert.NoErr(err)
}
