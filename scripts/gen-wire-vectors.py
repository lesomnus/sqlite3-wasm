#!/usr/bin/env python3
"""Generate the golden wire-format corpus.

This is a deliberately *independent* third implementation of docs/PROTOCOL.md.
If the corpus were produced by either the Go or the TypeScript codec, a bug in
that codec would simply be blessed as correct and the other side would be made
to match it. Written in Python, agreement between all three is real evidence.

Run:  python3 scripts/gen-wire-vectors.py
Out:  testdata/wire/vectors.json
"""

import json
import struct
import os

VERSION = 1
ABSENT = 0xFFFFFFFF

OPS = {
    "OPEN": 0x01, "CLOSE": 0x02, "PREPARE": 0x03, "FINALIZE": 0x04,
    "QUERY": 0x05, "QUERY_STMT": 0x06, "EXEC": 0x07, "EXEC_STMT": 0x08,
    "CREDIT": 0x09, "ABORT": 0x0A, "SHUTDOWN": 0x0B,
    "READY": 0x80, "OK": 0x81, "ERROR": 0x82, "OPENED": 0x83,
    "PREPARED": 0x84, "ROWS": 0x85, "EXEC_RESULT": 0x86, "ABORTED": 0x87,
}

FLAG_EOF = 1 << 0
FLAG_HAS_COLUMNS = 1 << 1

TAG_NULL, TAG_INT, TAG_REAL, TAG_TEXT, TAG_BLOB = 0, 1, 2, 3, 4


# The canonical quiet NaN. CPython's float("nan") carries a payload bit that Go
# and JS do not produce, and JS typed arrays are permitted to canonicalise NaN
# anyway, so NaN payload bits are explicitly not part of the wire contract:
# every NaN on the wire is this one.
CANONICAL_NAN = struct.unpack("<d", bytes.fromhex("000000000000f87f"))[0]


def parse_f64(s):
    if s == "NaN":
        return CANONICAL_NAN
    if s == "Infinity":
        return float("inf")
    if s == "-Infinity":
        return float("-inf")
    return float(s)


def encode(op_name, flags, ident, ops):
    """Encode a frame from the language-neutral op list."""
    out = bytearray()
    out += struct.pack("<BBHI", VERSION, OPS[op_name], flags, ident)

    def u32(v):
        out.extend(struct.pack("<I", v))

    def blen(b):
        u32(len(b))
        out.extend(b)

    for item in ops:
        kind, arg = item[0], (item[1] if len(item) > 1 else None)
        if kind == "u8":
            out.extend(struct.pack("<B", arg))
        elif kind == "bool":
            out.extend(struct.pack("<B", 1 if arg else 0))
        elif kind == "u16":
            out.extend(struct.pack("<H", arg))
        elif kind == "u32":
            u32(arg)
        elif kind == "i32":
            out.extend(struct.pack("<i", arg))
        elif kind == "i64":
            out.extend(struct.pack("<q", int(arg)))
        elif kind == "f64":
            out.extend(struct.pack("<d", parse_f64(arg)))
        elif kind == "str":
            blen(arg.encode("utf-8"))
        elif kind == "nstr":
            if arg is None:
                u32(ABSENT)
            else:
                blen(arg.encode("utf-8"))
        elif kind == "bytes":
            blen(bytes.fromhex(arg))
        elif kind == "vnull":
            out.extend(struct.pack("<B", TAG_NULL))
        elif kind == "vint":
            out.extend(struct.pack("<B", TAG_INT))
            out.extend(struct.pack("<q", int(arg)))
        elif kind == "vreal":
            out.extend(struct.pack("<B", TAG_REAL))
            out.extend(struct.pack("<d", parse_f64(arg)))
        elif kind == "vtext":
            out.extend(struct.pack("<B", TAG_TEXT))
            blen(arg.encode("utf-8"))
        elif kind == "vtextb":
            out.extend(struct.pack("<B", TAG_TEXT))
            blen(bytes.fromhex(arg))
        elif kind == "vblob":
            out.extend(struct.pack("<B", TAG_BLOB))
            blen(bytes.fromhex(arg))
        else:
            raise SystemExit("unknown op kind: " + kind)
    return bytes(out)


# --- the corpus ------------------------------------------------------------
#
# Every entry names what it pins down. Prefer one property per vector so a
# failure points somewhere.

V = []


def add(name, op, flags, ident, ops):
    V.append({"name": name, "op": op, "flags": flags, "id": ident, "ops": ops})


# Headers -------------------------------------------------------------------
add("header/empty-payload", "SHUTDOWN", 0, 1, [])
add("header/id-zero", "READY", 0, 0, [])
add("header/id-max", "OK", 0, 0xFFFFFFFF, [])
add("header/flag-eof", "ROWS", FLAG_EOF, 7, [["u32", 0]])
add("header/flag-both", "ROWS", FLAG_EOF | FLAG_HAS_COLUMNS, 7,
    [["u32", 0], ["u32", 0]])

# Scalars -------------------------------------------------------------------
add("scalar/u8-bounds", "OK", 0, 1, [["u8", 0], ["u8", 1], ["u8", 255]])
add("scalar/u16-bounds", "OK", 0, 1, [["u16", 0], ["u16", 1], ["u16", 65535]])
add("scalar/u32-bounds", "OK", 0, 1,
    [["u32", 0], ["u32", 1], ["u32", 0xFFFFFFFE], ["u32", 0xFFFFFFFF]])
add("scalar/i32-negative", "OK", 0, 1,
    [["i32", 0], ["i32", -1], ["i32", 2147483647], ["i32", -2147483648]])
add("scalar/bool", "OK", 0, 1, [["bool", False], ["bool", True]])

# int64 is the reason this rewrite exists: everything past 2^53 is where the
# old worker1 path turned values into BigInt and panicked the Go runtime.
add("scalar/i64-edges", "OK", 0, 1, [
    ["i64", "0"], ["i64", "1"], ["i64", "-1"],
    ["i64", "9007199254740991"],    # 2^53-1, last exactly-representable double
    ["i64", "9007199254740992"],    # 2^53
    ["i64", "9007199254740993"],    # 2^53+1, not representable as a double
    ["i64", "-9007199254740993"],
    ["i64", "9223372036854775807"],  # max int64
    ["i64", "-9223372036854775808"],  # min int64
])

add("scalar/f64-edges", "OK", 0, 1, [
    ["f64", "0"], ["f64", "-0"], ["f64", "1"], ["f64", "-1"],
    ["f64", "0.5"], ["f64", "0.1"],
    ["f64", "1.7976931348623157e308"],   # max normal
    ["f64", "5e-324"],                    # min subnormal
    ["f64", "NaN"], ["f64", "Infinity"], ["f64", "-Infinity"],
])

# Strings -------------------------------------------------------------------
add("string/empty", "OK", 0, 1, [["str", ""]])
add("string/ascii", "OK", 0, 1, [["str", "hello"]])
add("string/utf8-multibyte", "OK", 0, 1, [["str", "héllo☃"]])
add("string/utf8-astral", "OK", 0, 1, [["str", "\U0001F600\U0001F1F0\U0001F1F7"]])
add("string/embedded-nul", "OK", 0, 1, [["str", "a\x00b"]])
add("string/nullable", "OK", 0, 1,
    [["nstr", None], ["nstr", ""], ["nstr", "INTEGER"]])

add("bytes/empty", "OK", 0, 1, [["bytes", ""]])
add("bytes/short", "OK", 0, 1, [["bytes", "00ff7f80"]])

# Values --------------------------------------------------------------------
add("value/null", "ROWS", FLAG_EOF, 1, [["u32", 1], ["vnull"]])
add("value/int-edges", "ROWS", FLAG_EOF, 1, [
    ["u32", 3],
    ["vint", "9007199254740993"],
    ["vint", "-9223372036854775808"],
    ["vint", "0"],
])
add("value/real-edges", "ROWS", FLAG_EOF, 1, [
    ["u32", 3], ["vreal", "-0"], ["vreal", "NaN"], ["vreal", "1e308"],
])
# NULL, '' and x'' are indistinguishable by pointer+length inside sqlite3; only
# sqlite3_column_type separates them, so the tag has to carry that distinction.
add("value/empty-vs-null", "ROWS", FLAG_EOF, 1, [
    ["u32", 4], ["vnull"], ["vtext", ""], ["vblob", ""], ["vnull"],
])
add("value/text-nul", "ROWS", FLAG_EOF, 1, [["u32", 1], ["vtext", "a\x00b"]])
# SQLite does not guarantee TEXT is valid UTF-8; the codec must pass bytes through.
add("value/text-invalid-utf8", "ROWS", FLAG_EOF, 1,
    [["u32", 1], ["vtextb", "61ff62"]])
add("value/blob-bytes", "ROWS", FLAG_EOF, 1,
    [["u32", 1], ["vblob", "deadbeef00"]])

# Realistic frames ----------------------------------------------------------
add("frame/open", "OPEN", 0, 3, [
    ["str", "file:app.db"], ["str", "opfs"], ["i32", 6],
])
add("frame/opened", "OPENED", 0, 3, [["u32", 1], ["u32", 0], ["bool", True]])
add("frame/error", "ERROR", FLAG_EOF, 9, [
    ["i32", 19], ["i32", 2067], ["i32", 7], ["str", "UNIQUE constraint failed"],
])
add("frame/error-no-offset", "ERROR", FLAG_EOF, 9, [
    ["i32", 1], ["i32", 1], ["i32", -1], ["str", "no such table: t"],
])
add("frame/exec-result", "EXEC_RESULT", FLAG_EOF, 4, [
    ["i64", "3"], ["i64", "9007199254740993"],
])
add("frame/credit", "CREDIT", 0, 11, [["u32", 4]])
add("frame/abort", "ABORT", 0, 11, [])

add("frame/prepared", "PREPARED", FLAG_EOF, 5, [
    ["u32", 12],       # stmtId
    ["u32", 2],        # paramCount
    ["bool", True],    # paramCountExact
    ["u32", 41],       # tailOffset
    ["bool", False],   # readOnly
    ["u32", 3],        # columns
    ["str", "id"], ["nstr", "INTEGER"],
    ["str", "name"], ["nstr", "TEXT"],
    ["str", "total"], ["nstr", None],   # an expression column has no decltype
])

add("frame/rows-first-batch", "ROWS", FLAG_HAS_COLUMNS, 5, [
    ["u32", 4],
    ["str", "id"], ["nstr", "INTEGER"],
    ["str", "name"], ["nstr", "TEXT"],
    ["str", "score"], ["nstr", "REAL"],
    ["str", "created_at"], ["nstr", "DATETIME"],
    ["u32", 2],
    ["vint", "1"], ["vtext", "Alice"], ["vreal", "1"],
    ["vtext", "2024-01-02 15:04:05.123456789+09:00"],
    ["vint", "2"], ["vnull"], ["vreal", "-0"], ["vint", "1704173045"],
])

add("frame/rows-continuation", "ROWS", FLAG_EOF, 5, [
    ["u32", 1],
    ["vint", "9007199254740993"], ["vblob", "00"], ["vnull"], ["vtext", ""],
])

add("frame/rows-empty-result", "ROWS", FLAG_HAS_COLUMNS | FLAG_EOF, 5, [
    ["u32", 1], ["str", "n"], ["nstr", None], ["u32", 0],
])

add("frame/query-args", "QUERY", 0, 6, [
    ["u32", 1],                     # dbId
    ["str", "SELECT * FROM t WHERE a = ? AND b = :name"],
    ["u32", 2],                     # args
    ["nstr", None], ["u32", 1], ["vint", "42"],
    ["nstr", "name"], ["u32", 2], ["vtext", "x"],
])

add("frame/ready", "READY", 0, 0, [
    ["u16", 1],
    ["str", "3.50.4"],
    ["u32", 0b1111111],
    ["u32", 3], ["str", "opfs"], ["str", "memdb"], ["str", "unix"],
])


def main():
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    out_dir = os.path.join(root, "testdata", "wire")
    os.makedirs(out_dir, exist_ok=True)

    seen = set()
    for v in V:
        if v["name"] in seen:
            raise SystemExit("duplicate vector name: " + v["name"])
        seen.add(v["name"])
        v["hex"] = encode(v["op"], v["flags"], v["id"], v["ops"]).hex()

    doc = {
        "_": "GENERATED by scripts/gen-wire-vectors.py - do not edit by hand.",
        "protocolVersion": VERSION,
        "vectors": V,
    }
    path = os.path.join(out_dir, "vectors.json")
    with open(path, "w", encoding="utf-8") as f:
        json.dump(doc, f, indent=1, ensure_ascii=False)
        f.write("\n")
    print("wrote %d vectors to %s" % (len(V), path))


if __name__ == "__main__":
    main()
