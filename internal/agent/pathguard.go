package agent

import (
	"fmt"
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
	joined := path.Clean(path.Join(root, p))
	if joined != root && !strings.HasPrefix(joined, root+"/") {
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
