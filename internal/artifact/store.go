package artifact

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// WriteTarZstd archives path (a directory OR a single file) and streams it to w
// as a tar+zstd archive. For a directory, entries are named relative to the
// directory; for a single file, the archive contains one entry named the file's
// base name (so `path: out.txt` round-trips to `out.txt`, matching the docs).
func WriteTarZstd(w io.Writer, path string) error {
	return WriteTarZstdExcluding(w, path, nil)
}

// WriteTarZstdExcluding is WriteTarZstd that leaves out every path in exclude:
// the entry itself and, for a directory, everything below it. Paths are
// compared after filepath.Clean, in the same form as path (both absolute, or
// both relative to the same directory). An exclusion that does not exist under
// path is simply never met. A symlink in the tree is archived as a symlink
// entry and never followed, so a dangling link or a link to a directory is
// fine; this is the same archive the Kubernetes sidecar writes.
func WriteTarZstdExcluding(w io.Writer, path string, exclude []string) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", path, err)
	}
	absExclude := make([]string, 0, len(exclude))
	for _, e := range exclude {
		abs, err := filepath.Abs(e)
		if err != nil {
			return fmt.Errorf("resolve excluded path %q: %w", e, err)
		}
		absExclude = append(absExclude, abs)
	}
	// The archived path is its own root, so the walk can never leave it.
	root := absPath
	if info, err := os.Stat(absPath); err == nil && !info.IsDir() {
		root = filepath.Dir(absPath)
	}
	return WriteTarZstdIn(w, root, absPath, absExclude)
}

// StreamTarZstd returns an io.ReadCloser that yields the tar+zstd archive of
// path, produced by a background goroutine writing into an io.Pipe, so the
// whole archive is never held in memory. A production error from WriteTarZstd
// surfaces to the consumer as a read error (via pw.CloseWithError). Callers
// MUST Close the returned reader — even on an early abort such as an HTTP 4xx
// or a failed Put — so the producer goroutine cannot leak: Close delivers
// io.ErrClosedPipe to the producer's next Write, unwinding it.
func StreamTarZstd(path string) io.ReadCloser {
	return StreamTarZstdExcluding(path, nil)
}

// StreamTarZstdExcluding is StreamTarZstd with the exclusions of
// WriteTarZstdExcluding.
func StreamTarZstdExcluding(path string, exclude []string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		// CloseWithError(nil) behaves like Close(): the reader sees a clean io.EOF.
		pw.CloseWithError(WriteTarZstdExcluding(pw, path, exclude))
	}()
	return pr
}

// isSafeArtifactPathSegment reports whether s is safe to use as a single path
// segment in an object-store key: non-empty, containing no path separators
// and no "..", so it can never introduce or traverse into another directory
// component of the key.
func isSafeArtifactPathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	if strings.ContainsAny(s, "/\\") {
		return false
	}
	return true
}

// ArtifactKey builds the object-store key for an artifact. It is defensive
// regardless of upstream validation (internal/dsl already rejects unsafe
// uploadArtifact/downloadArtifact names at parse time, but this is the last
// line of defense against path traversal — see #26): runID and name are
// required to be plain, single path segments with no "/", "\\", or "..". A
// name/runID that satisfies that constraint produces the exact same key as
// before this fix, so already-stored artifacts with plain names are
// unaffected.
func ArtifactKey(runID, name string) (string, error) {
	if !isSafeArtifactPathSegment(runID) {
		return "", fmt.Errorf("invalid runID %q", runID)
	}
	if !isSafeArtifactPathSegment(name) {
		return "", fmt.Errorf("invalid artifact name %q", name)
	}
	return fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name), nil
}

// Upload tars+zstds dir and stores it at artifacts/{runID}/{name}.tar.gz,
// streaming the archive so it is never fully buffered in memory.
func Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string) error {
	return UploadExcluding(ctx, store, runID, name, dir, nil)
}

// UploadExcluding is Upload that leaves out the paths in exclude (see
// WriteTarZstdExcluding).
func UploadExcluding(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string, exclude []string) error {
	key, err := ArtifactKey(runID, name)
	if err != nil {
		return fmt.Errorf("upload artifact: %w", err)
	}
	body := StreamTarZstdExcluding(dir, exclude)
	defer body.Close()
	return store.Put(ctx, key, body, -1)
}

// Download fetches artifacts/{runID}/{name}.tar.gz and extracts it into dest.
func Download(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string) error {
	return DownloadExcluding(ctx, store, runID, name, dest, nil)
}

// DownloadExcluding is Download that extracts nothing at or below the paths in
// exclude (see ExtractTarZstdExcluding).
func DownloadExcluding(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string, exclude []string) error {
	return DownloadWritten(ctx, store, runID, name, dest, exclude, nil)
}

// DownloadWritten is DownloadExcluding that records what it wrote in w.
func DownloadWritten(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string, exclude []string, w *Written) error {
	key, err := ArtifactKey(runID, name)
	if err != nil {
		return fmt.Errorf("download artifact: %w", err)
	}
	rc, err := store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get artifact: %w", err)
	}
	defer rc.Close()
	if dest == "" {
		dest = "."
	}
	return ExtractTarZstdWritten(rc, dest, exclude, w)
}
