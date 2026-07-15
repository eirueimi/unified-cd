package agent

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContainWithinSlash(t *testing.T) {
	root := "/workspace"
	ok := map[string]string{
		"":              "/workspace",
		"node_modules":  "/workspace/node_modules",
		"a/b/c":         "/workspace/a/b/c",
		"foo/../bar":    "/workspace/bar", // stays in bounds after cleaning
		"./dist":        "/workspace/dist",
	}
	// A non-canonical root (trailing slash) must still work: root is cleaned.
	got, err := ContainWithinSlash("/workspace/", "foo")
	require.NoError(t, err)
	assert.Equal(t, "/workspace/foo", got)
	for in, want := range ok {
		got, err := ContainWithinSlash(root, in)
		require.NoError(t, err, "input %q", in)
		assert.Equal(t, want, got, "input %q", in)
	}
	bad := []string{"../etc/passwd", "..", "a/../../b", "/etc/passwd", "/workspace/../x"}
	for _, in := range bad {
		_, err := ContainWithinSlash(root, in)
		require.Error(t, err, "input %q must be rejected", in)
		assert.Contains(t, err.Error(), "escapes the workspace", "input %q", in)
	}
}

func TestContainWithinOS(t *testing.T) {
	// Use a slash root; on Windows filepath still treats it as relative-to-drive
	// but the containment logic (prefix of cleaned join) holds for the in-bounds
	// cases and rejects the traversal cases regardless of separator.
	root := "/tmp/ws"
	got, err := ContainWithinOS(root, "node_modules")
	require.NoError(t, err)
	assert.Equal(t, filepathJoin(root, "node_modules"), got)

	_, err = ContainWithinOS(root, "../../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the workspace")

	// Absolute is rejected.
	_, err = ContainWithinOS(root, absForTest())
	require.Error(t, err)
}

func filepathJoin(a, b string) string { return filepath.Join(a, b) }
func absForTest() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\system32`
	}
	return "/etc/passwd"
}
