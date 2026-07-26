//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"
)

// Driver is the database/sql driver registered as "sqlite3-wasm".
type Driver struct{}

var (
	_ driver.Driver        = Driver{}
	_ driver.DriverContext = Driver{}
)

// Open is the legacy entry point. database/sql prefers OpenConnector because
// this driver implements DriverContext, so this is only reached by a caller
// using the driver directly.
//
// The connection owns the connector it was minted from, and closing it
// terminates that connector's worker — otherwise this path would leak one
// worker per call.
func (d Driver) Open(name string) (driver.Conn, error) {
	c, err := newConnector(name)
	if err != nil {
		return nil, err
	}
	conn, err := c.Connect(context.Background())
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	conn.(*Conn).owned = c
	return conn, nil
}

// OpenConnector returns a fresh connector, and therefore a fresh worker, per
// call.
//
// Connectors are deliberately *not* memoised by DSN. Each sql.DB closes the
// connector it was opened with, so sharing one between two sql.DB values on the
// same DSN would mean closing either of them terminated the other's worker.
func (d Driver) OpenConnector(name string) (driver.Connector, error) {
	return newConnector(name)
}

func newConnector(dsn string) (*Connector, error) {
	cfg, err := ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return &Connector{dsn: dsn, cfg: cfg}, nil
}
