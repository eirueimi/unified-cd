//go:build !windows

package agent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// The read fallback (hostBackend.readWithFallback) triggers on
// errors.Is(err, fs.ErrPermission). This pins that the real upload and cache
// save entry points keep the permission error unwrappable through their
// streaming pipelines, so a lost %w cannot silently disable the fallback.
func TestPermissionErrorSurvivesUploadAndCacheSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 file")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret"), []byte("x"), 0); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer srv.Close()

	err := NewClient(srv.URL, "tok").UploadArtifactExcluding(context.Background(), "r1", "a", dir, nil)
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("UploadArtifactExcluding error does not wrap fs.ErrPermission: %v", err)
	}

	err = cache.SaveExcluding(context.Background(), objectstore.NewLocalObjectStore(t.TempDir()), "job", dir, "k", 1, nil)
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("cache.SaveExcluding error does not wrap fs.ErrPermission: %v", err)
	}
}
