// docgen generates docs/field-reference.md from schemas/unified-cd.schema.json.
// Run via: go generate ./internal/dsl/
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// rootKinds defines display order for the field reference.
var rootKinds = []string{"Job", "Schedule", "WebhookReceiver", "AppSource", "GitCredential"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "docgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := projectRoot()
	if err != nil {
		return err
	}

	data, err := os.ReadFile(filepath.Join(root, "schemas", "unified-cd.schema.json"))
	if err != nil {
		return err
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return err
	}
	defs, _ := schema["definitions"].(map[string]any)

	var sb strings.Builder
	sb.WriteString("# unified-cd Field Reference\n\n")
	sb.WriteString("> This file is auto-generated. Do not edit it directly.\n")
	sb.WriteString("> Regenerate with `go generate ./internal/dsl/`.\n\n")
	sb.WriteString("## Table of Contents\n\n")
	for _, kind := range rootKinds {
		if _, ok := defs[kind]; ok {
			fmt.Fprintf(&sb, "- [%s](#%s)\n", kind, strings.ToLower(kind))
		}
	}
	sb.WriteString("\n---\n\n")

	for _, kind := range rootKinds {
		def, ok := defs[kind].(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(&sb, "## %s\n\n", kind)
		if desc, _ := def["description"].(string); desc != "" {
			fmt.Fprintf(&sb, "%s\n\n", desc)
		}

		written := map[string]bool{}
		writeSection(&sb, kind, def, defs, written)
	}

	out := filepath.Join(root, "docs", "field-reference.md")
	return os.WriteFile(out, []byte(sb.String()), 0o644)
}

// writeSection writes a field table for defName and recurses into referenced types.
func writeSection(sb *strings.Builder, defName string, def map[string]any, defs map[string]any, written map[string]bool) {
	if written[defName] {
		return
	}
	written[defName] = true

	props, _ := def["properties"].(map[string]any)
	if len(props) == 0 {
		return
	}

	reqSet := requiredSet(def)

	// Sort field names for deterministic output.
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	sb.WriteString("| Field | Type | Required | Description |\n")
	sb.WriteString("|-------|------|----------|-------------|\n")

	var deferred []string // ref names to expand after the table

	for _, k := range keys {
		v, _ := props[k].(map[string]any)
		req := "no"
		if reqSet[k] {
			req = "yes"
		}
		typeStr, refName := fieldType(v)
		desc, _ := v["description"].(string)
		fmt.Fprintf(sb, "| `%s` | %s | %s | %s |\n", k, typeStr, req, desc)

		if refName != "" && !written[refName] {
			if _, exists := defs[refName]; exists {
				deferred = append(deferred, refName)
			}
		}
	}
	sb.WriteString("\n")

	// Expand referenced types as sub-sections (deduped).
	seen := map[string]bool{}
	for _, ref := range deferred {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		// Also skip if already emitted by a recursive call above.
		if written[ref] {
			continue
		}
		refDef, ok := defs[ref].(map[string]any)
		if !ok {
			continue
		}
		fmt.Fprintf(sb, "### %s\n\n", ref)
		if desc, _ := refDef["description"].(string); desc != "" {
			fmt.Fprintf(sb, "%s\n\n", desc)
		}
		writeSection(sb, ref, refDef, defs, written)
	}
}

// fieldType returns a human-readable type string and the direct $ref name (if any).
func fieldType(v map[string]any) (typeStr, ref string) {
	if r, ok := v["$ref"].(string); ok {
		name := strings.TrimPrefix(r, "#/definitions/")
		return name, name
	}
	if enums, ok := v["enum"].([]any); ok {
		parts := make([]string, len(enums))
		for i, e := range enums {
			parts[i] = fmt.Sprintf("`%v`", e)
		}
		return strings.Join(parts, " \\| "), ""
	}
	switch v["type"] {
	case "array":
		items, _ := v["items"].(map[string]any)
		inner, innerRef := fieldType(items)
		return "[]" + inner, innerRef
	case "object":
		if ap, ok := v["additionalProperties"].(map[string]any); ok {
			inner, _ := fieldType(ap)
			return "map[string]" + inner, ""
		}
		return "object", ""
	case "string":
		return "string", ""
	case "boolean":
		return "boolean", ""
	case "integer":
		return "integer", ""
	case "number":
		return "number", ""
	}
	return "any", ""
}

func requiredSet(def map[string]any) map[string]bool {
	s := map[string]bool{}
	req, _ := def["required"].([]any)
	for _, r := range req {
		if k, ok := r.(string); ok {
			s[k] = true
		}
	}
	return s
}

func projectRoot() (string, error) {
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}
