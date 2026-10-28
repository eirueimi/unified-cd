package dsl

import (
	"fmt"
	"path"
	"strings"
)

// ProtectedMountDirs are container directories a job must not choose as (or
// above, or below) a mount path. On the Kubernetes agent a job-chosen
// workspace mountPath, and an absolute spec.volumes mountPath, is mounted into
// EVERY container of the Pod, the artifact sidecar included. The sidecar holds
// the object-store credentials, so a job-writable directory that lands on its
// binaries, libraries, configuration, credential mounts or the /.ucd shim
// volume would let job content run inside, or read from, the sidecar. These
// are the standard system and runtime directories of a Linux image plus the
// agent's reserved /.ucd mount.
var ProtectedMountDirs = []string{
	"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
	"/etc", "/proc", "/sys", "/dev", "/run", "/var/run", "/.ucd",
	"/usr/local/bin",
}

// CheckContainerMountPath returns an error when p is not an acceptable job
// chosen mount path: it must be absolute and clean, must not be "/", and must
// not equal, sit under, or be an ancestor of any entry of ProtectedMountDirs or
// of extraProtected (directories only the caller knows, such as the k8s
// agent's sidecar credential mounts). It does not look at relative paths: a
// relative spec.volumes mountPath resolves under the workspace, so the
// workspace check covers it.
func CheckContainerMountPath(p string, extraProtected ...string) error {
	if !path.IsAbs(p) {
		return fmt.Errorf("mountPath %q must be an absolute path", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("mountPath %q must be a clean path (no trailing slash, \".\", \"..\" or repeated slashes); use %q", p, path.Clean(p))
	}
	if p == "/" {
		return fmt.Errorf("mountPath %q is the container root", p)
	}
	for _, list := range [][]string{ProtectedMountDirs, extraProtected} {
		for _, d := range list {
			switch {
			case p == d:
				return fmt.Errorf("mountPath %q is a protected container directory", p)
			case strings.HasPrefix(p, d+"/"):
				return fmt.Errorf("mountPath %q is inside the protected container directory %q", p, d)
			case strings.HasPrefix(d, p+"/"):
				return fmt.Errorf("mountPath %q would cover the protected container directory %q", p, d)
			}
		}
	}
	return nil
}

// validateWorkspaceMountPath is the apply-time check of
// podTemplate.workspace.mountPath (see ProtectedMountDirs for why).
func validateWorkspaceMountPath(spec Spec) error {
	pt := spec.PodTemplate
	if pt == nil || pt.Workspace == nil || pt.Workspace.MountPath == "" {
		return nil
	}
	if err := CheckContainerMountPath(pt.Workspace.MountPath); err != nil {
		return fmt.Errorf("spec.podTemplate.workspace: %w", err)
	}
	return nil
}
