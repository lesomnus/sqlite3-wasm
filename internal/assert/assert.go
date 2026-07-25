// Package assert is a minimal test helper for the example programs, which run
// as Go/wasm inside a browser worker.
//
// It reports failures through panic, which the runner turns into a non-zero
// exit code. The message carries the caller's file and line, because a browser
// worker has no other way to say where an assertion failed — the previous
// version panicked with an empty []string and no location at all.
package assert

import (
	"fmt"
	"runtime"
)

// NoErr fails unless err is nil.
func NoErr(err error, what ...any) {
	if err == nil {
		return
	}
	fail(fmt.Sprintf("unexpected error: %v", err), what)
}

// True fails unless ok.
func True(ok bool, what ...any) {
	if ok {
		return
	}
	fail("assertion failed", what)
}

// Eq fails unless got and want are equal.
func Eq[T comparable](got, want T, what ...any) {
	if got == want {
		return
	}
	fail(fmt.Sprintf("got %v (%T), want %v (%T)", got, got, want, want), what)
}

func fail(msg string, what []any) {
	if len(what) > 0 {
		msg = fmt.Sprint(what...) + ": " + msg
	}
	if _, file, line, ok := runtime.Caller(2); ok {
		msg = fmt.Sprintf("%s:%d: %s", file, line, msg)
	}
	panic(msg)
}
