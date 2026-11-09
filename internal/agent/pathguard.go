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

// EnsureNoSymlinkEscapeOS verifies that resolved (already lexically confined
// to root by ContainWithinOS) does not reach outside root through a symlink.
// The lexical check cannot see a symlink a job planted in the workspace, and
// os.OpenRoot follows a symlink that is the top-level path it is given, so a
// `path:` naming such a link would otherwise archive or write its target.
//
// Symlinks that stay inside root are fine. For a path that does not exist yet
// (a download or cache-restore destination) the deepest existing ancestor is
// resolved instead. A dangling symlink is refused because its target is not
// known to be inside root.
func EnsureNoSymlinkEscapeOS(root, resolved string) error {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no workspace on disk, so nothing in it can be a link
		}
		return fmt.Errorf("resolve workspace %q: %w", root, err)
	}
	escape := fmt.Errorf("artifact/cache path %q escapes the workspace through a symbolic link", resolved)
	cur := resolved
	for {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if real != realRoot && !strings.HasPrefix(real, realRoot+string(filepath.Separator)) {
				return escape
			}
			return nil
		}
		if _, lerr := os.Lstat(cur); lerr == nil {
			// Exists but cannot be resolved: a dangling or looping link.
			return escape
		} else if !os.IsNotExist(lerr) {
			return fmt.Errorf("inspect %q: %w", cur, lerr)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return nil
		}
		cur = parent
	}
}
