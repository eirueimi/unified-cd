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

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/klauspost/compress/zstd"
)

// WriteTarZstd walks dir and streams its contents to w as a tar+zstd archive.
func WriteTarZstd(w io.Writer, dir string) error {
	enc, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(enc)
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
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
		return fmt.Errorf("tar walk %q: %w", dir, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close: %w", err)
	}
	return nil
}

func artifactKey(runID, name string) string {
	return fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name)
}

// Upload tars+zstds dir and stores it at artifacts/{runID}/{name}.tar.gz.
func Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string) error {
	var buf bytes.Buffer
	if err := WriteTarZstd(&buf, dir); err != nil {
		return err
	}
	return store.Put(ctx, artifactKey(runID, name), bytes.NewReader(buf.Bytes()), int64(buf.Len()))
}

// Download fetches artifacts/{runID}/{name}.tar.gz and extracts it into dest.
func Download(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string) error {
	rc, err := store.Get(ctx, artifactKey(runID, name))
	if err != nil {
		return fmt.Errorf("get artifact: %w", err)
	}
	defer rc.Close()
	if dest == "" {
		dest = "."
	}
	return ExtractTarZstd(rc, dest)
}
