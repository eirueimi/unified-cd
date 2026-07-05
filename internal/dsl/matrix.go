package dsl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DefaultMatrixMaxCombinations is the cap applied when the controller does not
// supply one (or supplies 0).
const DefaultMatrixMaxCombinations = 64

// MatrixCombo is one expanded combination of a matrix step.
type MatrixCombo struct {
	Values map[string]string // dimension name → value
	Key    string            // values joined with "/" in dimension declaration order
}

// EvalMatrix resolves every dimension source, builds the cartesian product in
// declaration order, removes exclude matches, and enforces the combination cap.
// A dimension that resolves to an empty list yields zero combinations (skip).
// Values containing "/" are rejected because "/" is the key separator.
func EvalMatrix(def MatrixDef, data TemplateData, maxCombos int) ([]MatrixCombo, error) {
	if maxCombos <= 0 {
		maxCombos = DefaultMatrixMaxCombinations
	}
	if len(def.Dimensions) == 0 {
		return nil, fmt.Errorf("matrix has no dimensions")
	}
	combos := []MatrixCombo{{Values: map[string]string{}}}
	for _, dim := range def.Dimensions {
		items, err := EvalForeachSource(dim.Source, data)
		if err != nil {
			return nil, fmt.Errorf("matrix.%s: %w", dim.Name, err)
		}
		// Bail out on the running (pre-exclude) product size as soon as it
		// would exceed the cap, before allocating the expanded slice. This
		// avoids materializing a huge cartesian product (e.g. 100^4 combos)
		// just to discard it a few lines later. Excludes can only shrink the
		// final count, so a running product already over the cap is enough
		// to know the post-exclude count also cannot be computed cheaply
		// without the same blow-up; we report the same error the old
		// post-exclude check would have produced once we can no longer
		// bound the result within maxCombos.
		if len(items) != 0 && len(combos) > maxCombos/len(items) {
			// len(combos)*len(items) would exceed maxCombos.
			projected := len(combos) * len(items)
			return nil, fmt.Errorf("matrix expands to %d combinations, exceeding the limit of %d", projected, maxCombos)
		}
		next := make([]MatrixCombo, 0, len(combos)*len(items))
		for _, c := range combos {
			for _, v := range items {
				if strings.Contains(v, "/") {
					return nil, fmt.Errorf("matrix.%s: value %q must not contain \"/\" (reserved as the combination key separator)", dim.Name, v)
				}
				values := make(map[string]string, len(c.Values)+1)
				for k, val := range c.Values {
					values[k] = val
				}
				values[dim.Name] = v
				key := v
				if c.Key != "" {
					key = c.Key + "/" + v
				}
				next = append(next, MatrixCombo{Values: values, Key: key})
			}
		}
		combos = next
	}
	filtered := combos[:0]
	for _, c := range combos {
		if !matchesAnyExclude(c.Values, def.Exclude) {
			filtered = append(filtered, c)
		}
	}
	if len(filtered) > maxCombos {
		return nil, fmt.Errorf("matrix expands to %d combinations, exceeding the limit of %d", len(filtered), maxCombos)
	}
	return filtered, nil
}

// matchesAnyExclude reports whether values matches at least one exclude entry
// (an entry matches when all of its key/value pairs equal the combination's).
func matchesAnyExclude(values map[string]string, exclude []map[string]string) bool {
	for _, ex := range exclude {
		if len(ex) == 0 {
			continue
		}
		all := true
		for k, v := range ex {
			if values[k] != v {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// StringOutputs converts a plain string output map to the any-typed form
// stored in StepData.Outputs.
func StringOutputs(m map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// OutputValueString renders a StepData output value as a string: plain strings
// pass through, aggregated matrix maps are JSON-encoded (stable key order via
// encoding/json), anything else falls back to fmt.
func OutputValueString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]string:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	default:
		return fmt.Sprintf("%v", v)
	}
}
