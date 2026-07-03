package controller

import (
	"encoding/json"
	"fmt"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// resolveParams validates the caller-supplied params against the Job's declared
// inputs and fills in defaults for any that were omitted.
//
//   - A param omitted by the caller but carrying a `default` is injected with
//     that default (formatted via fmt.Sprintf("%v", ...)).
//   - A param declared `required: true` with no caller-supplied value and no
//     default causes an error naming the missing param.
//   - Params not declared as inputs are passed through unchanged.
//
// The input map is not mutated; a new map is returned.
func resolveParams(inputs []dsl.Input, supplied map[string]string) (map[string]string, error) {
	resolved := make(map[string]string, len(supplied)+len(inputs))
	for k, v := range supplied {
		resolved[k] = v
	}

	var missing []string
	for _, in := range inputs {
		if _, ok := resolved[in.Name]; ok {
			continue
		}
		if in.Default != nil {
			resolved[in.Name] = fmt.Sprintf("%v", in.Default)
			continue
		}
		if in.Required {
			missing = append(missing, in.Name)
		}
	}

	if len(missing) == 1 {
		return nil, fmt.Errorf("missing required param: %s", missing[0])
	}
	if len(missing) > 1 {
		return nil, fmt.Errorf("missing required params: %v", missing)
	}

	return resolved, nil
}

// inputsFromSpecJSON extracts the declared params.inputs from a stored job spec
// JSON blob. Returns nil (no validation performed) when parsing fails or there
// are no inputs — callers should tolerate a nil/empty slice.
func inputsFromSpecJSON(specJSON []byte) []dsl.Input {
	var spec dsl.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil
	}
	return spec.Params.Inputs
}
