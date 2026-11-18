package quickpy

import (
	"encoding/json"
	"fmt"
	"math"
)

const maxValueDepth = 64

// validateValue enforces the public JSON-like value model.
func validateValue(v Value, depth int) error {
	if depth > maxValueDepth {
		return fmt.Errorf("value depth exceeds %d", maxValueDepth)
	}
	switch x := v.(type) {
	case nil, bool, int64, string:
		return nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return fmt.Errorf("non-finite float")
		}
		return nil
	case []any:
		for i, e := range x {
			if err := validateValue(e, depth+1); err != nil {
				return fmt.Errorf("[%d]: %w", i, err)
			}
		}
		return nil
	case map[string]any:
		for k, e := range x {
			if err := validateValue(e, depth+1); err != nil {
				return fmt.Errorf("[%q]: %w", k, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported value type %T", v)
	}
}

// stateSize returns the encoded size of a committed state value.
func stateSize(v Value) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}
