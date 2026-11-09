package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
}

// A job can plant a symlink in the workspace and name it directly as an
// artifact/cache path. The lexical guard accepts it and os.OpenRoot follows a
// top-level symlink, so the resolver itself must refuse a target that leaves
// the workspace.
func TestHostBackend_ResolveArtifactPath_RejectsSymlinkLeavingWorkspace(t *testing.T) {
	a, _ := newSymlinkAgent(t)
	ws := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("top secret"), 0o600))
	mustSymlink(t, secret, filepath.Join(ws, "filelink"))
	mustSymlink(t, outside, filepath.Join(ws, "dirlink"))
	mustSymlink(t, filepath.Join(outside, "missing"), filepath.Join(ws, "danglinglink"))
	b := newHostBackend(a, "r1", "job", ws, nil)

	for _, p := range []string{"filelink", "dirlink", "dirlink/secret.txt", "danglinglink", "dirlink/new/file"} {
		_, err := b.ResolveArtifactPath(ScopeHandle{}, p)
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "escapes the workspace", p)
	}
}

func TestHostBackend_ResolveArtifactPath_AllowsInWorkspaceLinksAndNewPaths(t *testing.T) {
	a, _ := newSymlinkAgent(t)
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "real"), 0o755))
	mustSymlink(t, "real", filepath.Join(ws, "alias"))
	b := newHostBackend(a, "r1", "job", ws, nil)

	for _, p := range []string{"real", "alias", "alias/x", "not/yet/created", ".", ""} {
		got, err := b.ResolveArtifactPath(ScopeHandle{}, p)
		require.NoError(t, err, p)
		assert.NotEmpty(t, got, p)
	}
}
