package agent

import (
	"context"
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

// isolatedLinkBackend is an isolated claim backend whose workspace is workDir.
func isolatedLinkBackend(t *testing.T) (*hostBackend, string) {
	t.Helper()
	a, _ := newSymlinkAgent(t)
	b, ws := podBackend(t, a, &podFakeRT{}, nil)
	return b, ws
}

// A job can plant a symlink in the workspace and name it directly as an
// artifact/cache path. The lexical guard accepts it and os.OpenRoot follows a
// top-level symlink, so the resolver itself must refuse a target that leaves
// the workspace.
func TestHostBackend_ResolveArtifactPath_RejectsSymlinkLeavingWorkspace(t *testing.T) {
	b, ws := isolatedLinkBackend(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(secret, []byte("top secret"), 0o600))
	mustSymlink(t, secret, filepath.Join(ws, "filelink"))
	mustSymlink(t, outside, filepath.Join(ws, "dirlink"))
	mustSymlink(t, filepath.Join(outside, "missing"), filepath.Join(ws, "danglinglink"))

	for _, p := range []string{"filelink", "dirlink", "dirlink/secret.txt", "danglinglink", "dirlink/new/file"} {
		_, err := b.ResolveArtifactPath(ScopeHandle{}, p)
		require.Error(t, err, p)
		assert.Contains(t, err.Error(), "escapes the workspace", p)
	}
}

// A symlink planted inside a named volume leads out of the volume's host
// directory, which is outside the workspace: it is confined to the volume.
func TestHostBackend_ResolveArtifactPath_ConfinesPathsInsideAVolume(t *testing.T) {
	b, _ := isolatedLinkBackend(t)
	volDir := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(volDir, "real"), 0o755))
	mustSymlink(t, outside, filepath.Join(volDir, "link"))
	mustSymlink(t, "real", filepath.Join(volDir, "alias"))
	b.volumeRefs = []volumeMountRef{{Volume: "v", MountPath: "vol", Rel: "vol", HostPath: volDir}}

	_, err := b.ResolveArtifactPath(ScopeHandle{}, "vol/link")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "escapes the workspace")
	_, err = b.ResolveArtifactPath(ScopeHandle{}, "vol/link/x")
	require.Error(t, err)

	got, err := b.ResolveArtifactPath(ScopeHandle{}, "vol/alias")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(volDir, "real"), got)
}

func TestHostBackend_ResolveArtifactPath_AllowsInWorkspaceLinksAndNewPaths(t *testing.T) {
	b, ws := isolatedLinkBackend(t)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "real"), 0o755))
	mustSymlink(t, "real", filepath.Join(ws, "alias"))

	for _, p := range []string{"real", "alias", "alias/x", "not/yet/created", ".", ""} {
		got, err := b.ResolveArtifactPath(ScopeHandle{}, p)
		require.NoError(t, err, p)
		assert.NotEmpty(t, got, p)
	}
}

// A native claim has no boundary to protect and may deliberately link a
// directory outside the workspace (out -> /opt/build), so it is not confined.
func TestHostBackend_ResolveArtifactPath_NativeClaimIsNotConfined(t *testing.T) {
	a, _ := newSymlinkAgent(t)
	ws := t.TempDir()
	outside := t.TempDir()
	mustSymlink(t, outside, filepath.Join(ws, "out"))
	b := newHostBackend(a, "r1", "job", ws, nil)

	got, err := b.ResolveArtifactPath(ScopeHandle{}, "out")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(ws, "out"), got)
}

// The resolver's check and the archive/extract open are separate steps, so a
// background process in the claim pod could swap a directory for a symlink in
// between. The open itself is confined: handing the layer an unresolved path
// that goes through an escaping symlink must fail, not follow it.
func TestHostBackend_Transfer_OpenIsConfinedEvenWithoutTheUpFrontCheck(t *testing.T) {
	ctx := context.Background()
	b, ws := isolatedLinkBackend(t)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("top secret"), 0o600))
	mustSymlink(t, outside, filepath.Join(ws, "swapped"))

	err := b.UploadArtifact(ctx, ScopeHandle{}, "r1", "leak", filepath.Join(ws, "swapped", "secret.txt"))
	require.Error(t, err, "upload must not read through a symlink that leaves the workspace")

	err = b.CacheSave(ctx, ScopeHandle{}, "k", filepath.Join(ws, "swapped", "secret.txt"), 1)
	require.Error(t, err, "cache save must not read through a symlink that leaves the workspace")

	// A download into a path below the symlink must not write outside.
	src := filepath.Join(ws, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644))
	require.NoError(t, b.UploadArtifact(ctx, ScopeHandle{}, "r1", "good", src))
	err = b.DownloadArtifact(ctx, ScopeHandle{}, "r1", "good", filepath.Join(ws, "swapped", "dest"))
	require.Error(t, err)
	entries, rerr := os.ReadDir(outside)
	require.NoError(t, rerr)
	assert.Len(t, entries, 1, "nothing may be written outside the workspace")
}

func TestResolveWorkspaceDir_IsAbsolute(t *testing.T) {
	got, err := ResolveWorkspaceDir("rel/ws")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got), got)
}

// A relative --workspace-dir must not change where the confined transfers read
// and write: the upload/cache paths are cwd-relative, the confined archivers
// read relative paths as root-relative.
func TestHostBackend_Transfers_WithARelativeWorkspaceDir(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	a, _ := newSymlinkAgent(t)
	a.WorkspaceDir = "ws"
	// Deliberately cwd-relative: defence in depth behind ResolveWorkspaceDir
	// making the base absolute.
	workDir := filepath.Join("ws", "working0", "job")
	absWork, err := filepath.Abs(workDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	m := newClaimPodManager(&podFakeRT{}, workDir, "/workspace", "pause:img", "runner:img", "/host/tools")
	require.NoError(t, m.Start(ctx, nil))
	b := newHostBackend(a, "r1", "job", workDir, m)

	require.NoError(t, os.MkdirAll(filepath.Join(workDir, "build"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "build", "out.txt"), []byte("hello"), 0o644))

	p, err := b.ResolveArtifactPath(ScopeHandle{}, "build")
	require.NoError(t, err)
	require.NoError(t, b.UploadArtifact(ctx, ScopeHandle{}, "r1", "art", p))
	require.NoError(t, b.CacheSave(ctx, ScopeHandle{}, "k", p, 1))

	dl, err := b.ResolveArtifactPath(ScopeHandle{}, "dl")
	require.NoError(t, err)
	require.NoError(t, b.DownloadArtifact(ctx, ScopeHandle{}, "r1", "art", dl))
	got, err := os.ReadFile(filepath.Join(absWork, "dl", "out.txt"))
	require.NoError(t, err, "the download must land at the resolved destination")
	assert.Equal(t, "hello", string(got))

	rs, err := b.ResolveCachePath(ScopeHandle{}, "restored")
	require.NoError(t, err)
	hit, err := b.CacheRestore(ctx, ScopeHandle{}, "k", nil, rs)
	require.NoError(t, err)
	require.True(t, hit)
	got, err = os.ReadFile(filepath.Join(absWork, "restored", "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
}
