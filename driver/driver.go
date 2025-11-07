//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"
)

type Driver struct {
	name string
}

func (d Driver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}

	return c.Connect(context.Background())
}

func (d Driver) OpenConnector(name string) (driver.Connector, error) {
	return Connector{Driver{name}}, nil
}
