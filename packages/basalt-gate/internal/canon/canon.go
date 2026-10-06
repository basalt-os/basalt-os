// Package canon writes the canonical JSON form the gate hashes (the
// proposal digest, rule hashes). It is the subset of RFC 8785 (JSON
// Canonicalization Scheme) that gate documents use, so a client in any
// language can compute the same bytes:
//
//   - objects: members sorted by key (keys are ASCII, so byte order and
//     the UTF-16 order of RFC 8785 agree), no duplicate keys;
//   - arrays in their order; no whitespace anywhere;
//   - strings: UTF-8; only '"', '\\' and control characters below U+0020
//     are escaped (\b \t \n \f \r, others as \u00xx in lower case);
//     everything else, including '<', '>', '&', U+2028 and U+2029, is
//     written as is;
//   - numbers: integers only, between -(2^53-1) and 2^53-1, in plain
//     decimal; fractions and exponents are refused;
//   - true, false, null.
package canon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxSafe is the largest integer canonical JSON carries (2^53 - 1).
const MaxSafe = 1<<53 - 1

// ErrNotCanonical is returned for values outside the canonical subset.
var ErrNotCanonical = errors.New("value outside the canonical JSON subset")

// Marshal returns the canonical form of v. v is first encoded with
// encoding/json (so structs and their tags work) and decoded again with
// numbers kept exact, then written canonically.
func Marshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Canonicalize(raw)
}

// Canonicalize rewrites a JSON document in canonical form.
func Canonicalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", ErrNotCanonical)
	}
	var b strings.Builder
	if err := write(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func write(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case json.Number:
		n, err := Int(x)
		if err != nil {
			return err
		}
		b.WriteString(strconv.FormatInt(n, 10))
	case string:
		return writeString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			for i := 0; i < len(k); i++ {
				if k[i] >= utf8.RuneSelf {
					return fmt.Errorf("%w: object key %q is not ASCII", ErrNotCanonical, k)
				}
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := write(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("%w: %T", ErrNotCanonical, v)
	}
	return nil
}

// Int returns a JSON number as an integer within the canonical range.
func Int(n json.Number) (int64, error) {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != math.Trunc(f) || math.Abs(f) > MaxSafe {
			return 0, fmt.Errorf("%w: number %s is not an integer", ErrNotCanonical, s)
		}
		return 0, fmt.Errorf("%w: number %s must be written as an integer", ErrNotCanonical, s)
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil || i > MaxSafe || i < -MaxSafe {
		return 0, fmt.Errorf("%w: number %s out of range", ErrNotCanonical, s)
	}
	return i, nil
}

func writeString(b *strings.Builder, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: invalid UTF-8", ErrNotCanonical)
	}
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 {
				fmt.Fprintf(b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return nil
}
