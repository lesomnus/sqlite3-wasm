//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

type Connector struct {
	d Driver
}

var _ driver.Connector = Connector{}

func (c Connector) Driver() driver.Driver {
	return c.d
}

func (c Connector) Connect(ctx context.Context) (driver.Conn, error) {
	p, err := binding.NewPromiser(ctx)
	if err != nil {
		return nil, err
	}

	_, err = p.Open(c.d.name)
	if err != nil {
		return nil, err
	}
	return Conn{p}, nil
}
