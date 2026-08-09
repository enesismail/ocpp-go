// Package canonjson defines the comparison representation used by parity
// checks. Canonicalize must recursively sort object keys, preserve array
// element order, and preserve the original bytes of JSON numbers and strings.
package canonjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// Canonicalize returns byte-stable JSON with object keys sorted at every
// nesting level. Arrays remain in input order; scalar number and string
// lexemes are not normalized. Invalid JSON is returned as an error.
func Canonicalize(input []byte) ([]byte, error) {
	p := &parser{input: input}
	p.skipSpace()

	value, err := p.parseValue()
	if err != nil {
		return nil, err
	}

	p.skipSpace()
	if p.pos != len(p.input) {
		return nil, fmt.Errorf("canonjson: unexpected data after JSON value at offset %d", p.pos)
	}

	var buf bytes.Buffer
	writeValue(&buf, value)
	return buf.Bytes(), nil
}

type valueKind int

const (
	kindObject valueKind = iota
	kindArray
	kindLiteral
)

// member is one key/value pair of a JSON object, in the order it was read.
// rawKey is the property's JSON string bytes exactly as written (quotes and
// any escapes included) and is what gets written back out; sortKey is its
// decoded form, used only to order members.
type member struct {
	rawKey  []byte
	sortKey string
	value   jsonValue
}

// jsonValue is a parsed JSON value that keeps enough of the original bytes to
// reproduce every scalar unchanged. Object members and array elements are
// stored in the order they were read; canonicalizing only reorders an
// object's own members, recursively.
type jsonValue struct {
	kind    valueKind
	raw     []byte // populated for kindLiteral: a string, number, true, false or null exactly as read
	members []member
	items   []jsonValue
}

type parser struct {
	input []byte
	pos   int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.input) {
		switch p.input[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) parseValue() (jsonValue, error) {
	if p.pos >= len(p.input) {
		return jsonValue{}, fmt.Errorf("canonjson: unexpected end of input")
	}

	switch c := p.input[p.pos]; {
	case c == '{':
		return p.parseObject()
	case c == '[':
		return p.parseArray()
	case c == '"':
		raw, err := p.parseString()
		if err != nil {
			return jsonValue{}, err
		}
		return jsonValue{kind: kindLiteral, raw: raw}, nil
	case c == 't':
		raw, err := p.parseLiteral("true")
		if err != nil {
			return jsonValue{}, err
		}
		return jsonValue{kind: kindLiteral, raw: raw}, nil
	case c == 'f':
		raw, err := p.parseLiteral("false")
		if err != nil {
			return jsonValue{}, err
		}
		return jsonValue{kind: kindLiteral, raw: raw}, nil
	case c == 'n':
		raw, err := p.parseLiteral("null")
		if err != nil {
			return jsonValue{}, err
		}
		return jsonValue{kind: kindLiteral, raw: raw}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		raw, err := p.parseNumber()
		if err != nil {
			return jsonValue{}, err
		}
		return jsonValue{kind: kindLiteral, raw: raw}, nil
	default:
		return jsonValue{}, fmt.Errorf("canonjson: unexpected character %q at offset %d", c, p.pos)
	}
}

func (p *parser) parseObject() (jsonValue, error) {
	p.pos++ // consume '{'
	obj := jsonValue{kind: kindObject}

	p.skipSpace()
	if p.pos < len(p.input) && p.input[p.pos] == '}' {
		p.pos++
		return obj, nil
	}

	for {
		p.skipSpace()
		if p.pos >= len(p.input) || p.input[p.pos] != '"' {
			return jsonValue{}, fmt.Errorf("canonjson: expected object key at offset %d", p.pos)
		}
		rawKey, err := p.parseString()
		if err != nil {
			return jsonValue{}, err
		}
		var sortKey string
		if err := json.Unmarshal(rawKey, &sortKey); err != nil {
			return jsonValue{}, fmt.Errorf("canonjson: invalid object key: %w", err)
		}

		p.skipSpace()
		if p.pos >= len(p.input) || p.input[p.pos] != ':' {
			return jsonValue{}, fmt.Errorf("canonjson: expected ':' after object key at offset %d", p.pos)
		}
		p.pos++
		p.skipSpace()

		value, err := p.parseValue()
		if err != nil {
			return jsonValue{}, err
		}
		obj.members = append(obj.members, member{rawKey: rawKey, sortKey: sortKey, value: value})

		p.skipSpace()
		if p.pos >= len(p.input) {
			return jsonValue{}, fmt.Errorf("canonjson: unterminated object")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
			continue
		case '}':
			p.pos++
			return obj, nil
		default:
			return jsonValue{}, fmt.Errorf("canonjson: expected ',' or '}' at offset %d", p.pos)
		}
	}
}

func (p *parser) parseArray() (jsonValue, error) {
	p.pos++ // consume '['
	arr := jsonValue{kind: kindArray}

	p.skipSpace()
	if p.pos < len(p.input) && p.input[p.pos] == ']' {
		p.pos++
		return arr, nil
	}

	for {
		p.skipSpace()
		value, err := p.parseValue()
		if err != nil {
			return jsonValue{}, err
		}
		arr.items = append(arr.items, value)

		p.skipSpace()
		if p.pos >= len(p.input) {
			return jsonValue{}, fmt.Errorf("canonjson: unterminated array")
		}
		switch p.input[p.pos] {
		case ',':
			p.pos++
			continue
		case ']':
			p.pos++
			return arr, nil
		default:
			return jsonValue{}, fmt.Errorf("canonjson: expected ',' or ']' at offset %d", p.pos)
		}
	}
}

// parseString returns the raw bytes of a JSON string literal, quotes
// included, exactly as they appear in the input.
func (p *parser) parseString() ([]byte, error) {
	start := p.pos
	p.pos++ // consume opening quote

	for {
		if p.pos >= len(p.input) {
			return nil, fmt.Errorf("canonjson: unterminated string starting at offset %d", start)
		}
		c := p.input[p.pos]
		switch {
		case c == '"':
			p.pos++
			return p.input[start:p.pos], nil
		case c == '\\':
			p.pos++
			if p.pos >= len(p.input) {
				return nil, fmt.Errorf("canonjson: unterminated escape sequence")
			}
			switch p.input[p.pos] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				p.pos++
			case 'u':
				p.pos++
				if p.pos+4 > len(p.input) {
					return nil, fmt.Errorf("canonjson: invalid unicode escape at offset %d", p.pos)
				}
				for i := 0; i < 4; i++ {
					if !isHexDigit(p.input[p.pos+i]) {
						return nil, fmt.Errorf("canonjson: invalid unicode escape at offset %d", p.pos)
					}
				}
				p.pos += 4
			default:
				return nil, fmt.Errorf("canonjson: invalid escape character %q at offset %d", p.input[p.pos], p.pos)
			}
		case c < 0x20:
			return nil, fmt.Errorf("canonjson: invalid control character in string at offset %d", p.pos)
		default:
			p.pos++
		}
	}
}

// parseNumber returns the raw bytes of a JSON number literal exactly as they
// appear in the input, so a lexeme such as 1.00 or -0.0e+0 is never
// reformatted.
func (p *parser) parseNumber() ([]byte, error) {
	start := p.pos

	if p.pos < len(p.input) && p.input[p.pos] == '-' {
		p.pos++
	}
	if p.pos >= len(p.input) || !isDigit(p.input[p.pos]) {
		return nil, fmt.Errorf("canonjson: invalid number at offset %d", start)
	}
	if p.input[p.pos] == '0' {
		p.pos++
	} else {
		for p.pos < len(p.input) && isDigit(p.input[p.pos]) {
			p.pos++
		}
	}

	if p.pos < len(p.input) && p.input[p.pos] == '.' {
		p.pos++
		if p.pos >= len(p.input) || !isDigit(p.input[p.pos]) {
			return nil, fmt.Errorf("canonjson: invalid number fraction at offset %d", start)
		}
		for p.pos < len(p.input) && isDigit(p.input[p.pos]) {
			p.pos++
		}
	}

	if p.pos < len(p.input) && (p.input[p.pos] == 'e' || p.input[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.input) && (p.input[p.pos] == '+' || p.input[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.input) || !isDigit(p.input[p.pos]) {
			return nil, fmt.Errorf("canonjson: invalid number exponent at offset %d", start)
		}
		for p.pos < len(p.input) && isDigit(p.input[p.pos]) {
			p.pos++
		}
	}

	return p.input[start:p.pos], nil
}

func (p *parser) parseLiteral(lit string) ([]byte, error) {
	if p.pos+len(lit) > len(p.input) || string(p.input[p.pos:p.pos+len(lit)]) != lit {
		return nil, fmt.Errorf("canonjson: invalid literal at offset %d, want %q", p.pos, lit)
	}
	start := p.pos
	p.pos += len(lit)
	return p.input[start:p.pos], nil
}

func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// writeValue serializes v, sorting each object's members by their decoded
// key at every nesting level while leaving array order and every scalar's
// raw bytes untouched.
func writeValue(buf *bytes.Buffer, v jsonValue) {
	switch v.kind {
	case kindObject:
		members := make([]member, len(v.members))
		copy(members, v.members)
		sort.SliceStable(members, func(i, j int) bool { return members[i].sortKey < members[j].sortKey })

		buf.WriteByte('{')
		for i, m := range members {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.Write(m.rawKey)
			buf.WriteByte(':')
			writeValue(buf, m.value)
		}
		buf.WriteByte('}')
	case kindArray:
		buf.WriteByte('[')
		for i, item := range v.items {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeValue(buf, item)
		}
		buf.WriteByte(']')
	default:
		buf.Write(v.raw)
	}
}
