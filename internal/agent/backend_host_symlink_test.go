package agent

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// nodeModulesTree builds a workspace subtree the way npm leaves it: real
// packages plus .bin symlinks, and a dangling link.
func nodeModulesTree(t *testing.T, root string) string {
	t.Helper()
	nm := filepath.Join(root, "node_modules")
	require.NoError(t, os.MkdirAll(filepath.Join(nm, "tool", "bin"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(nm, ".bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nm, "tool", "bin", "cli.js"), []byte("#!/usr/bin/env node\n"), 0o755))
	for link, target := range map[string]string{
		filepath.Join(nm, ".bin", "tool"): filepath.Join("..", "tool", "bin", "cli.js"),
		filepath.Join(nm, "tool-dir"):     "tool",
		filepath.Join(nm, "dangling"):     "missing",
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("cannot create symlinks here: %v", err)
		}
	}
	return nm
}

type symlinkServer struct {
	mu       sync.Mutex
	uploaded []byte
	logs     string
}

func newSymlinkAgent(t *testing.T) (*Agent, *symlinkServer) {
	t.Helper()
	s := &symlinkServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/runs/{run}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.uploaded = b
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/runs/{run}/artifacts/{name}", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, _ = w.Write(s.uploaded)
	})
	mux.HandleFunc("POST /api/v1/agents/a1/runs/{run}/steps/{idx}/logs/bulk", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.logs += string(b)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Agent{ID: "a1", Client: NewClient(srv.URL, "tok"), CacheStore: objectstore.NewLocalObjectStore(t.TempDir())}, s
}

func tarTypes(t *testing.T, archive []byte) map[string]byte {
	t.Helper()
	dec, err := zstd.NewReader(bytes.NewReader(archive))
	require.NoError(t, err)
	defer dec.Close()
	tr := tar.NewReader(dec)
	got := map[string]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err)
		got[h.Name] = h.Typeflag
	}
}

func TestHostBackend_UploadArtifact_TreeWithSymlinksSucceeds(t *testing.T) {
	a, srv := newSymlinkAgent(t)
	ws := t.TempDir()
	nm := nodeModulesTree(t, ws)
	b := newHostBackend(a, "r1", "job", ws, nil)

	require.NoError(t, b.UploadArtifact(context.Background(), ScopeHandle{}, "r1", "deps", nm))

	types := tarTypes(t, srv.uploaded)
	assert.Equal(t, byte(tar.TypeSymlink), types[".bin/tool"])
	assert.Equal(t, byte(tar.TypeSymlink), types["tool-dir"])
	assert.Equal(t, byte(tar.TypeSymlink), types["dangling"])
	assert.Equal(t, byte(tar.TypeReg), types["tool/bin/cli.js"])
}

func TestHostBackend_DownloadArtifact_RefusesLinkEntries(t *testing.T) {
	a, _ := newSymlinkAgent(t)
	ws := t.TempDir()
	nm := nodeModulesTree(t, ws)
	b := newHostBackend(a, "r1", "job", ws, nil)
	require.NoError(t, b.UploadArtifact(context.Background(), ScopeHandle{}, "r1", "deps", nm))

	err := b.DownloadArtifact(context.Background(), ScopeHandle{}, "r1", "deps", filepath.Join(ws, "out"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported symlink entry")
}

func TestHostBackend_CacheSaveRestore_SymlinksSkippedWithRunLogWarning(t *testing.T) {
	ctx := context.Background()
	a, srv := newSymlinkAgent(t)
	ws := t.TempDir()
	nm := nodeModulesTree(t, ws)
	b := newHostBackend(a, "r1", "job", ws, nil)

	require.NoError(t, b.CacheSave(ctx, ScopeHandle{}, "k", nm, 1))

	dest := filepath.Join(ws, "restored")
	hit, err := b.CacheRestore(ctx, ScopeHandle{}, "k", nil, dest)
	require.NoError(t, err)
	assert.True(t, hit, "a restore that skipped links is still a hit, as on the k8s agent")

	got, err := os.ReadFile(filepath.Join(dest, "tool", "bin", "cli.js"))
	require.NoError(t, err)
	assert.Equal(t, "#!/usr/bin/env node\n", string(got))
	_, err = os.Lstat(filepath.Join(dest, ".bin", "tool"))
	assert.ErrorIs(t, err, os.ErrNotExist)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	assert.Contains(t, srv.logs, artifact.SkippedLinksWarning(3))
}

// A symlink already in the destination that leads outside it is never written
// through by a cache restore or an artifact download.
func TestHostBackend_Extraction_NeverWritesThroughAnOutsideSymlink(t *testing.T) {
	ctx := context.Background()
	a, _ := newSymlinkAgent(t)
	ws := t.TempDir()
	src := filepath.Join(ws, "src")
	require.NoError(t, os.MkdirAll(filepath.Join(src, "link"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "link", "x"), []byte("pwned"), 0o644))
	b := newHostBackend(a, "r1", "job", ws, nil)
	require.NoError(t, b.CacheSave(ctx, ScopeHandle{}, "k", src, 1))
	require.NoError(t, b.UploadArtifact(ctx, ScopeHandle{}, "r1", "art", src))

	for name, restore := range map[string]func(dest string) error{
		"cache": func(dest string) error {
			_, err := b.CacheRestore(ctx, ScopeHandle{}, "k", nil, dest)
			return err
		},
		"artifact": func(dest string) error { return b.DownloadArtifact(ctx, ScopeHandle{}, "r1", "art", dest) },
	} {
		outside := t.TempDir()
		dest := filepath.Join(ws, "dest-"+name)
		require.NoError(t, os.MkdirAll(dest, 0o755))
		if err := os.Symlink(outside, filepath.Join(dest, "link")); err != nil {
			t.Skipf("cannot create symlinks here: %v", err)
		}
		assert.Error(t, restore(dest), name)
		entries, err := os.ReadDir(outside)
		require.NoError(t, err)
		assert.Empty(t, entries, name+": nothing may be written outside the destination")
	}
}

// What a restore creates, and only that, is handed to chownAgentWrite: the new
// files, not the skipped links and not what was already there.
func TestHostBackend_CacheRestore_ChownsTheFilesItCreated(t *testing.T) {
	ctx := context.Background()
	f := &podFakeRT{}
	b, workDir := podBackend(t, downloadAgent(t), f, permPT(map[string]any{"runAsUser": 1000}, nil))

	src := filepath.Join(workDir, "src")
	require.NoError(t, os.MkdirAll(src, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(src, "dep"), []byte("d"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(src, "old"), []byte("n"), 0o644))
	if err := os.Symlink("dep", filepath.Join(src, "alias")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	require.NoError(t, b.CacheSave(ctx, ScopeHandle{}, "k", src, 1))

	dest := filepath.Join(workDir, "deps")
	require.NoError(t, os.MkdirAll(dest, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "old"), []byte("mine"), 0o644))
	hit, err := b.CacheRestore(ctx, ScopeHandle{}, "k", nil, dest)
	require.NoError(t, err)
	require.True(t, hit)

	got := strings.Join(chownExecs(f), "\n")
	assert.Contains(t, got, "/workspace/deps/dep")
	assert.NotContains(t, got, "/workspace/deps/alias")
	assert.NotContains(t, got, "/workspace/deps/old")
}
