package policy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ParseTOML parses the subset of TOML that rule files use into a map:
//
//   - comments (#) and blank lines;
//   - [table] and [[array-of-tables]] headers with one bare key;
//   - key = value lines with bare keys ([A-Za-z0-9_-]);
//   - values: basic strings ("..." with \" \\ \n \t \r \uXXXX), literal
//     strings ('...'), integers, booleans, arrays (over several lines,
//     trailing comma allowed) and inline tables ({ k = v, ... }).
//
// Anything else (dotted keys, dates, floats, multi-line strings) is an
// error with its line number: a rule file never means something other
// than what it says.
func ParseTOML(src string) (map[string]any, error) {
	p := &tparser{s: src, line: 1}
	root := map[string]any{}
	cur := root
	for {
		p.skipBlank()
		if p.eof() {
			return root, nil
		}
		switch {
		case strings.HasPrefix(p.rest(), "[["):
			p.i += 2
			name, err := p.bareKey()
			if err != nil {
				return nil, err
			}
			if !strings.HasPrefix(p.rest(), "]]") {
				return nil, p.errf("expected ]]")
			}
			p.i += 2
			t := map[string]any{}
			switch x := root[name].(type) {
			case nil:
				root[name] = []any{t}
			case []any:
				root[name] = append(x, t)
			default:
				return nil, p.errf("%s is not an array of tables", name)
			}
			cur = t
		case p.peek() == '[':
			p.i++
			name, err := p.bareKey()
			if err != nil {
				return nil, err
			}
			if p.peek() != ']' {
				return nil, p.errf("expected ]")
			}
			p.i++
			if _, dup := root[name]; dup {
				return nil, p.errf("table %s defined twice", name)
			}
			t := map[string]any{}
			root[name] = t
			cur = t
		default:
			if err := p.keyValue(cur); err != nil {
				return nil, err
			}
		}
		if err := p.endOfLine(); err != nil {
			return nil, err
		}
	}
}

type tparser struct {
	s    string
	i    int
	line int
}

func (p *tparser) eof() bool    { return p.i >= len(p.s) }
func (p *tparser) rest() string { return p.s[p.i:] }
func (p *tparser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}

func (p *tparser) errf(format string, a ...any) error {
	return fmt.Errorf("line %d: %s", p.line, fmt.Sprintf(format, a...))
}

// skipBlank skips spaces, newlines and comments.
func (p *tparser) skipBlank() {
	for !p.eof() {
		switch c := p.peek(); {
		case c == ' ' || c == '\t' || c == '\r':
			p.i++
		case c == '\n':
			p.line++
			p.i++
		case c == '#':
			for !p.eof() && p.peek() != '\n' {
				p.i++
			}
		default:
			return
		}
	}
}

func (p *tparser) skipSpace() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.i++
	}
}

func (p *tparser) endOfLine() error {
	p.skipSpace()
	if p.peek() == '#' {
		for !p.eof() && p.peek() != '\n' {
			p.i++
		}
	}
	if p.peek() == '\r' {
		p.i++
	}
	if p.eof() {
		return nil
	}
	if p.peek() != '\n' {
		return p.errf("unexpected %q after the value", p.peek())
	}
	return nil
}

func isBare(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (p *tparser) bareKey() (string, error) {
	p.skipSpace()
	start := p.i
	for !p.eof() && isBare(p.peek()) {
		p.i++
	}
	if p.i == start {
		return "", p.errf("expected a key")
	}
	k := p.s[start:p.i]
	p.skipSpace()
	if p.peek() == '.' {
		return "", p.errf("dotted keys are not supported")
	}
	return k, nil
}

func (p *tparser) keyValue(t map[string]any) error {
	k, err := p.bareKey()
	if err != nil {
		return err
	}
	if p.peek() != '=' {
		return p.errf("expected = after %s", k)
	}
	p.i++
	p.skipSpace()
	v, err := p.value(0)
	if err != nil {
		return err
	}
	if _, dup := t[k]; dup {
		return p.errf("key %s defined twice", k)
	}
	t[k] = v
	return nil
}

func (p *tparser) value(depth int) (any, error) {
	if depth > 8 {
		return nil, p.errf("nested too deep")
	}
	switch c := p.peek(); {
	case c == '"':
		return p.basicString()
	case c == '\'':
		p.i++
		end := strings.IndexAny(p.rest(), "'\n")
		if end < 0 || p.s[p.i+end] != '\'' {
			return nil, p.errf("unterminated string")
		}
		s := p.s[p.i : p.i+end]
		p.i += end + 1
		return s, nil
	case c == '[':
		p.i++
		var out []any
		for {
			p.skipBlank()
			if p.peek() == ']' {
				p.i++
				return out, nil
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
			p.skipBlank()
			switch p.peek() {
			case ',':
				p.i++
			case ']':
			default:
				return nil, p.errf("expected , or ] in an array")
			}
		}
	case c == '{':
		p.i++
		t := map[string]any{}
		p.skipSpace()
		if p.peek() == '}' {
			p.i++
			return t, nil
		}
		for {
			if err := p.keyValue(t); err != nil {
				return nil, err
			}
			p.skipSpace()
			switch p.peek() {
			case ',':
				p.i++
			case '}':
				p.i++
				return t, nil
			default:
				return nil, p.errf("expected , or } in an inline table")
			}
		}
	case strings.HasPrefix(p.rest(), "true"):
		p.i += 4
		return true, nil
	case strings.HasPrefix(p.rest(), "false"):
		p.i += 5
		return false, nil
	case c == '-' || c == '+' || (c >= '0' && c <= '9'):
		start := p.i
		p.i++
		for !p.eof() && (p.peek() >= '0' && p.peek() <= '9' || p.peek() == '_') {
			p.i++
		}
		lit := strings.ReplaceAll(p.s[start:p.i], "_", "")
		if !p.eof() && strings.ContainsRune(".eE:T", rune(p.peek())) {
			return nil, p.errf("only integers are supported")
		}
		n, err := strconv.ParseInt(lit, 10, 64)
		if err != nil {
			return nil, p.errf("bad integer %q", lit)
		}
		return n, nil
	}
	return nil, p.errf("unexpected value")
}

func (p *tparser) basicString() (string, error) {
	p.i++ // opening quote
	if strings.HasPrefix(p.rest(), `""`) {
		return "", p.errf("multi-line strings are not supported")
	}
	var b strings.Builder
	for {
		if p.eof() {
			return "", p.errf("unterminated string")
		}
		c := p.peek()
		switch c {
		case '"':
			p.i++
			return b.String(), nil
		case '\n':
			return "", p.errf("unterminated string")
		case '\\':
			p.i++
			e := p.peek()
			p.i++
			switch e {
			case '"', '\\':
				b.WriteByte(e)
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'u':
				if p.i+4 > len(p.s) {
					return "", p.errf("bad \\u escape")
				}
				n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
				if err != nil {
					return "", p.errf("bad \\u escape")
				}
				b.WriteRune(rune(n))
				p.i += 4
			default:
				return "", p.errf("unsupported escape \\%c", e)
			}
		default:
			r, size := utf8.DecodeRuneInString(p.rest())
			if r == utf8.RuneError && size == 1 {
				return "", errors.New("invalid UTF-8")
			}
			b.WriteRune(r)
			p.i += size
		}
	}
}
