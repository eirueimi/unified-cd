package controller

import (
	"fmt"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// resolveParams validates the caller-supplied params against the Job's declared
// inputs and fills in defaults for any that were omitted.
//
//   - A param omitted by the caller (or supplied as an explicit empty string)
//     but carrying a `default` is injected with that default (formatted via
//     fmt.Sprintf("%v", ...)). An explicit empty value is treated as "unset" so
//     that `working_dir: ""` falls back to the declared default rather than
//     silently overriding it with "".
//   - A param declared `required: true` with no caller-supplied (non-empty) value
//     and no default causes an error naming the missing param.
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
		// Presence with a non-empty value keeps the caller's value. An explicitly
		// empty value ("") is treated as unset so `default:` still applies — this
		// matches documented behavior and avoids an empty string silently bypassing
		// a declared default. (Params without a declared default keep the caller's
		// empty string, since there is nothing to fall back to.)
		if v, ok := resolved[in.Name]; ok && v != "" {
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
