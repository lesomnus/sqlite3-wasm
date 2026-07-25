//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"
	"sync"
)

// Driver is the database/sql driver registered as "sqlite3-wasm".
type Driver struct{}

var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

// connectors memoises one Connector per DSN.
//
// Driver.Open is a second entry point into the same DSN, and each Connector
// owns a database worker; without memoisation the two paths would each spawn
// one, and for a file-backed database that means two handles racing for the
// same exclusive OPFS access handle.
var (
	connectorsMu sync.Mutex
	connectors   = map[string]*Connector{}
)

func (Driver) Open(name string) (driver.Conn, error) {
	c, err := Driver{}.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

func (Driver) OpenConnector(name string) (driver.Connector, error) {
	connectorsMu.Lock()
	defer connectorsMu.Unlock()

	if c, ok := connectors[name]; ok {
		return c, nil
	}
	cfg, err := ParseDSN(name)
	if err != nil {
		return nil, err
	}
	c := &Connector{dsn: name, cfg: cfg}
	connectors[name] = c
	return c, nil
}

// forgetConnector drops a closed Connector so a later Open starts fresh.
func forgetConnector(dsn string) {
	connectorsMu.Lock()
	defer connectorsMu.Unlock()
	delete(connectors, dsn)
}
