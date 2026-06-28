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

func makeTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "test")

	jobsDir := filepath.Join(dir, "jobs")
	require.NoError(t, os.MkdirAll(jobsDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(jobsDir, "build.yaml"), []byte(`
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  steps:
    - name: compile
      run: go build ./...
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(jobsDir, "README.md"), []byte("not yaml"), 0o644))

	run("add", ".")
	run("commit", "-m", "init")
	return dir
}

func TestFetcher_ResolveCommitSHA(t *testing.T) {
	repoDir := makeTestRepo(t)
	f := gittemplate.NewFetcher()
	sha, err := f.ResolveCommitSHA(context.Background(), "file://"+repoDir, "main", "", "")
	require.NoError(t, err)
	assert.Len(t, sha, 40, "should be a full SHA")
}

func TestFetcher_FetchDir(t *testing.T) {
	repoDir := makeTestRepo(t)
	f := gittemplate.NewFetcher()
	files, err := f.FetchDir(context.Background(), "file://"+repoDir, "main", "jobs/", "", "")
	require.NoError(t, err)
	assert.Len(t, files, 1, "only .yaml files, not README.md")
	_, found := files["jobs/build.yaml"]
	assert.True(t, found, "jobs/build.yaml should be in result")
}
