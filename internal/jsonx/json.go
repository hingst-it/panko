// Package jsonx preserves JSON numbers and the canonical encoding used by Panko's Python prototype.
package jsonx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

type Object = map[string]any

func Decode(r io.Reader, value any) error {
	d := json.NewDecoder(r)
	d.UseNumber()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func Encode(v any) []byte {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func Clone(v Object) Object {
	var result Object
	if err := Decode(bytes.NewReader(Encode(v)), &result); err != nil {
		panic(err)
	}
	return result
}
func Obj(v any) Object  { result, _ := v.(map[string]any); return result }
func Array(v any) []any { result, _ := v.([]any); return result }
func Str(v any) string  { result, _ := v.(string); return result }
func Int(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// Canonical matches json.dumps(sort_keys=True, separators=(',', ':'), ensure_ascii=True).
// Integer tokens retain arbitrary precision; floating-point tokens follow Python's repr thresholds.
func Canonical(v any) ([]byte, error) {
	var b bytes.Buffer
	var write func(any) error
	write = func(v any) error {
		switch x := v.(type) {
		case nil:
			b.WriteString("null")
		case bool:
			b.WriteString(strconv.FormatBool(x))
		case string:
			b.WriteByte('"')
			for _, r := range x {
				switch r {
				case '"', '\\':
					b.WriteByte('\\')
					b.WriteRune(r)
				case '\b':
					b.WriteString(`\b`)
				case '\f':
					b.WriteString(`\f`)
				case '\n':
					b.WriteString(`\n`)
				case '\r':
					b.WriteString(`\r`)
				case '\t':
					b.WriteString(`\t`)
				default:
					if r < 32 || r >= 127 {
						if r > 0xffff {
							a, c := utf16.EncodeRune(r)
							fmt.Fprintf(&b, `\u%04x\u%04x`, a, c)
						} else {
							fmt.Fprintf(&b, `\u%04x`, r)
						}
					} else {
						b.WriteRune(r)
					}
				}
			}
			b.WriteByte('"')
		case json.Number:
			s := string(x)
			if !strings.ContainsAny(s, ".eE") {
				n, ok := new(big.Int).SetString(s, 10)
				if !ok {
					return fmt.Errorf("invalid integer")
				}
				b.WriteString(n.String())
			} else {
				f, err := strconv.ParseFloat(s, 64)
				if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
					return fmt.Errorf("invalid number")
				}
				scientific := strconv.FormatFloat(f, 'e', -1, 64)
				parts := strings.Split(scientific, "e")
				exponent, _ := strconv.Atoi(parts[1])
				if exponent >= -4 && exponent < 16 {
					s = strconv.FormatFloat(f, 'f', -1, 64)
					if !strings.Contains(s, ".") {
						s += ".0"
					}
				} else {
					s = scientific
				}
				b.WriteString(s)
			}
		case int:
			b.WriteString(strconv.Itoa(x))
		case int64:
			b.WriteString(strconv.FormatInt(x, 10))
		case []any:
			b.WriteByte('[')
			for i, item := range x {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := write(item); err != nil {
					return err
				}
			}
			b.WriteByte(']')
		case map[string]any:
			keys := make([]string, 0, len(x))
			for key := range x {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			b.WriteByte('{')
			for i, key := range keys {
				if i > 0 {
					b.WriteByte(',')
				}
				_ = write(key)
				b.WriteByte(':')
				if err := write(x[key]); err != nil {
					return err
				}
			}
			b.WriteByte('}')
		default:
			return fmt.Errorf("unsupported canonical JSON type %T", v)
		}
		return nil
	}
	if err := write(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func Digest(spec any, generation int64) (string, error) {
	b, err := Canonical(Object{"spec": spec, "generation": generation})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
