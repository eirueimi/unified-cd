package dsl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// TestExamplesParse ensures every example YAML under examples/ (repo root,
// sibling of internal/dsl) parses successfully via the appropriate dsl
// Parse* function for its "kind:". Multi-document files (separated by
// "\n---\n") are split and each document is routed independently.
// Run with: go test ./internal/dsl/ -run Examples -v
func TestExamplesParse(t *testing.T) {
	var matches []string
	for _, pattern := range []string{
		"../../examples/jobs/**/*.yaml",
		"../../examples/resources/*.yaml",
		"../../examples/self-monitoring/*.yaml",
	} {
		m, err := filepath.Glob(pattern)
		require.NoError(t, err)
		matches = append(matches, m...)
	}
	// examples/jobs has subdirectories (k8s/, team-a/) that filepath.Glob's
	// "**" does not recurse into (Go's glob has no true globstar support),
	// so walk examples/jobs explicitly as well.
	err := filepath.Walk("../../examples/jobs", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".yaml") {
			matches = append(matches, path)
		}
		return nil
	})
	require.NoError(t, err)

	require.NotEmpty(t, matches, "expected to find example YAML files under examples/")

	seen := map[string]bool{}
	for _, path := range matches {
		path := path
		norm := filepath.ToSlash(path)
		if seen[norm] {
			continue
		}
		seen[norm] = true

		t.Run(norm, func(t *testing.T) {
			data, err := os.ReadFile(path)
			require.NoError(t, err)

			docs := strings.Split(string(data), "\n---\n")
			for i, doc := range docs {
				doc := strings.TrimSpace(doc)
				if doc == "" {
					continue
				}

				var kindProbe struct {
					Kind string `yaml:"kind"`
				}
				if err := yaml.Unmarshal([]byte(doc), &kindProbe); err != nil {
					t.Fatalf("doc %d: could not probe kind: %v", i, err)
				}
				if kindProbe.Kind == "" {
					continue // skip docs with no kind (e.g. stray "---" separators)
				}

				var parseErr error
				switch kindProbe.Kind {
				case "Job":
					_, parseErr = Parse(strings.NewReader(doc))
				case "WebhookReceiver":
					_, parseErr = ParseWebhookReceiver(strings.NewReader(doc))
				case "AppSource":
					_, parseErr = ParseAppSource(strings.NewReader(doc))
				case "Schedule":
					_, parseErr = ParseSchedule(strings.NewReader(doc))
				case "GitCredential":
					_, parseErr = ParseGitCredential(strings.NewReader(doc))
				default:
					t.Fatalf("doc %d: unknown kind %q", i, kindProbe.Kind)
				}
				require.NoError(t, parseErr, "doc %d (kind=%s) in %s failed to parse", i, kindProbe.Kind, path)
			}
		})
	}
}
