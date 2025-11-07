//go:build js && wasm

package sqlitewasm

import (
	"database/sql"

	"github.com/lesomnus/sqlite3-wasm/driver"
)

func init() {
	sql.Register("sqlite-wasm", driver.Driver{})
}
