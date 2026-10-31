// Package strictjson performs the lexical validation pass required before
// ordinary typed decoding: valid UTF-8, no unpaired escaped surrogates, no
// duplicate decoded property names, bounded nesting, and nothing after the
// value.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Error describes a lexical rejection.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func fail(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

type scanner struct {
	b        []byte
	i        int
	maxDepth int
}

// Validate checks data against the strict rules. maxDepth counts nested
// containers (objects and arrays); the top level value is depth 1.
func Validate(data []byte, maxDepth int) error {
	if !utf8.Valid(data) {
		return fail("request body is not valid UTF-8")
	}
	s := &scanner{b: data, maxDepth: maxDepth}
	s.ws()
	if err := s.value(0); err != nil {
		return err
	}
	s.ws()
	if s.i != len(s.b) {
		return fail("unexpected data after the JSON value")
	}
	return nil
}

// ValidateObject validates and additionally requires a top level object.
func ValidateObject(data []byte, maxDepth int) error {
	if err := Validate(data, maxDepth); err != nil {
		return err
	}
	t := bytes.TrimLeft(data, " \t\r\n")
	if len(t) == 0 || t[0] != '{' {
		return fail("request body must be a JSON object")
	}
	return nil
}

func (s *scanner) ws() {
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ' ', '\t', '\r', '\n':
			s.i++
		default:
			return
		}
	}
}

func (s *scanner) value(depth int) error {
	if s.i >= len(s.b) {
		return fail("unexpected end of JSON")
	}
	c := s.b[s.i]
	switch {
	case c == '{':
		return s.object(depth + 1)
	case c == '[':
		return s.array(depth + 1)
	case c == '"':
		_, err := s.str()
		return err
	case c == 't':
		return s.lit("true")
	case c == 'f':
		return s.lit("false")
	case c == 'n':
		return s.lit("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return s.number()
	default:
		return fail("unexpected character %q at offset %d", c, s.i)
	}
}

func (s *scanner) lit(w string) error {
	if len(s.b)-s.i < len(w) || string(s.b[s.i:s.i+len(w)]) != w {
		return fail("invalid literal at offset %d", s.i)
	}
	s.i += len(w)
	return nil
}

func (s *scanner) digits() int {
	n := 0
	for s.i < len(s.b) && s.b[s.i] >= '0' && s.b[s.i] <= '9' {
		s.i++
		n++
	}
	return n
}

func (s *scanner) number() error {
	start := s.i
	if s.b[s.i] == '-' {
		s.i++
	}
	if s.i >= len(s.b) {
		return fail("invalid number at offset %d", start)
	}
	if s.b[s.i] == '0' {
		s.i++
	} else if s.b[s.i] >= '1' && s.b[s.i] <= '9' {
		s.digits()
	} else {
		return fail("invalid number at offset %d", start)
	}
	if s.i < len(s.b) && s.b[s.i] == '.' {
		s.i++
		if s.digits() == 0 {
			return fail("invalid number at offset %d", start)
		}
	}
	if s.i < len(s.b) && (s.b[s.i] == 'e' || s.b[s.i] == 'E') {
		s.i++
		if s.i < len(s.b) && (s.b[s.i] == '+' || s.b[s.i] == '-') {
			s.i++
		}
		if s.digits() == 0 {
			return fail("invalid number at offset %d", start)
		}
	}
	return nil
}

func hexVal(b []byte) (int, bool) {
	v := 0
	for _, c := range b {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= int(c - '0')
		case c >= 'a' && c <= 'f':
			v |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// str scans a string and returns its decoded value.
func (s *scanner) str() (string, error) {
	start := s.i
	s.i++ // opening quote
	var out []byte
	for {
		if s.i >= len(s.b) {
			return "", fail("unterminated string starting at offset %d", start)
		}
		c := s.b[s.i]
		switch {
		case c == '"':
			s.i++
			return string(out), nil
		case c == '\\':
			s.i++
			if s.i >= len(s.b) {
				return "", fail("unterminated escape at offset %d", s.i)
			}
			e := s.b[s.i]
			s.i++
			switch e {
			case '"', '\\', '/':
				out = append(out, e)
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				if len(s.b)-s.i < 4 {
					return "", fail("truncated unicode escape at offset %d", s.i)
				}
				v, ok := hexVal(s.b[s.i : s.i+4])
				if !ok {
					return "", fail("invalid unicode escape at offset %d", s.i)
				}
				s.i += 4
				if v >= 0xD800 && v <= 0xDBFF {
					if len(s.b)-s.i < 6 || s.b[s.i] != '\\' || s.b[s.i+1] != 'u' {
						return "", fail("unpaired high surrogate escape at offset %d", s.i-6)
					}
					lo, ok := hexVal(s.b[s.i+2 : s.i+6])
					if !ok || lo < 0xDC00 || lo > 0xDFFF {
						return "", fail("high surrogate escape not followed by a low surrogate at offset %d", s.i-6)
					}
					s.i += 6
					v = 0x10000 + ((v - 0xD800) << 10) + (lo - 0xDC00)
				} else if v >= 0xDC00 && v <= 0xDFFF {
					return "", fail("unpaired low surrogate escape at offset %d", s.i-6)
				}
				out = utf8.AppendRune(out, rune(v))
			default:
				return "", fail("invalid escape sequence at offset %d", s.i-1)
			}
		case c < 0x20:
			return "", fail("raw control character in string at offset %d", s.i)
		default:
			out = append(out, c)
			s.i++
		}
	}
}

func (s *scanner) object(depth int) error {
	if depth > s.maxDepth {
		return fail("JSON nesting exceeds %d containers", s.maxDepth)
	}
	s.i++ // {
	s.ws()
	seen := map[string]bool{}
	if s.i < len(s.b) && s.b[s.i] == '}' {
		s.i++
		return nil
	}
	for {
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != '"' {
			return fail("expected property name at offset %d", s.i)
		}
		k, err := s.str()
		if err != nil {
			return err
		}
		if seen[k] {
			return fail("duplicate property name %q", k)
		}
		seen[k] = true
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != ':' {
			return fail("expected colon at offset %d", s.i)
		}
		s.i++
		s.ws()
		if err := s.value(depth); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) {
			return fail("unterminated object")
		}
		switch s.b[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return nil
		default:
			return fail("expected comma or closing brace at offset %d", s.i)
		}
	}
}

func (s *scanner) array(depth int) error {
	if depth > s.maxDepth {
		return fail("JSON nesting exceeds %d containers", s.maxDepth)
	}
	s.i++ // [
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == ']' {
		s.i++
		return nil
	}
	for {
		s.ws()
		if err := s.value(depth); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) {
			return fail("unterminated array")
		}
		switch s.b[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			return nil
		default:
			return fail("expected comma or closing bracket at offset %d", s.i)
		}
	}
}

// Decode validates data strictly, then decodes into v rejecting unknown fields.
func Decode(data []byte, maxDepth int, v any) error {
	if err := ValidateObject(data, maxDepth); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &Error{Msg: friendly(err)}
	}
	return nil
}

func friendly(err error) string {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fmt.Sprintf("field %q has the wrong type", ute.Field)
	}
	return err.Error()
}
