package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dslschema"
	"github.com/stretchr/testify/require"
)

func TestCellText(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain":           {"a plain description", "a plain description"},
		"newlines":        {"wraps over\nthree\n  lines", "wraps over three lines"},
		"blank line":      {"one\n\ntwo", "one two"},
		"angle brackets":  {"path from\n$UNIFIED_VOLUME_<AS>: upper-cased", "path from $UNIFIED_VOLUME_&lt;AS&gt;: upper-cased"},
		"a select":        {"rendered as a <select>.", "rendered as a &lt;select&gt;."},
		"pipe":            {"none | hmac-sha256", `none \| hmac-sha256`},
		"code span":       {"host: `<rt> run`; k8s", "host: `<rt> run`; k8s"},
		"pipe in code":    {"use `a|b` here", "use `a\\|b` here"},
		"after a code":    {"`x` then <y>", "`x` then &lt;y&gt;"},
		"comparison":      {"Must be >= 1.", "Must be &gt;= 1."},
		"empty":           {"", ""},
		"only whitespace": {" \n\t ", ""},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, cellText(tc.in))
		})
	}
}

// A description that wraps over several lines must not end the table early: every
// property of a type is a row of that type's table.
func TestWriteSectionKeepsEveryRowOfATableWhoseDescriptionsWrap(t *testing.T) {
	def := map[string]any{"properties": map[string]any{
		"a": map[string]any{"type": "string", "description": "first line\nsecond line"},
		"b": map[string]any{"type": "string", "description": "uses $X_<AS> and a | bar"},
		"c": map[string]any{"type": "string"},
	}}
	var sb strings.Builder
	writeSection(&sb, "T", def, map[string]any{}, map[string]bool{})
	lines := strings.Split(strings.TrimSpace(sb.String()), "\n")
	require.Len(t, lines, 5, "a header, a rule and one line per property")
	for _, l := range lines {
		require.True(t, strings.HasPrefix(l, "|") && strings.HasSuffix(l, "|"), "every line is a table row: %q", l)
	}
	require.Contains(t, sb.String(), "$X_&lt;AS&gt; and a \\| bar")
}

// TestFieldReferenceIsUpToDate regenerates docs/reference/field-reference.md
// in memory — parsing internal/dsl/*_types.go and building the schema the
// same way schemagen does, then rendering it through generateDoc — and
// compares the result against the committed file. Without this, a DSL
// struct change (a field added, a doc comment edited, ...) can land
// without a `go generate ./internal/dsl/` pass and ship a stale field
// reference to readers — exactly how spec.detached silently went missing
// from both the schema AND the field reference for several commits before
// any guard existed. cmd/schemagen/main_test.go's TestSchemaIsUpToDate now
// guards schemas/unified-cd.schema.json against internal/dsl; this test
// closes the other half by guarding docs/reference/field-reference.md
// against internal/dsl too.
//
// This test regenerates the schema itself in memory (via
// internal/dslschema, the package factored out of cmd/schemagen so both
// generators' tests can build the same in-memory schema) rather than
// reading the committed schemas/unified-cd.schema.json off disk. That
// keeps this guard independent of TestSchemaIsUpToDate: a DSL edit fails
// THIS test on its own, without relying on the schema guard having run
// first or the committed schema.json being fresh.
//
// They stay two separate tests, not one, because they guard two
// independently-committed artifacts. Either file can go stale on its own
// (a hand-edit to one, a generator bug touching only one, a partial
// `go generate` run) and a reader needs to know which committed file is
// wrong, not just that "generation drifted" — collapsing them into a
// single test would blur that. Both do share the same parsing/building
// step now, via internal/dslschema, so the two are consistent about what
// "current" means; only the rendering (JSON Schema vs. Markdown table) and
// the committed file compared against differ.
//
// As with TestSchemaIsUpToDate, the comparison normalizes CRLF to LF on
// both sides before comparing: docgen always writes '\n'-only bytes, but
// this repo has no .gitattributes, so a Windows checkout with the common
// core.autocrlf=true rewrites the committed LF bytes to CRLF on disk at
// checkout time. That is a checkout-filter artifact, not generator output,
// so it is not part of what "stale" means here.
func TestFieldReferenceIsUpToDate(t *testing.T) {
	root, err := projectRoot()
	require.NoError(t, err)

	structs, typeDescs, err := dslschema.ParseDSL(filepath.Join(root, "internal", "dsl"))
	require.NoError(t, err)
	built := dslschema.BuildSchema(structs, typeDescs)

	// Round-trip through JSON, same as the real pipeline: schemagen
	// marshals this struct to bytes and docgen un-marshals those bytes back
	// into a plain map[string]any. Skipping that round trip here would feed
	// generateDoc types it never sees in production (e.g. []dslschema.SchemaNode
	// where real callers get []any from json.Unmarshal), which breaks type
	// assertions like schemakinds.RootKinds' schema["oneOf"].([]any).
	rawSchema, err := json.Marshal(built)
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(rawSchema, &schema))

	want, err := generateDoc(schema)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(root, "docs", "reference", "field-reference.md"))
	require.NoError(t, err)

	normalize := func(b []byte) string {
		return strings.ReplaceAll(string(b), "\r\n", "\n")
	}

	require.Equal(t, normalize([]byte(want)), normalize(got),
		"docs/reference/field-reference.md is stale — run `go generate ./internal/dsl/` and commit the result")
}
