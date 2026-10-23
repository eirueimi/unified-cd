package artifact

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Written records what an extraction put on disk, so a caller can re-own
// exactly that and nothing it did not write. A nil *Written is valid and
// records nothing.
//
// It keeps every file the extraction created and the top-most directory of each
// chain it newly created. A file that already existed is overwritten in place
// and keeps its owner, as it does under the Kubernetes artifact sidecar's
// os.Create, so it is not recorded; nor is a directory that existed before:
// re-owning it (the chown is recursive) would also re-own files the job put
// there.
type Written struct {
	files   []string
	newDirs []string
}

// File records a file (or link) the extraction created.
func (w *Written) File(p string) {
	if w != nil {
		w.files = append(w.files, p)
	}
}

// lstat is os.Lstat; a variable so a test can inject an error that is not
// "does not exist".
var lstat = os.Lstat

// MkdirAll is os.MkdirAll that records the top-most directory it had to
// create, if any.
func (w *Written) MkdirAll(dir string, perm os.FileMode) error {
	var top string
	if w != nil {
		for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
			_, err := lstat(d)
			if err == nil {
				break
			}
			if !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			top = d
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	if top != "" {
		w.newDirs = append(w.newDirs, top)
	}
	return nil
}

// Roots returns the paths to re-own: every written file and every newly
// created directory, without any path lying below a newly created directory
// (that one's recursion covers it). Sorted and de-duplicated.
func (w *Written) Roots() []string {
	if w == nil {
		return nil
	}
	dirs := map[string]bool{}
	for _, d := range w.newDirs {
		dirs[d] = true
	}
	covered := func(p string) bool {
		for d := filepath.Dir(p); ; d = filepath.Dir(d) {
			if dirs[d] {
				return true
			}
			if filepath.Dir(d) == d {
				return false
			}
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string{}, w.newDirs...), w.files...) {
		if seen[p] || covered(p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
