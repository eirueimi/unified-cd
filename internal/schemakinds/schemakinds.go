// Package schemakinds derives the set of top-level manifest kinds from the
// generated JSON Schema (schemas/unified-cd.schema.json) — the schema
// itself derived from internal/dsl by cmd/schemagen (see deriveRoots there).
//
// This is the second consumer of that derivation, after cmd/docgen. Both
// need the same fact — "what are all the root kinds" — and neither may
// answer it with a hand-written list: this project has now shipped that
// exact bug (a maintained list drifting from the thing it describes) eight
// times, most recently as export's own exportKindDir silently omitting a
// kind never taught to it. rootKindsFromSchema/RootKinds used to live only
// in cmd/docgen; it is pulled out here so internal/cli's export-completeness
// guard (see export_kind_completeness_test.go) can read the same derived
// list instead of restating it a third time.
package schemakinds

import (
	"fmt"
	"strings"
)

// RootKinds returns the definition names in the schema's document-level
// oneOf, in order. An empty or missing oneOf is an error rather than an
// empty list: for docgen that would silently render a field reference with
// nothing under it, and for the export guard it would make every kind look
// "covered" by vacuous truth — both read as "the generator is broken", not
// "this project has no resource kinds".
func RootKinds(schema map[string]any) ([]string, error) {
	oneOf, _ := schema["oneOf"].([]any)
	kinds := make([]string, 0, len(oneOf))
	for _, entry := range oneOf {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		ref, ok := m["$ref"].(string)
		if !ok {
			continue
		}
		kinds = append(kinds, strings.TrimPrefix(ref, "#/definitions/"))
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("schema has no document-level oneOf branches: cannot tell which kinds are roots")
	}
	return kinds, nil
}
