package wasmguest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxValueDepth = 64
	maxValueNodes = 1 << 20
)

// decodeArgs decodes a string-keyed kwargs object into JSON-like Go values:
// nil, bool, int64, float64, string, []any, map[string]any.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	v, err := decodeValue(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("args is not an object")
	}
	return m, nil
}

// decodeValue decodes one JSON-like value strictly. It rejects duplicate
// object keys, invalid UTF-8, unpaired surrogate escapes, non-finite floats,
// integer tokens outside int64, excessive depth, and excessive node count.
func decodeValue(raw []byte) (any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty value")
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	if err := validateJSONStringEscapes(raw); err != nil {
		return nil, err
	}
	if err := checkDuplicateKeys(raw); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	d := &valueDecoder{}
	return d.normalize(v, 0)
}

type valueDecoder struct {
	nodes int
}

func (d *valueDecoder) normalize(v any, depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("value depth exceeds %d", maxValueDepth)
	}
	d.nodes++
	if d.nodes > maxValueNodes {
		return nil, fmt.Errorf("value node count exceeds %d", maxValueNodes)
	}
	switch x := v.(type) {
	case nil, bool:
		return x, nil
	case string:
		if !utf8.ValidString(x) {
			return nil, fmt.Errorf("invalid UTF-8 string")
		}
		return x, nil
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			i, err := x.Int64()
			if err != nil {
				return nil, fmt.Errorf("integer out of int64 range: %s", s)
			}
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, err
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("non-finite float")
		}
		return f, nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			n, err := d.normalize(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if !utf8.ValidString(k) {
				return nil, fmt.Errorf("invalid UTF-8 object key")
			}
			n, err := d.normalize(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value type %T", v)
	}
}

// encodeValue encodes one JSON-like value strictly. It accepts nil, bool,
// int64, finite float64, valid UTF-8 strings, slices/arrays of JSON-like
// values, and maps with string keys. It rejects unsupported types, cycles,
// excessive depth/node count, invalid UTF-8, and non-finite floats.
func encodeValue(v any) ([]byte, error) {
	e := &valueEncoder{seen: map[valueVisit]bool{}}
	if err := e.encode(v, 0); err != nil {
		return nil, err
	}
	return e.buf.Bytes(), nil
}

type valueVisit struct {
	kind reflect.Kind
	ptr  uintptr
}

type valueEncoder struct {
	buf   bytes.Buffer
	nodes int
	seen  map[valueVisit]bool
}

func (e *valueEncoder) encode(v any, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("value depth exceeds %d", maxValueDepth)
	}
	e.nodes++
	if e.nodes > maxValueNodes {
		return fmt.Errorf("value node count exceeds %d", maxValueNodes)
	}
	switch x := v.(type) {
	case nil:
		e.buf.WriteString("null")
	case bool:
		if x {
			e.buf.WriteString("true")
		} else {
			e.buf.WriteString("false")
		}
	case int64:
		e.buf.WriteString(strconv.FormatInt(x, 10))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("non-finite float")
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eE") {
			s += ".0"
		}
		e.buf.WriteString(s)
	case string:
		if !utf8.ValidString(x) {
			return fmt.Errorf("invalid UTF-8 string")
		}
		e.writeString(x)
	case []any:
		ptr := reflect.ValueOf(x).Pointer()
		key := valueVisit{kind: reflect.Slice, ptr: ptr}
		if e.seen[key] {
			return fmt.Errorf("cyclic value")
		}
		e.seen[key] = true
		defer delete(e.seen, key)
		e.buf.WriteByte('[')
		for i, elem := range x {
			if i > 0 {
				e.buf.WriteByte(',')
			}
			if err := e.encode(elem, depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte(']')
	case map[string]any:
		ptr := reflect.ValueOf(x).Pointer()
		key := valueVisit{kind: reflect.Map, ptr: ptr}
		if e.seen[key] {
			return fmt.Errorf("cyclic value")
		}
		e.seen[key] = true
		defer delete(e.seen, key)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		e.buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				e.buf.WriteByte(',')
			}
			if !utf8.ValidString(k) {
				return fmt.Errorf("invalid UTF-8 object key")
			}
			e.writeString(k)
			e.buf.WriteByte(':')
			if err := e.encode(x[k], depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte('}')
	default:
		return e.encodeReflect(v, depth)
	}
	return nil
}

func (e *valueEncoder) encodeReflect(v any, depth int) error {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("unsupported value type %T (bytes are not a JSON-like value)", v)
		}
		if rv.Kind() == reflect.Slice {
			ptr := rv.Pointer()
			key := valueVisit{kind: reflect.Slice, ptr: ptr}
			if e.seen[key] {
				return fmt.Errorf("cyclic value")
			}
			e.seen[key] = true
			defer delete(e.seen, key)
		}
		e.buf.WriteByte('[')
		for i := 0; i < rv.Len(); i++ {
			if i > 0 {
				e.buf.WriteByte(',')
			}
			if err := e.encode(rv.Index(i).Interface(), depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte(']')
		return nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("unsupported map key type %s", rv.Type().Key())
		}
		ptr := rv.Pointer()
		key := valueVisit{kind: reflect.Map, ptr: ptr}
		if e.seen[key] {
			return fmt.Errorf("cyclic value")
		}
		e.seen[key] = true
		defer delete(e.seen, key)
		keys := rv.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		e.buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				e.buf.WriteByte(',')
			}
			ks := k.String()
			if !utf8.ValidString(ks) {
				return fmt.Errorf("invalid UTF-8 object key")
			}
			e.writeString(ks)
			e.buf.WriteByte(':')
			if err := e.encode(rv.MapIndex(k).Interface(), depth+1); err != nil {
				return err
			}
		}
		e.buf.WriteByte('}')
		return nil
	default:
		return fmt.Errorf("unsupported value type %T", v)
	}
}

const hexDigits = "0123456789abcdef"

func (e *valueEncoder) writeString(s string) {
	e.buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			e.buf.WriteString(`\"`)
		case '\\':
			e.buf.WriteString(`\\`)
		case '\n':
			e.buf.WriteString(`\n`)
		case '\r':
			e.buf.WriteString(`\r`)
		case '\t':
			e.buf.WriteString(`\t`)
		case '\b':
			e.buf.WriteString(`\b`)
		case '\f':
			e.buf.WriteString(`\f`)
		default:
			if c < 0x20 {
				e.buf.WriteString(`\u00`)
				e.buf.WriteByte(hexDigits[c>>4])
				e.buf.WriteByte(hexDigits[c&0xf])
			} else {
				e.buf.WriteByte(c)
			}
		}
	}
	e.buf.WriteByte('"')
}

// validateJSONStringEscapes rejects unpaired UTF-16 surrogate escapes. Go's
// encoding/json silently replaces them with U+FFFD, which would hide a
// malformed guest value.
func validateJSONStringEscapes(raw []byte) error {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		if c == '"' {
			inString = false
			continue
		}
		if c != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return fmt.Errorf("truncated JSON escape")
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return fmt.Errorf("truncated JSON unicode escape")
		}
		v, err := parseHex4(raw[i+1 : i+5])
		if err != nil {
			return err
		}
		i += 4
		if v >= 0xD800 && v <= 0xDBFF {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return fmt.Errorf("unpaired high surrogate")
			}
			v2, err := parseHex4(raw[i+3 : i+7])
			if err != nil {
				return err
			}
			if v2 < 0xDC00 || v2 > 0xDFFF {
				return fmt.Errorf("unpaired high surrogate")
			}
			i += 6
		} else if v >= 0xDC00 && v <= 0xDFFF {
			return fmt.Errorf("unpaired low surrogate")
		}
	}
	return nil
}

func parseHex4(b []byte) (uint32, error) {
	if len(b) != 4 {
		return 0, fmt.Errorf("truncated JSON unicode escape")
	}
	var v uint32
	for _, c := range b {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		default:
			return 0, fmt.Errorf("invalid JSON unicode escape")
		}
	}
	return v, nil
}
