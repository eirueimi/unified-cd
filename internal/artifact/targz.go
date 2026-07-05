// Package artifact provides the shared tar+zstd artifact wire-format helpers
// used by both the agent and the CLI.
package artifact

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ExtractTarZstd extracts a tar+zstd stream into dest.
// Includes path checks to prevent path-traversal attacks.
func ExtractTarZstd(r io.Reader, dest string) error {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()

	// Resolve dest to an absolute path so the traversal guard works regardless of
	// how dest was given (e.g. "." or a relative dir) — filepath.Join with a
	// relative dest would normalise away a "./" prefix and make the HasPrefix
	// check always fail for archives whose entries are relative (tar -C dir .).
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("resolve dest %q: %w", dest, err)
	}
	cleanDest := absDest + string(filepath.Separator)
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		target := filepath.Join(absDest, filepath.FromSlash(hdr.Name))
		if !strings.HasPrefix(target+string(filepath.Separator), cleanDest) {
			return fmt.Errorf("invalid path %q in artifact archive", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Refuse rather than silently mis-extracting: falling through to
			// the default case would os.Create an empty regular file and
			// discard Linkname, corrupting the archive contents. Creating a
			// real symlink/hardlink instead would reintroduce a traversal
			// vector (a symlink can point outside dest). So fail loudly.
			return fmt.Errorf("unsupported %s entry %q in artifact archive", typeflagName(hdr.Typeflag), hdr.Name)
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}

// typeflagName renders a tar.Typeflag for error messages.
func typeflagName(t byte) string {
	switch t {
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hardlink"
	default:
		return fmt.Sprintf("typeflag %q", string(t))
	}
}
