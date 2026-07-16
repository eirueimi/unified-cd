package dsl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// standaloneJobTemplates lists files under templates/ that are deliberately
// kind: Job — standalone jobs meant to be registered with `apply` (and run
// directly or via call:), NOT uses: targets. buildkit-rootless-build-push
// replaces the primary "job" container in its own podTemplate, which uses:
// forbids (reserved container name), so it can only work as a standalone job.
var standaloneJobTemplates = map[string]bool{
	"buildkit-rootless-build-push.yaml": true,
}

// TestTemplatesParse ensures every file under templates/ (repo root, sibling
// of internal/dsl) parses under its intended contract: uses: templates via
// ParseJobTemplate (kind: JobTemplate, strict schema), and the documented
// standalone exceptions via Parse (kind: Job). This is the drift gate: a
// template that silently gains an unsupported field, or a new template with
// the wrong kind, fails here instead of at a user's run creation.
// Run with: go test ./internal/dsl/ -run Templates -v
func TestTemplatesParse(t *testing.T) {
	matches, err := filepath.Glob("../../templates/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected to find template YAML files under templates/")

	for _, path := range matches {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			require.NoError(t, err)

			if standaloneJobTemplates[filepath.Base(path)] {
				f, err := os.Open(path)
				require.NoError(t, err)
				defer f.Close()
				job, err := Parse(f)
				require.NoError(t, err, "standalone job template %s failed to parse as kind: Job", path)
				assert.NotEmpty(t, job.Metadata.Name, "template %s has empty metadata.name", path)
				assert.NotEmpty(t, job.Spec.Steps, "template %s has no steps", path)
				return
			}

			tpl, err := ParseJobTemplate(data)
			require.NoError(t, err, "uses: template %s failed to parse as kind: JobTemplate", path)
			assert.NotEmpty(t, tpl.Metadata.Name, "template %s has empty metadata.name", path)
			assert.NotEmpty(t, tpl.Spec.Steps, "template %s has no steps", path)
		})
	}
}
