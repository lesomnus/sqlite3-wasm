package driver

import (
	"database/sql/driver"
	"testing"
)

func TestSubstituteParams(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		args    []driver.NamedValue
		want    string
		wantErr bool
	}{
		{"question int",
			"SELECT * FROM t WHERE id = ?",
			[]driver.NamedValue{
				{Value: int64(42)},
			},
			"SELECT * FROM t WHERE id = 42",
			false,
		},
		{"question string",
			"INSERT INTO t(x) VALUES (?)",
			[]driver.NamedValue{{Value: "O'Reilly"}},
			"INSERT INTO t(x) VALUES ('O''Reilly')",
			false,
		},
		{"dollar positional",
			"SELECT $1, $2",
			[]driver.NamedValue{
				{Value: "a"},
				{Value: int64(7)},
			},
			"SELECT 'a', 7",
			false,
		},
		{"mixed placeholders",
			"SELECT ?, $2, $1",
			[]driver.NamedValue{
				{Value: int64(1)},
				{Value: "b"},
			},
			"SELECT 1, 'b', 1",
			false,
		},
		{"ordinal fallback",
			"$3",
			[]driver.NamedValue{
				{Ordinal: 3, Value: "x"},
			},
			"'x'",
			false,
		},
		{"bytes",
			"INSERT INTO t(b) VALUES (?)",
			[]driver.NamedValue{
				{Value: []byte{0x01, 0x02}},
			},
			"INSERT INTO t(b) VALUES (x'0102')",
			false,
		},
		{"too few for question",
			"SELECT ?, ?",
			[]driver.NamedValue{
				{Value: 1},
			},
			"",
			true,
		},
		{"unused params error",
			"SELECT 1",
			[]driver.NamedValue{
				{Value: 1},
			},
			"",
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := substituteParams(tc.query, tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (got=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("substituteParams(%q) = %q; want %q", tc.query, got, tc.want)
			}
		})
	}
}
