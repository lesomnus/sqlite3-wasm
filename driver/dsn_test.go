package driver

import (
	"strings"
	"testing"
	"time"
)

func TestParseDSN(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	tests := []struct {
		name string
		dsn  string
		want func(*testing.T, *Config)
	}{
		{
			name: "bare path is persistent",
			dsn:  "app.db",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "filename", c.Filename, "app.db")
				assertEq(t, "vfs", c.VFS, "")
				assertEq(t, "persistent", c.Persistent, true)
				assertEq(t, "memory", c.Memory, false)
			},
		},
		{
			name: "defaults",
			dsn:  "app.db",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "loc", c.Loc.String(), time.UTC.String())
				assertEq(t, "foreign keys", c.ForeignKeys, true)
				assertEq(t, "txlock", c.TxLock, "immediate")
				assertEq(t, "time format", c.TimeFormat, TimeFormatOffset)
				assertEq(t, "integer time", c.IntegerTime, IntegerTimeNone)
			},
		},
		{
			name: "empty dsn is a private temporary database",
			dsn:  "",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "filename", c.Filename, "")
				assertEq(t, "persistent", c.Persistent, false)
				assertEq(t, "memory", c.Memory, false)
			},
		},
		{
			// The README's canonical example has to keep working.
			name: "file uri with a vfs",
			dsn:  "file:sqlite3.db?vfs=opfs",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "filename", c.Filename, "file:sqlite3.db?vfs=opfs")
				assertEq(t, "vfs", c.VFS, "opfs")
				assertEq(t, "persistent", c.Persistent, true)
			},
		},
		{
			name: "driver parameters are stripped from the filename",
			dsn:  "file:app.db?vfs=opfs&_loc=Asia/Seoul&_fk=0&_txlock=deferred",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "filename", c.Filename, "file:app.db?vfs=opfs")
				assertEq(t, "loc", c.Loc.String(), seoul.String())
				assertEq(t, "foreign keys", c.ForeignKeys, false)
				assertEq(t, "txlock", c.TxLock, "deferred")
			},
		},
		{
			name: "sqlite parameters survive alongside driver parameters",
			dsn:  "file:app.db?mode=ro&immutable=1&_fk=1",
			want: func(t *testing.T, c *Config) {
				// url.Values.Encode sorts, so the order is stable.
				assertEq(t, "filename", c.Filename, "file:app.db?immutable=1&mode=ro")
			},
		},
		{
			name: "bare :memory: becomes a named memdb database",
			dsn:  ":memory:",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "memory", c.Memory, true)
				assertEq(t, "vfs", c.VFS, "memdb")
				assertEq(t, "persistent", c.Persistent, false)
				assertEq(t, "filename for", c.FilenameFor("abc"), "file:/abc")
			},
		},
		{
			name: "file::memory: becomes a named memdb database",
			dsn:  "file::memory:",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "memory", c.Memory, true)
				assertEq(t, "vfs", c.VFS, "memdb")
				assertEq(t, "filename for", c.FilenameFor("abc"), "file:/abc")
			},
		},
		{
			name: "mode=memory becomes a named memdb database",
			dsn:  "file:whatever?mode=memory",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "memory", c.Memory, true)
				assertEq(t, "vfs", c.VFS, "memdb")
				assertEq(t, "filename for", c.FilenameFor("xyz"), "file:/xyz")
			},
		},
		{
			// An explicitly named memdb path is already shared between
			// handles, so it must survive verbatim.
			name: "explicit memdb path is left alone",
			dsn:  "file:/shared?vfs=memdb",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "memory", c.Memory, true)
				assertEq(t, "vfs", c.VFS, "memdb")
				assertEq(t, "filename", c.Filename, "file:/shared?vfs=memdb")
				assertEq(t, "filename for", c.FilenameFor("abc"), "file:/shared?vfs=memdb")
			},
		},
		{
			// A bare path may contain a '?', and it has no parameters.
			name: "a stray question mark stays part of a plain filename",
			dsn:  "weird?name.db",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "filename", c.Filename, "weird?name.db")
				assertEq(t, "persistent", c.Persistent, true)
			},
		},
		{
			// ":memory:" does take parameters, and folding them into the
			// filename silently bypassed every guarantee below.
			name: "bare :memory: accepts driver parameters",
			dsn:  ":memory:?_fk=0&_loc=Asia/Seoul",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "memory", c.Memory, true)
				assertEq(t, "vfs", c.VFS, "memdb")
				assertEq(t, "foreign keys", c.ForeignKeys, false)
				assertEq(t, "loc", c.Loc.String(), seoul.String())
			},
		},
		{
			name: "_timezone is accepted as an alias for _loc",
			dsn:  "file:app.db?_timezone=Asia/Seoul",
			want: func(t *testing.T, c *Config) { assertEq(t, "loc", c.Loc.String(), seoul.String()) },
		},
		{
			name: "_time_format",
			dsn:  "file:app.db?_time_format=utc",
			want: func(t *testing.T, c *Config) { assertEq(t, "time format", c.TimeFormat, TimeFormatUTC) },
		},
		{
			name: "_time_integer_format",
			dsn:  "file:app.db?_time_integer_format=unix_milli",
			want: func(t *testing.T, c *Config) {
				assertEq(t, "integer time", c.IntegerTime, IntegerTimeUnixMilli)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseDSN(tc.dsn)
			if err != nil {
				t.Fatalf("ParseDSN(%q): %v", tc.dsn, err)
			}
			tc.want(t, cfg)
		})
	}
}

func TestParseDSNRejections(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		mustSay string
	}{
		{
			// This is the mattn-standard DSN every migrating user will paste,
			// and in this build it silently yields N invisible databases.
			name:    "cache=shared",
			dsn:     "file:x?mode=memory&cache=shared",
			mustSay: "vfs=memdb",
		},
		{
			name:    "cache=private",
			dsn:     "file:x?cache=private",
			mustSay: "SQLITE_OMIT_SHARED_CACHE",
		},
		{
			// A non-zero busy timeout blocks the worker thread that would have
			// to deliver the COMMIT releasing the lock.
			name:    "_busy_timeout",
			dsn:     "file:x?_busy_timeout=5000",
			mustSay: "deadlock",
		},
		{
			// The most common DSN in the mattn ecosystem, in its bare form.
			name:    "bare :memory: with cache=shared",
			dsn:     ":memory:?cache=shared",
			mustSay: "vfs=memdb",
		},
		{
			name:    "bare :memory: with _busy_timeout",
			dsn:     ":memory:?_busy_timeout=5000",
			mustSay: "deadlock",
		},
		{
			name:    "unknown driver parameter",
			dsn:     "file:x?_nope=1",
			mustSay: "_nope",
		},
		{
			name:    "bad _time_format",
			dsn:     "file:x?_time_format=rfc3339",
			mustSay: "offset, utc or datetime",
		},
		{
			name:    "bad _txlock",
			dsn:     "file:x?_txlock=shared",
			mustSay: "immediate, deferred or exclusive",
		},
		{
			name:    "bad _fk",
			dsn:     "file:x?_fk=maybe",
			mustSay: "_fk",
		},
		{
			name:    "unknown timezone",
			dsn:     "file:x?_loc=Mars/Olympus",
			mustSay: "Mars/Olympus",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDSN(tc.dsn)
			if err == nil {
				t.Fatalf("ParseDSN(%q) succeeded, want an error", tc.dsn)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Errorf("error %q does not mention %q", err, tc.mustSay)
			}
		})
	}
}

func TestTxBeginStatement(t *testing.T) {
	for dsn, want := range map[string]string{
		"file:x":                   "BEGIN IMMEDIATE",
		"file:x?_txlock=immediate": "BEGIN IMMEDIATE",
		"file:x?_txlock=deferred":  "BEGIN DEFERRED",
		"file:x?_txlock=exclusive": "BEGIN EXCLUSIVE",
	} {
		cfg, err := ParseDSN(dsn)
		if err != nil {
			t.Fatalf("ParseDSN(%q): %v", dsn, err)
		}
		assertEq(t, dsn, cfg.txBeginStatement(), want)
	}
}

func assertEq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
