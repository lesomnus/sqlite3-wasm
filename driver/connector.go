//go:build js && wasm

package driver

import (
	"context"
	"database/sql/driver"

	"github.com/lesomnus/sqlite3-wasm/binding"
)

type Connector struct {
	name string
}

var _ driver.Connector = Connector{}

func (c Connector) Driver() driver.Driver {
	return Driver{}
}

func (c Connector) Connect(ctx context.Context) (driver.Conn, error) {
	p, err := binding.NewPromiser(ctx)
	if err != nil {
		return nil, err
	}

	_, err = p.Open(c.name)
	if err != nil {
		return nil, err
	}

	for range p.Exec(ctx, `PRAGMA foreign_keys = ON;`) {
	}

	return Conn{p}, nil
}
