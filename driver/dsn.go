package driver

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TimeFormat selects how a time.Time is written to a TEXT column.
type TimeFormat uint8

const (
	// TimeFormatOffset writes "2006-01-02T15:04:05.000000000Z07:00" -- so `Z`
	// at UTC and `+09:00` elsewhere, which is RFC 3339 and what everything
	// else writes. It is understood by SQLite's datetime().
	TimeFormatOffset TimeFormat = iota
	// TimeFormatUTC writes "2006-01-02T15:04:05.000000000Z" in UTC. Unlike the
	// offset form it sorts and indexes correctly across zones.
	TimeFormatUTC
	// TimeFormatDatetime writes "2006-01-02T15:04:05".
	TimeFormatDatetime
)

// IntegerTimeFormat selects an integer encoding for time.Time instead of TEXT.
type IntegerTimeFormat uint8

const (
	IntegerTimeNone IntegerTimeFormat = iota
	IntegerTimeUnix
	IntegerTimeUnixMilli
	IntegerTimeUnixMicro
	IntegerTimeUnixNano
)

// Config is a parsed DSN. See docs/PROTOCOL.md §10.
type Config struct {
	// Filename is handed to sqlite3_open_v2 once driver-only parameters have
	// been stripped. For an anonymous in-memory database it is empty and
	// FilenameFor supplies the real name.
	Filename string

	// VFS is the sqlite3_vfs name, or "" for the default (transient) VFS.
	VFS string

	Loc         *time.Location
	TimeFormat  TimeFormat
	IntegerTime IntegerTimeFormat
	ForeignKeys bool
	TxLock      string

	// Memory reports an in-memory database, which always goes through the
	// memdb VFS; see anonymousMemory.
	Memory bool

	// Persistent reports a database backed by a file, which is subject to the
	// one-connection-per-file rule in docs/DESIGN.md §4.10.
	Persistent bool

	// anonymousMemory means the DSN asked for an unnamed in-memory database
	// and FilenameFor has to invent a name for it.
	anonymousMemory bool
}

// FilenameFor returns the filename to open. id identifies the connector and is
// used only for an anonymous in-memory database: this build has
// SQLITE_OMIT_SHARED_CACHE, so `:memory:` would give every pooled connection
// its own invisible database. Naming it and routing it through the memdb VFS is
// what makes a pool see one database.
func (c *Config) FilenameFor(id string) string {
	if c.anonymousMemory {
		return "file:/" + id
	}
	return c.Filename
}

// txBeginStatement returns the BEGIN form for an explicit transaction. The
// default is IMMEDIATE: a deferred transaction takes its write lock lazily, and
// an upgrade mid-transaction cannot be resolved when the busy handler would
// have to block the very worker thread that must deliver the other
// connection's COMMIT.
func (c *Config) txBeginStatement() string {
	switch c.TxLock {
	case "deferred":
		return "BEGIN DEFERRED"
	case "exclusive":
		return "BEGIN EXCLUSIVE"
	default:
		return "BEGIN IMMEDIATE"
	}
}

// ParseDSN parses a driver DSN.
//
//	file:app.db?vfs=opfs&_loc=Asia/Seoul&_fk=0
//	file:/shared?vfs=memdb
//	:memory:
//	app.db
//
// Standard SQLite URI parameters are passed through to sqlite3_open_v2;
// driver parameters are '_'-prefixed and stripped.
func ParseDSN(dsn string) (*Config, error) {
	cfg := &Config{
		Loc:         time.UTC,
		ForeignKeys: true,
		TxLock:      "immediate",
	}

	head, rawQuery, hasQuery := strings.Cut(dsn, "?")

	// A plain relative path may legitimately contain a '?', and it has no
	// parameters — but ":memory:" and an empty path do take them, and folding
	// the query back into the filename there would silently bypass every
	// guarantee below: the driver parameters, the cache= and _busy_timeout
	// rejections, and the memdb rewrite. `:memory:?cache=shared` is the single
	// most common DSN in the ecosystem.
	isURI := strings.HasPrefix(head, "file:")
	takesQuery := isURI || head == ":memory:" || head == ""
	if !takesQuery && hasQuery {
		head = dsn
		rawQuery, hasQuery = "", false
	}

	var q url.Values
	if hasQuery {
		var err error
		if q, err = url.ParseQuery(rawQuery); err != nil {
			return nil, fmt.Errorf("sqlite3-wasm: bad DSN query %q: %w", rawQuery, err)
		}
	} else {
		q = url.Values{}
	}

	for key := range q {
		if !strings.HasPrefix(key, "_") {
			continue
		}
		v := q.Get(key)
		if err := cfg.applyDriverParam(key, v); err != nil {
			return nil, err
		}
		delete(q, key)
	}

	cfg.VFS = q.Get("vfs")

	// Both spellings of shared cache are a silent no-op in this build, and the
	// failure mode is invisible: sqlite3_open_v2 returns rc=0 and then the
	// connections cannot see each other's tables.
	if cache := q.Get("cache"); cache != "" {
		return nil, fmt.Errorf(
			"sqlite3-wasm: cache=%s is not supported (this build has SQLITE_OMIT_SHARED_CACHE, "+
				"so it would be silently ignored); use vfs=memdb for a shared in-memory database", cache)
	}

	isMemory := head == "file::memory:" || head == ":memory:" || q.Get("mode") == "memory"
	switch {
	case cfg.VFS == "memdb":
		// An explicitly named memdb path is already shared between handles.
		cfg.Memory = true
	case isMemory:
		cfg.Memory, cfg.anonymousMemory, cfg.VFS = true, true, "memdb"
		delete(q, "mode")
		delete(q, "vfs")
	default:
		// "" is SQLite's private temporary database, "file:" the same in URI
		// form; neither is a persistent file.
		cfg.Persistent = head != "file:" && head != ""
	}

	if !cfg.anonymousMemory {
		cfg.Filename = head
		if len(q) > 0 {
			cfg.Filename += "?" + q.Encode()
		}
	}
	return cfg, nil
}

func (c *Config) applyDriverParam(key, v string) error {
	switch key {
	case "_loc", "_timezone":
		if strings.EqualFold(v, "auto") {
			// Under GOOS=js time.Local is UTC unless tzdata is embedded.
			c.Loc = time.Local
			return nil
		}
		loc, err := time.LoadLocation(v)
		if err != nil {
			return fmt.Errorf("sqlite3-wasm: %s=%s: %w", key, v, err)
		}
		c.Loc = loc

	case "_time_format":
		switch v {
		case "offset":
			c.TimeFormat = TimeFormatOffset
		case "utc":
			c.TimeFormat = TimeFormatUTC
		case "datetime":
			c.TimeFormat = TimeFormatDatetime
		default:
			return fmt.Errorf("sqlite3-wasm: _time_format=%s: want offset, utc or datetime", v)
		}

	case "_time_integer_format":
		switch v {
		case "unix":
			c.IntegerTime = IntegerTimeUnix
		case "unix_milli":
			c.IntegerTime = IntegerTimeUnixMilli
		case "unix_micro":
			c.IntegerTime = IntegerTimeUnixMicro
		case "unix_nano":
			c.IntegerTime = IntegerTimeUnixNano
		default:
			return fmt.Errorf(
				"sqlite3-wasm: _time_integer_format=%s: want unix, unix_milli, unix_micro or unix_nano", v)
		}

	case "_fk", "_foreign_keys":
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("sqlite3-wasm: %s=%s: %w", key, v, err)
		}
		c.ForeignKeys = b

	case "_txlock":
		switch v {
		case "immediate", "deferred", "exclusive":
			c.TxLock = v
		default:
			return fmt.Errorf("sqlite3-wasm: _txlock=%s: want immediate, deferred or exclusive", v)
		}

	case "_busy_timeout":
		// SQLite's busy handler sleeps with Atomics.wait on the DB worker's own
		// thread, and the COMMIT that would release the lock can only be
		// dequeued when that thread returns to its event loop. A non-zero
		// timeout is therefore a guaranteed self-deadlock for its full
		// duration; SQLITE_BUSY is retried on the Go side instead.
		return fmt.Errorf(
			"sqlite3-wasm: _busy_timeout is not supported: a non-zero busy timeout deadlocks the " +
				"database worker (see docs/DESIGN.md §4.10); SQLITE_BUSY is retried on the Go side")

	default:
		return fmt.Errorf("sqlite3-wasm: unknown driver parameter %q", key)
	}
	return nil
}
