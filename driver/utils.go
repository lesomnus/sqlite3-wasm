package driver

import (
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

func namedValues(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{
			Ordinal: i + 1,
			Value:   v,
		}
	}
	return named
}

func notWhitespace(sql string) bool {
	const (
		code = iota
		slash
		minus
		ccomment
		sqlcomment
		endcomment
	)

	state := code
	for _, b := range ([]byte)(sql) {
		if b == 0 {
			break
		}

		switch state {
		case code:
			switch b {
			case '/':
				state = slash
			case '-':
				state = minus
			case ' ', ';', '\t', '\n', '\v', '\f', '\r':
				continue
			default:
				return true
			}
		case slash:
			if b != '*' {
				return true
			}
			state = ccomment
		case minus:
			if b != '-' {
				return true
			}
			state = sqlcomment
		case ccomment:
			if b == '*' {
				state = endcomment
			}
		case sqlcomment:
			if b == '\n' {
				state = code
			}
		case endcomment:
			switch b {
			case '/':
				state = code
			case '*':
				state = endcomment
			default:
				state = ccomment
			}
		}
	}
	return state == slash || state == minus
}

// substituteParams performs a very small and unsafe parameter substitution
// for '?' and '$N' placeholders. It is intended only as a development shim
// until the worker provides a proper prepare/bind API. WARNING: this is
// susceptible to SQL injection if user-provided values are inserted into
// queries without proper validation. Use with caution.
func substituteParams(query string, args []driver.NamedValue) (string, error) {
	var b strings.Builder
	argIdx := 0
	placeholdersFound := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '?' {
			if argIdx >= len(args) {
				return "", errors.New("not enough parameters for '?' placeholder")
			}
			b.WriteString(escapeValue(args[argIdx].Value))
			argIdx++
			placeholdersFound = true
			continue
		}
		if ch == '$' {
			// parse number after $
			j := i + 1
			for j < len(query) && query[j] >= '0' && query[j] <= '9' {
				j++
			}
			if j == i+1 {
				// no digits, treat as literal $
				b.WriteByte(ch)
				continue
			}
			nstr := query[i+1 : j]
			n, err := strconv.Atoi(nstr)
			if err != nil {
				return "", fmt.Errorf("invalid $ placeholder: %w", err)
			}
			// Prefer positional index if available
			if n-1 < len(args) {
				b.WriteString(escapeValue(args[n-1].Value))
				placeholdersFound = true
			} else {
				// fallback: search for ordinal
				found := false
				for _, a := range args {
					if a.Ordinal == n {
						b.WriteString(escapeValue(a.Value))
						found = true
						break
					}
				}
				if !found {
					return "", fmt.Errorf("no parameter for $%d", n)
				}
				placeholdersFound = true
			}
			i = j - 1
			continue
		}
		b.WriteByte(ch)
	}

	// If no placeholders were present in the query but args were supplied,
	// treat that as an error (likely accidental). If placeholders were used
	// (including $N positional/ordinal), extra args are allowed and ignored.
	if !placeholdersFound && len(args) > 0 {
		return "", fmt.Errorf("too many parameters: %d unused", len(args))
	}

	return b.String(), nil
}

func escapeValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case int:
		return strconv.FormatInt(int64(t), 10)
	case int8:
		return strconv.FormatInt(int64(t), 10)
	case int16:
		return strconv.FormatInt(int64(t), 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint:
		return strconv.FormatUint(uint64(t), 10)
	case uint8:
		return strconv.FormatUint(uint64(t), 10)
	case uint16:
		return strconv.FormatUint(uint64(t), 10)
	case uint32:
		return strconv.FormatUint(uint64(t), 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		if t {
			return "1"
		}
		return "0"
	case string:
		s := strings.ReplaceAll(t, "'", "''")
		return "'" + s + "'"
	case []byte:
		return "x'" + hex.EncodeToString(t) + "'"
	default:
		s := strings.ReplaceAll(fmt.Sprintf("%v", t), "'", "''")
		return "'" + s + "'"
	}
}
