package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Task 4 removed three identifiers. A leftover mention in a compose file or a
// deployment guide silently instructs operators to set a variable that no
// longer does anything, so the absence is enforced rather than trusted.
func TestNoStaleControllerKeyReferences(t *testing.T) {
	removed := []string{"UNIFIED_CONTROLLER_KEY", "controllerKey", "controller_key_hex"}

	repoRoot, err := filepath.Abs("../..")
	require.NoError(t, err)

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true,
		"web": true, // built bundles contain arbitrary minified text
		// Gitignored SDD workflow scratch space: per-task briefs and reports
		// describing this very removal, which must keep the old names to stay
		// readable. Never committed, so it can't leave a stale reference behind.
		".superpowers": true,
	}
	// Design and plan documents describe the removal itself and must keep the
	// old names to stay readable.
	skipPathFragments := []string{
		filepath.Join("docs", "superpowers"),
		// Already-applied migrations are immutable historical DDL: 001_init
		// created the column under its old name, and 015_secrets_v2 drops it
		// by that same name (a DROP COLUMN statement cannot omit the name of
		// the column it drops). Rewriting shipped migrations is unsafe.
		filepath.Join("internal", "store", "migrations"),
	}

	var offenders []string
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if skipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		for _, frag := range skipPathFragments {
			if strings.Contains(path, frag) {
				return nil
			}
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".yaml", ".yml", ".sql", ".example", ".sh":
		default:
			if filepath.Base(path) != ".env.example" {
				return nil
			}
		}
		if strings.HasSuffix(path, "stale_refs_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		content := string(data)
		for _, needle := range removed {
			if needle == "UNIFIED_CONTROLLER_KEY" {
				if containsBareControllerKey(content) {
					rel, _ := filepath.Rel(repoRoot, path)
					offenders = append(offenders, rel+" contains "+needle)
				}
				continue
			}
			if strings.Contains(content, needle) {
				rel, _ := filepath.Rel(repoRoot, path)
				offenders = append(offenders, rel+" contains "+needle)
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, offenders, "these files still reference removed key configuration:\n%s",
		strings.Join(offenders, "\n"))
}

// containsBareControllerKey reports whether content mentions the removed
// UNIFIED_CONTROLLER_KEY identifier — in code (`UNIFIED_CONTROLLER_KEY=`,
// `UNIFIED_CONTROLLER_KEY:`) or in prose (backtick-quoted, plain text) —
// while excluding the still-valid UNIFIED_CONTROLLER_KEY_FILE.
func containsBareControllerKey(content string) bool {
	const needle = "UNIFIED_CONTROLLER_KEY"
	const suffix = "_FILE"
	start := 0
	for {
		idx := strings.Index(content[start:], needle)
		if idx == -1 {
			return false
		}
		abs := start + idx
		rest := content[abs+len(needle):]
		if !strings.HasPrefix(rest, suffix) {
			return true
		}
		start = abs + len(needle)
	}
}
