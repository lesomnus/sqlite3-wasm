// Package wire implements the binary protocol spoken between the Go worker and
// the SQLite DB worker. See docs/PROTOCOL.md, which this package is the
// normative Go half of.
//
// It carries no build tag on purpose: the codec is plain byte manipulation, so
// `go test ./...` exercises it on the host without a browser.
package wire

// Version is the protocol version carried in every frame header. Any change to
// the opcodes, payloads or flags in docs/PROTOCOL.md must bump it.
const Version = 1

// HeaderSize is the fixed size of a frame header in bytes.
const HeaderSize = 8

// Op identifies a frame's kind. Requests are < 0x80, responses >= 0x80.
type Op uint8

const (
	OpOpen      Op = 0x01
	OpClose     Op = 0x02
	OpPrepare   Op = 0x03
	OpFinalize  Op = 0x04
	OpQuery     Op = 0x05
	OpQueryStmt Op = 0x06
	OpExec      Op = 0x07
	OpExecStmt  Op = 0x08
	OpCredit    Op = 0x09
	OpAbort     Op = 0x0A
	OpShutdown  Op = 0x0B

	OpReady      Op = 0x80
	OpOK         Op = 0x81
	OpError      Op = 0x82
	OpOpened     Op = 0x83
	OpPrepared   Op = 0x84
	OpRows       Op = 0x85
	OpExecResult Op = 0x86
	OpAborted    Op = 0x87
)

func (o Op) String() string {
	switch o {
	case OpOpen:
		return "OPEN"
	case OpClose:
		return "CLOSE"
	case OpPrepare:
		return "PREPARE"
	case OpFinalize:
		return "FINALIZE"
	case OpQuery:
		return "QUERY"
	case OpQueryStmt:
		return "QUERY_STMT"
	case OpExec:
		return "EXEC"
	case OpExecStmt:
		return "EXEC_STMT"
	case OpCredit:
		return "CREDIT"
	case OpAbort:
		return "ABORT"
	case OpShutdown:
		return "SHUTDOWN"
	case OpReady:
		return "READY"
	case OpOK:
		return "OK"
	case OpError:
		return "ERROR"
	case OpOpened:
		return "OPENED"
	case OpPrepared:
		return "PREPARED"
	case OpRows:
		return "ROWS"
	case OpExecResult:
		return "EXEC_RESULT"
	case OpAborted:
		return "ABORTED"
	}
	return "Op(" + itoa(int(o)) + ")"
}

// Frame flags.
const (
	// FlagEOF marks the last frame for a request id.
	FlagEOF uint16 = 1 << 0
	// FlagHasColumns marks a ROWS frame that is prefixed with a columns block.
	FlagHasColumns uint16 = 1 << 1
)

// Tag identifies the storage class of an encoded value. It mirrors SQLite's
// storage classes rather than its SQLITE_* constants, so that the wire format
// does not depend on them.
type Tag uint8

const (
	TagNull Tag = 0x00
	TagInt  Tag = 0x01
	TagReal Tag = 0x02
	TagText Tag = 0x03
	TagBlob Tag = 0x04
)

func (t Tag) String() string {
	switch t {
	case TagNull:
		return "NULL"
	case TagInt:
		return "INT"
	case TagReal:
		return "REAL"
	case TagText:
		return "TEXT"
	case TagBlob:
		return "BLOB"
	}
	return "Tag(" + itoa(int(t)) + ")"
}

// absentLen marks a nullable string as absent. It is distinct from length 0,
// which is a present empty string.
const absentLen = 0xFFFFFFFF

// Capability bits reported in the READY frame.
const (
	CapCrossOriginIsolated uint32 = 1 << 0
	CapSharedArrayBuffer   uint32 = 1 << 1
	CapBigInt              uint32 = 1 << 2
	CapProgressHandler     uint32 = 1 << 3
	CapVFSOpfs             uint32 = 1 << 4
	CapVFSOpfsSAHPool      uint32 = 1 << 5
	CapVFSMemdb            uint32 = 1 << 6
)

// Header is the fixed 8-byte prefix of every frame.
type Header struct {
	Version uint8
	Op      Op
	Flags   uint16
	ID      uint32
}

func (h Header) EOF() bool        { return h.Flags&FlagEOF != 0 }
func (h Header) HasColumns() bool { return h.Flags&FlagHasColumns != 0 }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
