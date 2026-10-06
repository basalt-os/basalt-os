// SPDX-License-Identifier: MIT OR Apache-2.0

package gate

// A copy of the gate's canonical JSON writer (internal/canon), kept here so
// this package imports nothing else from Basalt. The protocol fixtures
// test that both produce the same bytes.

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

// maxSafe is the largest integer canonical JSON carries (2^53 - 1).
const maxSafe = 1<<53 - 1

// errNotCanonical is returned for values outside the canonical subset.
var errNotCanonical = errors.New("value outside the canonical JSON subset")

// canonMarshal returns the canonical form of v. v is first encoded with
// encoding/json (so structs and their tags work) and decoded again with
// numbers kept exact, then written canonically.
func canonMarshal(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canonicalize(raw)
}

// canonicalize rewrites a JSON document in canonical form.
func canonicalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: trailing data", errNotCanonical)
	}
	var b strings.Builder
	if err := canonWrite(&b, v); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

func canonWrite(b *strings.Builder, v any) error {
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
		n, err := canonInt(x)
		if err != nil {
			return err
		}
		b.WriteString(strconv.FormatInt(n, 10))
	case string:
		return canonString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := canonWrite(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			for i := 0; i < len(k); i++ {
				if k[i] >= utf8.RuneSelf {
					return fmt.Errorf("%w: object key %q is not ASCII", errNotCanonical, k)
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
			if err := canonString(b, k); err != nil {
				return err
			}
			b.WriteByte(':')
			if err := canonWrite(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("%w: %T", errNotCanonical, v)
	}
	return nil
}

// canonInt returns a JSON number as an integer within the canonical range.
func canonInt(n json.Number) (int64, error) {
	s := string(n)
	if strings.ContainsAny(s, ".eE") {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || f != math.Trunc(f) || math.Abs(f) > maxSafe {
			return 0, fmt.Errorf("%w: number %s is not an integer", errNotCanonical, s)
		}
		return 0, fmt.Errorf("%w: number %s must be written as an integer", errNotCanonical, s)
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil || i > maxSafe || i < -maxSafe {
		return 0, fmt.Errorf("%w: number %s out of range", errNotCanonical, s)
	}
	return i, nil
}

func canonString(b *strings.Builder, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: invalid UTF-8", errNotCanonical)
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
