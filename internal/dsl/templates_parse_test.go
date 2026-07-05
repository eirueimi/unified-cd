package dsl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTemplatesParse ensures every reusable job template under templates/
// (repo root, sibling of internal/dsl) parses successfully via dsl.Parse.
// Run with: go test ./internal/dsl/ -run Templates -v
func TestTemplatesParse(t *testing.T) {
	matches, err := filepath.Glob("../../templates/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected to find template YAML files under templates/")

	for _, path := range matches {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			require.NoError(t, err)
			defer f.Close()

			job, err := Parse(f)
			require.NoError(t, err, "template %s failed to parse", path)
			assert.NotEmpty(t, job.Metadata.Name, "template %s has empty metadata.name", path)
			assert.NotEmpty(t, job.Spec.Steps, "template %s has no steps", path)
		})
	}
}
