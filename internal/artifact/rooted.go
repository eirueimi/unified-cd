package artifact

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// The functions in this file are the confined counterparts of the plain
// archive helpers, for a process that is handed paths by an untrusted job and
// holds credentials the job must not reach: the k8s artifact sidecar. A lexical
// "stays under the workspace" check is not enough there, because the job
// controls symlinks inside the workspace and the sidecar resolves them in ITS
// filesystem: a link to /etc/unified-cd (the sidecar's credential mount) would
// let the job archive the credentials, and a link to the sidecar's trust store
// would let a download overwrite it.
//
// Every operation here goes through an os.Root opened on the directory the
// caller names. os.Root resolves each path component beneath that directory:
// ".." and symlinks (at any component, the final one included) that lead
// outside it fail instead of being followed, while symlinks that stay inside it
// keep working.

// RelInRoot returns p (absolute, or relative to root) as a path relative to
// root, and refuses one that is not at or below root. This is a lexical check
// only; the os.Root the callers open is what stops symlinks.
func RelInRoot(root, p string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(absRoot, p)
	}
	rel, err := filepath.Rel(absRoot, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the root %q", p, root)
	}
	return rel, nil
}

// openRoot opens root as an os.Root and returns it with root's absolute form.
func openRoot(root string) (*os.Root, string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, "", fmt.Errorf("resolve root %q: %w", root, err)
	}
	r, err := os.OpenRoot(absRoot)
	if err != nil {
		return nil, "", fmt.Errorf("open root %q: %w", root, err)
	}
	return r, absRoot, nil
}

// WriteTarZstdIn is WriteTarZstdExcluding confined to root: path (a directory
// or a single file, absolute or relative to root) must lie at or below root,
// and nothing outside root is read, whatever symlinks the tree holds. A
// symlink inside the tree is archived as a symlink entry (header only) and is
// never followed.
func WriteTarZstdIn(w io.Writer, root, path string, exclude []string) error {
	rt, absRoot, err := openRoot(root)
	if err != nil {
		return err
	}
	defer rt.Close()
	rel, err := RelInRoot(absRoot, path)
	if err != nil {
		return err
	}
	skip := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		skip[filepath.Clean(e)] = true
	}
	// Entries are named relative to the archived directory, or for a single
	// file relative to its parent so the entry is the base name.
	relBase := rel
	if info, err := rt.Stat(rel); err == nil && !info.IsDir() {
		relBase = filepath.Dir(rel)
	}

	enc, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(enc)
	var walk func(p string) error
	walk = func(p string) error {
		if skip[filepath.Join(absRoot, p)] {
			return nil
		}
		info, err := rt.Lstat(p)
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = rt.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		name, err := filepath.Rel(relBase, p)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(name)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		switch {
		case info.IsDir():
			d, err := rt.Open(p)
			if err != nil {
				return err
			}
			entries, err := d.ReadDir(-1)
			d.Close()
			if err != nil {
				return err
			}
			slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			for _, e := range entries {
				if err := walk(filepath.Join(p, e.Name())); err != nil {
					return err
				}
			}
		case info.Mode().IsRegular():
			f, err := rt.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	}
	if err := walk(rel); err != nil {
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

// StreamTarZstdIn is StreamTarZstdExcluding confined to root (see
// WriteTarZstdIn). The same rule applies: callers MUST Close the reader.
func StreamTarZstdIn(root, path string, exclude []string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(WriteTarZstdIn(pw, root, path, exclude))
	}()
	return pr
}

// UploadIn is UploadExcluding confined to root.
func UploadIn(ctx context.Context, store objectstore.ObjectStore, runID, name, root, dir string, exclude []string) error {
	key, err := ArtifactKey(runID, name)
	if err != nil {
		return fmt.Errorf("upload artifact: %w", err)
	}
	body := StreamTarZstdIn(root, dir, exclude)
	defer body.Close()
	return store.Put(ctx, key, body, -1)
}

// DownloadIn is DownloadExcluding confined to root.
func DownloadIn(ctx context.Context, store objectstore.ObjectStore, runID, name, root, dest string, exclude []string) error {
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
	return ExtractTarZstdIn(rc, root, dest, exclude, true)
}

// ExtractTarZstdIn is ExtractTarZstdExcluding confined to root: dest must lie
// at or below root, and no file or directory is created through a symlink that
// leads outside root (a symlink that stays inside root is followed). A symlink
// or hardlink entry in the archive is an error when refuseLinks is set and is
// skipped otherwise.
func ExtractTarZstdIn(r io.Reader, root, dest string, exclude []string, refuseLinks bool) error {
	_, err := ExtractTarZstdInCounting(r, root, dest, exclude, refuseLinks)
	return err
}

// ExtractTarZstdInCounting is ExtractTarZstdIn that also reports how many
// symlink and hardlink entries it skipped (always 0 when refuseLinks is set,
// since the first link is then an error), so a caller can tell the user the
// extracted tree is incomplete.
func ExtractTarZstdInCounting(r io.Reader, root, dest string, exclude []string, refuseLinks bool) (skippedLinks int, err error) {
	rt, absRoot, err := openRoot(root)
	if err != nil {
		return 0, err
	}
	defer rt.Close()
	destRel, err := RelInRoot(absRoot, dest)
	if err != nil {
		return skippedLinks, err
	}
	dec, err := zstd.NewReader(r)
	if err != nil {
		return skippedLinks, fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()
	var skip []string
	for _, e := range exclude {
		skip = append(skip, filepath.Clean(e))
	}
	tr := tar.NewReader(dec)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return skippedLinks, fmt.Errorf("tar next: %w", err)
		}
		rel := filepath.Join(destRel, filepath.FromSlash(hdr.Name))
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return skippedLinks, fmt.Errorf("invalid path %q in artifact archive", hdr.Name)
		}
		if isUnderAny(filepath.Join(absRoot, rel), skip) {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := rt.MkdirAll(rel, 0o750); err != nil {
				return skippedLinks, err
			}
		case tar.TypeSymlink, tar.TypeLink:
			if refuseLinks {
				return skippedLinks, fmt.Errorf("unsupported %s entry %q in artifact archive", typeflagName(hdr.Typeflag), hdr.Name)
			}
			skippedLinks++
		default:
			if err := rt.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
				return skippedLinks, err
			}
			f, err := rt.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
			if err != nil {
				return skippedLinks, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return skippedLinks, err
			}
			f.Close()
		}
	}
	return skippedLinks, nil
}
