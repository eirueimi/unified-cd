package gittemplate_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/gittemplate"
)

// setupBareRepo creates a local git repo with the given files and returns "file://<workDir>".
func setupBareRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	require.NoError(t, os.MkdirAll(work, 0o755))

	for rel, content := range files {
		full := filepath.Join(work, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}

	for _, args := range [][]string{
		{"init", work},
		{"-C", work, "config", "user.email", "test@test.com"},
		{"-C", work, "config", "user.name", "Test"},
		{"-C", work, "add", "."},
		{"-C", work, "commit", "-m", "init"},
		{"-C", work, "tag", "v1.0.0"},
	} {
		out, err := exec.Command("git", args...).CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}

	return "file://" + work
}

func TestFetcher_FetchWithURL_Success(t *testing.T) {
	const jobYAML = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hello
`
	repoURL := setupBareRepo(t, map[string]string{"jobs/hello.yaml": jobYAML})

	f := gittemplate.NewFetcher()
	data, err := f.FetchWithURL(context.Background(), repoURL, "jobs/hello.yaml", "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, jobYAML, string(data))
}

func TestFetcher_FetchWithURL_MissingFile(t *testing.T) {
	const jobYAML = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hello
`
	repoURL := setupBareRepo(t, map[string]string{"jobs/hello.yaml": jobYAML})

	f := gittemplate.NewFetcher()
	_, err := f.FetchWithURL(context.Background(), repoURL, "does/not/exist.yaml", "v1.0.0")
	require.Error(t, err)
}
