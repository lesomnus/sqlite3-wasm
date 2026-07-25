//go:build js && wasm

// Package binding is the Go half of the transport described in
// docs/PROTOCOL.md. It owns the DB worker, correlates requests, and hands
// frames to the driver without ever blocking the JavaScript event loop.
package binding

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall/js"
	"time"

	"github.com/lesomnus/sqlite3-wasm/binding/wire"
)

// GlobalKey is the property the JavaScript entry installs on globalThis. It is
// mirrored by GLOBAL_KEY in src/global.ts.
const GlobalKey = "sqlite3-wasm-go"

// legacyGlobalKey is the pre-rewrite spelling, checked for only so that a stale
// bundle produces a real message instead of a hang.
const legacyGlobalKey = "sqlite-wasm-go"

// handshakeTimeout bounds worker startup independently of any caller's context:
// a worker that fails to construct — a CSP block, an unsupported engine, a wasm
// compile failure — must not look like a slow query.
const handshakeTimeout = 30 * time.Second

var (
	uint8Array = js.Global().Get("Uint8Array")
	int32Array = js.Global().Get("Int32Array")
	atomics    = js.Global().Get("Atomics")
)

// Capabilities reports what the worker's environment supports. Callers use it
// to fail loudly rather than degrade silently.
type Capabilities uint32

func (c Capabilities) CrossOriginIsolated() bool {
	return c&Capabilities(wire.CapCrossOriginIsolated) != 0
}
func (c Capabilities) SharedArrayBuffer() bool { return c&Capabilities(wire.CapSharedArrayBuffer) != 0 }
func (c Capabilities) BigInt() bool            { return c&Capabilities(wire.CapBigInt) != 0 }
func (c Capabilities) ProgressHandler() bool   { return c&Capabilities(wire.CapProgressHandler) != 0 }
func (c Capabilities) OPFS() bool              { return c&Capabilities(wire.CapVFSOpfs) != 0 }
func (c Capabilities) OPFSSAHPool() bool       { return c&Capabilities(wire.CapVFSOpfsSAHPool) != 0 }
func (c Capabilities) Memdb() bool             { return c&Capabilities(wire.CapVFSMemdb) != 0 }

// Info is the worker's handshake reply.
type Info struct {
	ProtocolVersion uint16
	SQLiteVersion   string
	Capabilities    Capabilities
	VFSList         []string
}

// HasVFS reports whether the named VFS is registered.
func (i Info) HasVFS(name string) bool {
	for _, v := range i.VFSList {
		if v == name {
			return true
		}
	}
	return false
}

// route receives frames for one request id.
//
// Frames are appended under a mutex and a waiter is woken through a capacity-1
// channel. A blocking channel send from inside a js.Func would wedge the whole
// JavaScript event loop and busy-spin the Go runtime into a fatal deadlock, so
// the hand-off must never block.
type route struct {
	mu     sync.Mutex
	frames [][]byte
	err    error
	closed bool
	wake   chan struct{}
}

func newRoute() *route {
	return &route{wake: make(chan struct{}, 1)}
}

func (r *route) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *route) push(frame []byte) {
	r.mu.Lock()
	r.frames = append(r.frames, frame)
	r.mu.Unlock()
	r.signal()
}

func (r *route) fail(err error) {
	r.mu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.closed = true
	r.mu.Unlock()
	r.signal()
}

// next blocks until a frame arrives, the route fails, or ctx is done.
func (r *route) next(ctx context.Context) ([]byte, error) {
	for {
		r.mu.Lock()
		if len(r.frames) > 0 {
			f := r.frames[0]
			r.frames = r.frames[1:]
			r.mu.Unlock()
			return f, nil
		}
		err := r.err
		closed := r.closed
		r.mu.Unlock()

		if err != nil {
			return nil, err
		}
		if closed {
			return nil, io.EOF
		}

		select {
		case <-r.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Worker owns one DB worker and multiplexes requests over it.
type Worker struct {
	js   js.Value
	info Info

	onMessage      js.Func
	onError        js.Func
	onMessageError js.Func

	cancelView js.Value // Int32Array, or undefined without SharedArrayBuffer

	mu     sync.Mutex
	routes map[uint32]*route
	nextID uint32
	dead   error
	closed bool
}

// Lookup finds the JavaScript entry point and checks that it speaks this
// protocol version.
//
// It reads the global once, synchronously. ES module evaluation guarantees the
// global is installed before the importing module body runs, so polling for it
// would only turn a forgotten import into an indefinite hang.
func Lookup() (js.Value, error) {
	g := js.Global().Get(GlobalKey)
	if g.IsUndefined() || g.IsNull() {
		if v := js.Global().Get(legacyGlobalKey); !v.IsUndefined() {
			return js.Value{}, fmt.Errorf(
				"sqlite3-wasm: found the retired global %q; this driver needs %q "+
					"(update the JavaScript package and import it from inside the Go worker)",
				legacyGlobalKey, GlobalKey)
		}
		return js.Value{}, fmt.Errorf(
			"sqlite3-wasm: globalThis[%q] is not installed; "+
				"import the sqlite3-wasm-go package from the same worker that runs this program",
			GlobalKey)
	}

	v := g.Get("protocolVersion")
	if v.Type() != js.TypeNumber {
		return js.Value{}, fmt.Errorf(
			"sqlite3-wasm: globalThis[%q] has no protocolVersion; it is probably from an older release",
			GlobalKey)
	}
	if got := v.Int(); got != wire.Version {
		return js.Value{}, fmt.Errorf(
			"sqlite3-wasm: protocol version mismatch: the JavaScript package speaks v%d, this driver speaks v%d",
			got, wire.Version)
	}
	return g, nil
}

// Spawn creates a DB worker and completes the handshake.
func Spawn(ctx context.Context) (*Worker, error) {
	g, err := Lookup()
	if err != nil {
		return nil, err
	}

	w := &Worker{
		routes: make(map[uint32]*route),
		nextID: 1,
	}

	handshake := newRoute()
	w.routes[0] = handshake

	w.onMessage = js.FuncOf(w.receive)
	w.onError = js.FuncOf(func(_ js.Value, args []js.Value) any {
		w.die(fmt.Errorf("sqlite3-wasm: database worker failed: %s", errorText(args)))
		return nil
	})
	w.onMessageError = js.FuncOf(func(_ js.Value, _ []js.Value) any {
		w.die(errors.New("sqlite3-wasm: database worker sent an undeserialisable message"))
		return nil
	})

	worker := g.Call("createWorker")
	worker.Set("onmessage", w.onMessage)
	worker.Set("onerror", w.onError)
	worker.Set("onmessageerror", w.onMessageError)
	w.js = worker

	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	frame, err := handshake.next(hctx)
	if err != nil {
		w.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("sqlite3-wasm: the database worker did not become ready")
		}
		return nil, err
	}

	w.mu.Lock()
	delete(w.routes, 0)
	w.mu.Unlock()

	if err := w.readReady(frame); err != nil {
		w.Close()
		return nil, err
	}
	return w, nil
}

func (w *Worker) readReady(frame []byte) error {
	h, r, err := wire.ReadHeader(frame)
	if err != nil {
		return err
	}
	if h.Op != wire.OpReady {
		return fmt.Errorf("sqlite3-wasm: expected READY, got %v", h.Op)
	}

	info := Info{
		ProtocolVersion: r.U16(),
		SQLiteVersion:   r.String(),
		Capabilities:    Capabilities(r.U32()),
	}
	n := r.U32()
	for i := uint32(0); i < n; i++ {
		info.VFSList = append(info.VFSList, r.String())
	}
	if err := r.Err(); err != nil {
		return err
	}
	if info.ProtocolVersion != wire.Version {
		return fmt.Errorf(
			"sqlite3-wasm: worker speaks protocol v%d, this driver speaks v%d",
			info.ProtocolVersion, wire.Version)
	}
	w.info = info
	return nil
}

// Info reports the worker's handshake reply.
func (w *Worker) Info() Info { return w.info }

// receive runs on the JavaScript event loop. It copies the payload out
// immediately and hands it off without blocking.
func (w *Worker) receive(_ js.Value, args []js.Value) any {
	// An unrecovered panic inside a js.Func kills the whole Go program and
	// cannot be caught on the JavaScript side.
	defer func() {
		if r := recover(); r != nil {
			w.die(fmt.Errorf("sqlite3-wasm: panic while receiving a frame: %v", r))
		}
	}()

	if len(args) == 0 {
		return nil
	}
	data := args[0].Get("data")

	// The handshake is the one message that is not a bare Uint8Array, because a
	// SharedArrayBuffer can neither ride inside one nor be transferred.
	if !data.InstanceOf(uint8Array) {
		payload := data.Get("p")
		if payload.IsUndefined() {
			w.die(errors.New("sqlite3-wasm: unrecognised message from the database worker"))
			return nil
		}
		if cancel := data.Get("cancel"); !cancel.IsUndefined() && !cancel.IsNull() {
			w.cancelView = int32Array.New(cancel)
		}
		data = payload
	}

	frame := make([]byte, data.Get("length").Int())
	js.CopyBytesToGo(frame, data)
	// The js.Value is dropped here on purpose: its release is GC-latency bound
	// and there is no manual free, so queueing js.Values would pin the
	// transferred buffers.

	h, _, err := wire.ReadHeader(frame)
	if err != nil {
		w.die(err)
		return nil
	}

	w.mu.Lock()
	r := w.routes[h.ID]
	w.mu.Unlock()
	if r == nil {
		// A late frame for an aborted or already-finished request. Discarding
		// it is specified behaviour, not an error.
		return nil
	}
	r.push(frame)
	return nil
}

// die fails every outstanding request and marks the worker unusable.
func (w *Worker) die(err error) {
	w.mu.Lock()
	if w.dead == nil {
		w.dead = err
	}
	routes := make([]*route, 0, len(w.routes))
	for _, r := range w.routes {
		routes = append(routes, r)
	}
	w.routes = map[uint32]*route{}
	w.mu.Unlock()

	for _, r := range routes {
		r.fail(err)
	}
}

// Err reports why the worker is unusable, if it is.
func (w *Worker) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dead
}

// Close terminates the worker and releases every js.Func it holds. Without the
// releases the Go closures, their JavaScript wrappers and their reference slots
// leak for the life of the program.
func (w *Worker) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.mu.Unlock()

	w.die(errors.New("sqlite3-wasm: connection closed"))

	if !w.js.IsUndefined() {
		w.js.Set("onmessage", js.Null())
		w.js.Set("onerror", js.Null())
		w.js.Set("onmessageerror", js.Null())
		w.js.Call("terminate")
	}
	w.onMessage.Release()
	w.onError.Release()
	w.onMessageError.Release()
}

// begin registers a route and returns its request id.
func (w *Worker) begin() (uint32, *route, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead != nil {
		return 0, nil, w.dead
	}
	id := w.nextID
	w.nextID++
	// Zero means "no request" in the cancellation word.
	if w.nextID == 0 {
		w.nextID = 1
	}
	r := newRoute()
	w.routes[id] = r
	return id, r, nil
}

func (w *Worker) end(id uint32) {
	w.mu.Lock()
	delete(w.routes, id)
	w.mu.Unlock()
}

// post sends a frame, transferring its buffer.
func (w *Worker) post(frame []byte) {
	arr := uint8Array.New(len(frame))
	js.CopyBytesToJS(arr, frame)
	w.js.Call("postMessage", arr, []any{arr.Get("buffer")})
}

// call performs a request with exactly one reply frame.
func (w *Worker) call(ctx context.Context, op wire.Op, build func(*wire.Writer)) (*wire.Reader, error) {
	id, r, err := w.begin()
	if err != nil {
		return nil, err
	}
	defer w.end(id)

	msg := wire.NewWriter(op, 0, id)
	if build != nil {
		build(msg)
	}
	w.post(msg.Frame())

	frame, err := r.next(ctx)
	if err != nil {
		return nil, err
	}
	h, rd, err := wire.ReadHeader(frame)
	if err != nil {
		return nil, err
	}
	if h.Op == wire.OpError {
		return nil, readError(rd)
	}
	return rd, nil
}

// setCancel points the shared word at the request to abort. Go only ever
// stores: Atomics.wait from Go would freeze its own event loop, and with it the
// delivery of every reply.
func (w *Worker) setCancel(slot int, requestID uint32) {
	if w.cancelView.IsUndefined() {
		return
	}
	atomics.Call("store", w.cancelView, slot, int32(requestID))
}

// CancelSupported reports whether cancellation can actually interrupt a running
// statement. It needs both a SharedArrayBuffer and an installable progress
// handler.
func (w *Worker) CancelSupported() bool {
	return !w.cancelView.IsUndefined() && w.info.Capabilities.ProgressHandler()
}

func errorText(args []js.Value) string {
	if len(args) == 0 {
		return "unknown error"
	}
	if m := args[0].Get("message"); m.Type() == js.TypeString {
		return m.String()
	}
	return "unknown error"
}
