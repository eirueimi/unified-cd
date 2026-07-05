package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/klauspost/compress/zstd"
)

// WriteTarZstd archives path (a directory OR a single file) and streams it to w
// as a tar+zstd archive. For a directory, entries are named relative to the
// directory; for a single file, the archive contains one entry named the file's
// base name (so `path: out.txt` round-trips to `out.txt`, matching the docs).
func WriteTarZstd(w io.Writer, path string) error {
	// When path is a single file, name entries relative to its parent so the
	// entry is the base name rather than "." (which would extract onto the dest
	// directory itself).
	relBase := path
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		relBase = filepath.Dir(path)
	}

	enc, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(enc)
	if err := filepath.WalkDir(path, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(relBase, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("tar walk %q: %w", path, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close: %w", err)
	}
	return nil
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

// artifactKey builds the object-store key for an artifact. It is defensive
// regardless of upstream validation (internal/dsl already rejects unsafe
// uploadArtifact/downloadArtifact names at parse time, but this is the last
// line of defense against path traversal — see #26): runID and name are
// required to be plain, single path segments with no "/", "\\", or "..". A
// name/runID that satisfies that constraint produces the exact same key as
// before this fix, so already-stored artifacts with plain names are
// unaffected.
func artifactKey(runID, name string) (string, error) {
	if !isSafeArtifactPathSegment(runID) {
		return "", fmt.Errorf("invalid runID %q", runID)
	}
	if !isSafeArtifactPathSegment(name) {
		return "", fmt.Errorf("invalid artifact name %q", name)
	}
	return fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name), nil
}

// Upload tars+zstds dir and stores it at artifacts/{runID}/{name}.tar.gz.
func Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string) error {
	key, err := artifactKey(runID, name)
	if err != nil {
		return fmt.Errorf("upload artifact: %w", err)
	}
	var buf bytes.Buffer
	if err := WriteTarZstd(&buf, dir); err != nil {
		return err
	}
	return store.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()))
}

// Download fetches artifacts/{runID}/{name}.tar.gz and extracts it into dest.
func Download(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string) error {
	key, err := artifactKey(runID, name)
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
	return ExtractTarZstd(rc, dest)
}
