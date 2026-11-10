package agent

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ContainWithinSlash joins a RELATIVE forward-slash path p under root (a
// container path, always Linux) and guarantees the cleaned result stays
// within root. An empty p is the root itself. An absolute p, or any p that
// escapes root via "..", is rejected — this is the containment that stops a
// crafted artifact/cache path from reaching files outside the workspace
// (e.g. the artifact sidecar's mounted secrets on k8s).
func ContainWithinSlash(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	cleanRoot := path.Clean(root)
	joined := path.Clean(path.Join(cleanRoot, p))
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+"/") {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	return joined, nil
}

// ContainWithinOS is ContainWithinSlash for host (OS-native) paths.
func ContainWithinOS(root, p string) (string, error) {
	if p == "" {
		return root, nil
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	cleanRoot := filepath.Clean(root)
	joined := filepath.Clean(filepath.Join(cleanRoot, p))
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact/cache path %q escapes the workspace", p)
	}
	return joined, nil
}

// ConfineSymlinksOS checks that p (already lexically confined to root by
// ContainWithinOS) does not reach outside root through a symlink, and returns
// p rewritten with every symlink that stays inside root resolved. The lexical
// check cannot see a symlink a job planted in the workspace, and os.OpenRoot
// follows a symlink that is the top-level path it is given, so a `path:`
// naming such a link would otherwise archive or write its target.
//
// This is an up-front check for a clear error message only: the archive and
// extract layers also open p beneath root (artifact.*In), which is what stops a
// symlink swapped in after this check.
//
// For a path that does not exist yet (a download or cache-restore destination)
// the deepest existing ancestor is resolved. A dangling symlink is refused
// because its target is not known to be inside root.
//
// On Windows filepath.EvalSymlinks does not resolve NTFS junctions (mount
// points), so a junction that leaves root passes this up-front check. It is not
// what stops the read: the archive and extract layers open the path beneath
// root with os.Root, which treats a reparse point as a link and refuses to leave
// root through it. Only a native Windows job can create a junction without the
// claim-pod boundary, and native claims are not confined (see the caller).
func ConfineSymlinksOS(root, p string) (string, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return p, nil // no workspace on disk, so nothing in it can be a link
		}
		return "", fmt.Errorf("resolve workspace %q: %w", root, err)
	}
	escape := fmt.Errorf("artifact/cache path %q escapes the workspace through a symbolic link", p)
	cur := p
	var rest []string // components below the deepest existing ancestor
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			full := filepath.Join(append([]string{real}, rest...)...)
			if full != realRoot && !strings.HasPrefix(full, realRoot+string(filepath.Separator)) {
				return "", escape
			}
			rel, err := filepath.Rel(realRoot, full)
			if err != nil {
				return "", escape
			}
			return filepath.Join(root, rel), nil
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			// Exists but cannot be resolved: a dangling or looping link.
			return "", escape
		} else if !os.IsNotExist(lerr) {
			return "", fmt.Errorf("inspect %q: %w", cur, lerr)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}
