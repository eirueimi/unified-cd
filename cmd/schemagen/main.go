// schemagen generates schemas/unified-cd.schema.json from internal/dsl/*_types.go.
// Run via: go generate ./internal/dsl/
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// SchemaNode is an alias for a JSON Schema object.
type SchemaNode = map[string]any

// rootDisplayOrder is a COSMETIC ordering hint, not the list of root kinds.
//
// The SET of roots is derived (see deriveRoots) precisely so that adding a new
// top-level kind to internal/dsl cannot silently omit it from the schema. That
// failure has happened here once already: when kind: Vars landed, this file's
// hand-maintained list never learned about it, so the document-level oneOf had
// no Vars branch and every correct Vars manifest was INVALID against the
// published schema — flagged as an error by the VS Code extension.
//
// A derived root not named below is appended in alphabetical order, so the
// worst a forgotten entry here can cost is where a kind appears in the oneOf
// and in the generated field reference. Nothing can be dropped.
var rootDisplayOrder = []string{
	"Job",
	"JobTemplate",
	"Schedule",
	"WebhookReceiver",
	"AppSource",
	"GitCredential",
}

// deriveRoots returns the top-level YAML resource kinds: every parsed struct
// carrying BOTH an `apiVersion` and a `kind` YAML field. Those two fields are
// what makes a struct a document root — nothing else in internal/dsl declares
// them, and every kind that does is applied as its own YAML document.
//
// The kind VALUE is the struct name. That is not a convention invented here:
// each kind's Validate() compares Kind against a literal, and all of them
// (Job, JobTemplate, Schedule, WebhookReceiver, AppSource, GitCredential,
// Vars) compare against their own struct name.
func deriveRoots(structs map[string]StructInfo) []string {
	found := map[string]bool{}
	for name, info := range structs {
		var hasAPIVersion, hasKind bool
		for _, f := range info.Fields {
			switch f.YAMLName {
			case "apiVersion":
				hasAPIVersion = true
			case "kind":
				hasKind = true
			}
		}
		if hasAPIVersion && hasKind {
			found[name] = true
		}
	}

	out := make([]string, 0, len(found))
	for _, name := range rootDisplayOrder {
		if found[name] {
			out = append(out, name)
			delete(found, name)
		}
	}
	// Anything the display list does not mention still ships, alphabetically.
	out = append(out, sortedKeys(found)...)
	return out
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := projectRoot()
	if err != nil {
		return err
	}
	dslDir := filepath.Join(root, "internal", "dsl")

	structs, typeDescs, err := parseDSL(dslDir)
	if err != nil {
		return fmt.Errorf("parse dsl: %w", err)
	}

	schema := buildSchema(structs, typeDescs)

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return err
	}
	out := filepath.Join(root, "schemas", "unified-cd.schema.json")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return os.WriteFile(out, append(data, '\n'), 0o644)
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

// -----------------------------------------------------------------------
// AST parsing
// -----------------------------------------------------------------------

// StructInfo holds ordered field definitions for one struct.
type StructInfo struct {
	Fields []FieldInfo
}

// FieldInfo holds parsed metadata for one struct field.
type FieldInfo struct {
	YAMLName string
	Expr     ast.Expr
	Required bool // !omitempty && !pointer type
	Desc     string
	Enums    []string // values from schema:"enum:a,b,c" tag
}

// parseDSL reads all *_types.go files in dir and returns:
//   - structs: map from struct name → StructInfo
//   - typeDescs: map from struct name → doc comment
func parseDSL(dir string) (map[string]StructInfo, map[string]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		n := fi.Name()
		return n == "types.go" || strings.HasSuffix(n, "_types.go")
	}, parser.ParseComments)
	if err != nil {
		return nil, nil, err
	}

	structs := map[string]StructInfo{}
	typeDescs := map[string]string{}

	for _, pkg := range pkgs {
		// Sort file names for deterministic traversal order.
		fileNames := make([]string, 0, len(pkg.Files))
		for name := range pkg.Files {
			fileNames = append(fileNames, name)
		}
		sort.Strings(fileNames)

		for _, fname := range fileNames {
			file := pkg.Files[fname]
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				typeDoc := ""
				if gd.Doc != nil {
					typeDoc = cleanComment(gd.Doc.Text())
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					name := ts.Name.Name
					structs[name] = StructInfo{Fields: extractFields(st)}
					if typeDoc != "" {
						typeDescs[name] = typeDoc
					}
				}
			}
		}
	}
	return structs, typeDescs, nil
}

func extractFields(st *ast.StructType) []FieldInfo {
	var fields []FieldInfo
	for _, f := range st.Fields.List {
		if f.Tag == nil {
			continue
		}
		tag := reflect.StructTag(strings.Trim(f.Tag.Value, "`"))

		yamlTag := tag.Get("yaml")
		if yamlTag == "" || yamlTag == "-" {
			continue
		}
		parts := strings.SplitN(yamlTag, ",", 2)
		yamlName := parts[0]
		if yamlName == "" {
			continue
		}
		omitempty := len(parts) == 2 && strings.Contains(parts[1], "omitempty")
		_, isPtr := f.Type.(*ast.StarExpr)

		desc := ""
		if f.Comment != nil && len(f.Comment.List) > 0 {
			desc = cleanComment(f.Comment.List[0].Text)
		}
		if desc == "" && f.Doc != nil {
			desc = cleanComment(f.Doc.Text())
		}

		var enums []string
		if schemaTag := tag.Get("schema"); strings.HasPrefix(schemaTag, "enum:") {
			for _, e := range strings.Split(strings.TrimPrefix(schemaTag, "enum:"), ",") {
				if v := strings.TrimSpace(e); v != "" {
					enums = append(enums, v)
				}
			}
		}

		fields = append(fields, FieldInfo{
			YAMLName: yamlName,
			Expr:     f.Type,
			Required: !omitempty && !isPtr,
			Desc:     desc,
			Enums:    enums,
		})
	}
	return fields
}

func cleanComment(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "//") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "//"))
	}
	return s
}

// -----------------------------------------------------------------------
// Schema generation
// -----------------------------------------------------------------------

// manualSchemaOverrides holds hand-written schema fragments for types whose
// real YAML shape the AST-based field extractor cannot derive: these types
// have a custom UnmarshalYAML and yaml-untagged Go fields (so extractFields
// sees zero fields and would otherwise emit a useless
// {"properties": {}, "additionalProperties": false} — which actively
// *rejects* the type's real, valid YAML rather than merely under-describing
// it). Keyed by struct name; the fragment replaces structToSchema's output
// entirely (any description from a doc comment is still merged in by the
// caller).
var manualSchemaOverrides = map[string]SchemaNode{
	// ForeachSource (internal/dsl/types.go) unmarshals as either a YAML
	// sequence of strings (Literal) or a bare string (Expr, a $param
	// reference or a template expression) — never as a mapping. It's used
	// both as `foreach.in` and as each matrix dimension's value.
	"ForeachSource": {
		"oneOf": []SchemaNode{
			{"type": "array", "items": SchemaNode{"type": "string"}},
			{"type": "string"},
		},
	},
	// MatrixDef (internal/dsl/types.go) unmarshals as a mapping where the
	// reserved "exclude" key holds a list of dimension-name→value filters
	// and every other key is a dimension name whose value is a
	// ForeachSource. Dimension names aren't known ahead of time, so
	// additionalProperties must allow them (typed as ForeachSource) rather
	// than being false.
	"MatrixDef": {
		"type": "object",
		"properties": SchemaNode{
			"exclude": SchemaNode{
				"type":  "array",
				"items": SchemaNode{"type": "object", "additionalProperties": SchemaNode{"type": "string"}},
			},
		},
		"additionalProperties": SchemaNode{"$ref": "#/definitions/ForeachSource"},
	},
}

func buildSchema(structs map[string]StructInfo, typeDescs map[string]string) SchemaNode {
	defs := SchemaNode{}

	// Generate a definition for every struct.
	names := sortedKeys(structs)
	for _, name := range names {
		var def SchemaNode
		if override, ok := manualSchemaOverrides[name]; ok {
			def = override
		} else {
			def = structToSchema(structs[name], structs)
		}
		if d := typeDescs[name]; d != "" {
			def["description"] = d
		}
		defs[name] = def
	}

	// Patch root kinds: pin apiVersion/kind to const values, and list them in
	// the document-level oneOf. Both are driven by the same derived list, so a
	// kind can never appear in one and be missing from the other.
	roots := deriveRoots(structs)
	var oneOf []SchemaNode
	for _, name := range roots {
		raw, ok := defs[name]
		if !ok {
			continue
		}
		def := raw.(SchemaNode)
		props := def["properties"].(SchemaNode)
		if _, ok := props["apiVersion"]; ok {
			props["apiVersion"] = SchemaNode{"const": "unified-cd/v1", "type": "string"}
		}
		if _, ok := props["kind"]; ok {
			props["kind"] = SchemaNode{"const": name, "type": "string"}
		}
		oneOf = append(oneOf, SchemaNode{"$ref": "#/definitions/" + name})
	}

	return SchemaNode{
		"$schema":     "http://json-schema.org/draft-07/schema#",
		"$id":         "https://unified-cd/schemas/unified-cd.schema.json",
		"title":       "unified-cd",
		"description": "unified-cd DSL schema — generated from internal/dsl/*_types.go",
		"oneOf":       oneOf,
		"definitions": defs,
	}
}

func structToSchema(info StructInfo, structs map[string]StructInfo) SchemaNode {
	props := SchemaNode{}
	var required []string

	for _, f := range info.Fields {
		node := fieldToSchema(f, structs)
		props[f.YAMLName] = node
		if f.Required {
			required = append(required, f.YAMLName)
		}
	}

	s := SchemaNode{
		"additionalProperties": false,
		"properties":           props,
		"type":                 "object",
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func fieldToSchema(f FieldInfo, structs map[string]StructInfo) SchemaNode {
	node := exprToSchema(f.Expr, structs)
	if len(f.Enums) > 0 {
		node["enum"] = f.Enums
	}
	if f.Desc != "" {
		node["description"] = f.Desc
	}
	return node
}

func exprToSchema(expr ast.Expr, structs map[string]StructInfo) SchemaNode {
	switch t := expr.(type) {
	case *ast.Ident:
		return identToSchema(t.Name, structs)
	case *ast.StarExpr:
		return exprToSchema(t.X, structs) // pointer = optional, same schema
	case *ast.ArrayType:
		return SchemaNode{"items": exprToSchema(t.Elt, structs), "type": "array"}
	case *ast.MapType:
		return SchemaNode{"additionalProperties": exprToSchema(t.Value, structs), "type": "object"}
	case *ast.InterfaceType:
		return SchemaNode{} // any
	case *ast.SelectorExpr:
		// e.g. time.Time, dsl.WorkspaceConfig
		if x, ok := t.X.(*ast.Ident); ok && x.Name == "dsl" {
			return SchemaNode{"$ref": "#/definitions/" + t.Sel.Name}
		}
		return SchemaNode{"type": "string"} // time.Time → string
	}
	return SchemaNode{}
}

func identToSchema(name string, structs map[string]StructInfo) SchemaNode {
	switch name {
	case "string":
		return SchemaNode{"type": "string"}
	case "bool":
		return SchemaNode{"type": "boolean"}
	case "int":
		return SchemaNode{"type": "integer"}
	case "float64":
		return SchemaNode{"type": "number"}
	case "any":
		return SchemaNode{}
	default:
		if _, ok := structs[name]; ok {
			return SchemaNode{"$ref": "#/definitions/" + name}
		}
		return SchemaNode{"type": "string"}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
