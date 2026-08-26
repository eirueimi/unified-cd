package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/xeipuuv/gojsonschema"
	"gopkg.in/yaml.v3"
)

// TestSchemaIsUpToDate regenerates the schema in-memory from
// internal/dsl/*_types.go and compares it byte-for-byte against the
// committed schemas/unified-cd.schema.json. Without this, a DSL struct
// change (a field added, an omitempty tag corrected, ...) can land without a
// `go generate ./internal/dsl/` pass and ship a stale schema to users'
// editors — exactly how spec.detached silently went missing from the
// schema for several commits before this test was added.
func TestSchemaIsUpToDate(t *testing.T) {
	root, err := projectRoot()
	require.NoError(t, err)

	structs, typeDescs, err := parseDSL(filepath.Join(root, "internal", "dsl"))
	require.NoError(t, err)
	schema := buildSchema(structs, typeDescs)
	want, err := json.MarshalIndent(schema, "", "  ")
	require.NoError(t, err)
	want = append(want, '\n')

	got, err := os.ReadFile(filepath.Join(root, "schemas", "unified-cd.schema.json"))
	require.NoError(t, err)

	require.Equal(t, string(want), string(got),
		"schemas/unified-cd.schema.json is stale — run `go generate ./internal/dsl/` and commit the result")
}

// TestExamplesValidateAgainstSchema validates every shipped example manifest
// under examples/jobs, examples/resources, and examples/self-monitoring (the
// same set internal/dsl.TestExamplesParse feeds through Parse) against the
// generated JSON Schema.
//
// The schema must never be stricter than the parser: a manifest the parser
// accepts has to validate too, or a real, working manifest gets red-squiggled
// in editors (VS Code's YAML extension, etc.) even though `unified-cd apply`
// accepts it fine. schemagen derives "required" from each Go struct field's
// yaml tag (missing omitempty + not a pointer => required), which is a
// hand-maintained annotation that can drift from what internal/dsl/parse.go
// actually enforces (as spec.params did — tagged required, but Validate
// never checks it's present). This test is the guard: any future drift of
// that kind fails CI here instead of reaching a user's editor.
func TestExamplesValidateAgainstSchema(t *testing.T) {
	root, err := projectRoot()
	require.NoError(t, err)

	schemaBytes, err := os.ReadFile(filepath.Join(root, "schemas", "unified-cd.schema.json"))
	require.NoError(t, err)
	schemaLoader := gojsonschema.NewBytesLoader(schemaBytes)

	var files []string
	for _, dir := range []string{"jobs", "resources", "self-monitoring"} {
		err := filepath.Walk(filepath.Join(root, "examples", dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && strings.HasSuffix(path, ".yaml") {
				files = append(files, path)
			}
			return nil
		})
		require.NoError(t, err)
	}
	require.NotEmpty(t, files, "expected to find example YAML files under examples/")

	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)

		docs := strings.Split(string(data), "\n---\n")
		for i, doc := range docs {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			var probe struct {
				Kind string `yaml:"kind"`
			}
			require.NoError(t, yaml.Unmarshal([]byte(doc), &probe))
			if probe.Kind == "" {
				continue // stray "---" separator with no document
			}

			var parsed any
			require.NoError(t, yaml.Unmarshal([]byte(doc), &parsed))
			jb, err := json.Marshal(parsed)
			require.NoError(t, err, "%s doc %d: yaml document is not JSON-representable", rel, i)

			result, err := gojsonschema.Validate(schemaLoader, gojsonschema.NewBytesLoader(jb))
			require.NoError(t, err)
			if !result.Valid() {
				var msgs []string
				for _, e := range result.Errors() {
					msgs = append(msgs, "  - "+e.String())
				}
				t.Errorf("%s doc %d (kind=%s) fails schema validation despite being a valid, shipped example:\n%s",
					rel, i, probe.Kind, strings.Join(msgs, "\n"))
			}
		}
	}
}
