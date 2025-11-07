//go:build js && wasm

package binding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall/js"
	"time"

	"github.com/lesomnus/jz"
)

const noArg = 0

type Promiser struct {
	Value js.Value
}

func NewPromiser(ctx context.Context) (*Promiser, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
			init := js.Global().Get("sqlite-wasm-go")
			if init.IsUndefined() {
				continue
			}
			if init.Type() != js.TypeFunction {
				return nil, errors.New("`globalThis[\"sqlite-wasm-go\"]` must be a function")
			}

			factory := init.Invoke()
			promiser, err := jz.Await(factory)
			if err != nil {
				return nil, fmt.Errorf("factory rejected: %w", err)
			}

			return &Promiser{promiser}, nil
		}
	}
}

func (p Promiser) call(res any, args ...any) error {
	jv, err := jz.Await(p.Value.Invoke(args...))
	if err != nil {
		return err
	}

	result := jv.Get("result")
	if result.IsUndefined() {
		return errors.New("result was undefined")
	}
	if err := jz.Unmarshal(result, res); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}

	return nil
}

func (p Promiser) GetConfig() (Config, error) {
	var v Config
	err := p.call(&v, "config-get", noArg)
	return v, err
}

func (p Promiser) Open(filename string) (OpenResult, error) {
	var v OpenResult
	err := p.call(&v, "open", map[string]any{
		"filename": filename,
	})
	return v, err
}

func (p Promiser) Close() error {
	var v OpenResult
	return p.call(&v, "close", noArg)
}

func (p Promiser) Exec(ctx context.Context, query string) <-chan RowResult {
	type Z struct{}

	ok := true
	ch := make(chan RowResult)
	done := func() {
		ok = false
		close(ch)
	}
	abort := func(err error) {
		ok = false
		ch <- RowResult{Error: err}
		close(ch)
	}

	h := func(args []js.Value) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(args) == 0 {
			return errors.New("callback is called with no args")
		}

		var v RowResult
		if err := jz.Unmarshal(args[0], &v); err != nil {
			return fmt.Errorf("unmarshal the result: %w", err)
		}

		ch <- v
		if v.RowNumber == 0 {
			return io.EOF
		}

		return nil
	}

	cb := func(this js.Value, args []js.Value) any {
		if !ok {
			return js.Undefined()
		}

		err := h(args)
		if err == nil {
			return js.Undefined()
		}
		if errors.Is(err, io.EOF) {
			return js.Undefined()
		}

		abort(err)
		return js.Undefined()
	}

	p.Value.Invoke("exec", map[string]any{
		"sql":      query,
		"callback": js.FuncOf(cb),
	}).Call("then", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !ok {
			return js.Undefined()
		}

		done()
		return js.Undefined()
	})).Call("catch", js.FuncOf(func(this js.Value, args []js.Value) any {
		if !ok {
			return js.Undefined()
		}
		if len(args) == 0 {
			abort(errors.New("exec failed"))
			return js.Undefined()
		}

		abort(fmt.Errorf("exec failed: %s", jz.Stringify(args[0])))
		return js.Undefined()
	}))

	return ch
}
