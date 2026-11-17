package wasmguest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

const maxValueDepth = 64

// decodeArgs decodes a string-keyed kwargs object into JSON-like Go values:
// nil, bool, int64, float64, string, []any, map[string]any.
func decodeArgs(raw json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("args is not an object")
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value in args")
		}
		return nil, err
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		n, err := normalizeValue(v, 0)
		if err != nil {
			return nil, fmt.Errorf("args[%q]: %w", k, err)
		}
		out[k] = n
	}
	return out, nil
}

func normalizeValue(v any, depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("value depth exceeds %d", maxValueDepth)
	}
	switch x := v.(type) {
	case nil, bool, string:
		return x, nil
	case json.Number:
		s := x.String()
		if !strings.ContainsAny(s, ".eE") {
			if i, err := x.Int64(); err == nil {
				return i, nil
			}
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
			n, err := normalizeValue(e, depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			n, err := normalizeValue(e, depth+1)
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
