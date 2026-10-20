package dsl

import (
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultWorkspaceRoot is the container path of the workspace when
// podTemplate.workspace.mountPath is not set. It mirrors the value both
// agents use.
const defaultWorkspaceRoot = "/workspace"

// StringOrSlice is a YAML value that is either a single string or a list of
// strings:
//
//	name: go-cache
//	name: [go-cache-1, go-cache-2]
//
// Both forms decode to a slice; a scalar becomes a one-element slice.
type StringOrSlice []string

// UnmarshalYAML accepts a scalar or a sequence of scalars.
func (s *StringOrSlice) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var v string
		if err := node.Decode(&v); err != nil {
			return fmt.Errorf("line %d: must be a string or a list of strings: %w", node.Line, err)
		}
		*s = StringOrSlice{v}
		return nil
	case yaml.SequenceNode:
		var v []string
		if err := node.Decode(&v); err != nil {
			return fmt.Errorf("line %d: must be a string or a list of strings: %w", node.Line, err)
		}
		*s = StringOrSlice(v)
		return nil
	}
	return fmt.Errorf("line %d: must be a string or a list of strings", node.Line)
}

// MarshalYAML emits the scalar form for a single value and the list form
// otherwise, both of which UnmarshalYAML accepts.
func (s StringOrSlice) MarshalYAML() (any, error) {
	if len(s) == 1 {
		return s[0], nil
	}
	return []string(s), nil
}

// isTemplatedVolumeName reports whether a volume name contains a template expression.
func isTemplatedVolumeName(name string) bool {
	return strings.Contains(name, "{{")
}

// singleLiteralName returns the volume's one literal name, or "" when the
// name is a list of several candidates or contains a template. A one-element
// list is the same as a bare scalar.
func singleLiteralName(v VolumeMount) string {
	if len(v.Name) == 1 && !isTemplatedVolumeName(v.Name[0]) {
		return v.Name[0]
	}
	return ""
}

// VolumeAlias returns the handle a step uses to reach the volume: the
// explicit `as`, or the single literal name. It returns "" when neither
// exists, which ValidateVolumes rejects.
func VolumeAlias(v VolumeMount) string {
	if v.As != "" {
		return v.As
	}
	return singleLiteralName(v)
}

// workspaceVolumePath is the field path reported in errors about the
// podTemplate.workspace.volume shorthand.
const workspaceVolumePath = "spec.podTemplate.workspace.volume"

// normalizedVolume is a VolumeMount plus the YAML path to report it under.
type normalizedVolume struct {
	VolumeMount
	path string
}

func normalizeVolumes(spec Spec) ([]normalizedVolume, error) {
	var out []normalizedVolume
	if pt := spec.PodTemplate; pt != nil && pt.Workspace != nil && len(pt.Workspace.Volume) > 0 {
		for i, v := range spec.Volumes {
			if v.MountPath != "" && path.Clean(v.MountPath) == "." {
				return nil, fmt.Errorf("%s conflicts with spec.volumes[%d]: both mount at the workspace root (mountPath \".\"); keep only one", workspaceVolumePath, i)
			}
		}
		out = append(out, normalizedVolume{
			VolumeMount: VolumeMount{Name: pt.Workspace.Volume, MountPath: "."},
			path:        workspaceVolumePath,
		})
	}
	for i, v := range spec.Volumes {
		out = append(out, normalizedVolume{VolumeMount: v, path: fmt.Sprintf("spec.volumes[%d]", i)})
	}
	return out, nil
}

// NormalizeVolumes returns every volume the job mounts as one list. The
// podTemplate.workspace.volume shorthand is folded in, first, as an entry with
// MountPath ".". It is an error to use the shorthand together with an explicit
// spec.volumes entry at ".".
func NormalizeVolumes(spec Spec) ([]VolumeMount, error) {
	nv, err := normalizeVolumes(spec)
	if err != nil {
		return nil, err
	}
	if len(nv) == 0 {
		return nil, nil
	}
	out := make([]VolumeMount, len(nv))
	for i, v := range nv {
		out[i] = v.VolumeMount
	}
	return out, nil
}

// ValidateVolumes checks the syntax of a job's volume declarations. It is an
// apply-time check and deliberately limited to what the job alone can decide:
// the controller holds no volume state and `mode` lives in the agent's config,
// so whether a named volume exists, and whether a shared volume may sit at the
// workspace root, are decided later (the run stays Queued, or the agent
// rejects the mount).
func ValidateVolumes(spec Spec) error {
	vols, err := normalizeVolumes(spec)
	if err != nil {
		return err
	}
	if len(vols) == 0 {
		return nil
	}
	if spec.Detached {
		return fmt.Errorf("spec.volumes is not supported on a detached job: a detached run's workspace is deleted when the run finishes, which contradicts a persistent volume")
	}
	if spec.PodTemplate != nil && spec.PodTemplate.Reuse {
		return fmt.Errorf("spec.volumes is not supported with podTemplate.reuse: a reused pod keeps its workspace across runs, which contradicts a volume's lifecycle")
	}

	root := defaultWorkspaceRoot
	if pt := spec.PodTemplate; pt != nil && pt.Workspace != nil && pt.Workspace.MountPath != "" {
		root = pt.Workspace.MountPath
	}

	cleaned := make([]string, len(vols))
	for i, v := range vols {
		if err := validateVolumeName(v); err != nil {
			return err
		}
		c, err := cleanVolumeMountPath(v, root)
		if err != nil {
			return err
		}
		cleaned[i] = c
	}

	for i := range vols {
		for j := i + 1; j < len(vols); j++ {
			if cleaned[i] == cleaned[j] {
				return fmt.Errorf("%s: duplicate mountPath %q (also used by %s)", vols[j].path, vols[j].MountPath, vols[i].path)
			}
			// The workspace root may be combined with any other entry, so
			// "." is never an ancestor for this purpose.
			if cleaned[i] != "." && cleaned[j] != "." &&
				(isPathAncestor(cleaned[i], cleaned[j]) || isPathAncestor(cleaned[j], cleaned[i])) {
				return fmt.Errorf("%s: nested mountPath %q overlaps %s (%q): a volume cannot be mounted inside another volume", vols[j].path, vols[j].MountPath, vols[i].path, vols[i].MountPath)
			}
		}
	}
	return nil
}

// validateVolumeName checks the name and the `as` rule: a missing name, and a
// candidate list or templated name with no `as`.
func validateVolumeName(v normalizedVolume) error {
	if len(v.Name) == 0 {
		return fmt.Errorf("%s: name is required", v.path)
	}
	for _, n := range v.Name {
		if n == "" {
			return fmt.Errorf("%s: name is required (empty candidate in %v)", v.path, []string(v.Name))
		}
	}
	if v.As == "" && singleLiteralName(v.VolumeMount) == "" {
		hint := ""
		if v.path == workspaceVolumePath {
			hint = "; the workspace.volume shorthand has no `as`, so declare this volume under spec.volumes instead"
		}
		return fmt.Errorf("%s: a name that is a list of candidates or contains a template requires `as`: the chosen name is not known when the job is written%s", v.path, hint)
	}
	return nil
}

// cleanVolumeMountPath validates one mountPath and returns its cleaned form.
//
// This is deliberately not ContainWithinSlash (internal/agent/pathguard.go).
// That guard rejects every absolute path, because artifact and cache paths come
// from the job and must not reach outside the workspace. A volume mountPath
// differs: the operator declares which volumes exist and the job can only name
// one, so an absolute path is allowed (toolchains such as GOCACHE keep caches
// outside the workspace) and the escape is bounded by the agent's own
// configuration. What is still rejected is a relative path that climbs out of
// the workspace, and an absolute path that lands inside it (where a relative
// path would have said so).
func cleanVolumeMountPath(v normalizedVolume, workspaceRoot string) (string, error) {
	mp := v.MountPath
	if mp == "" {
		return "", fmt.Errorf("%s: mountPath is required", v.path)
	}
	c := path.Clean(mp)
	if path.IsAbs(c) {
		if isPathAncestor(path.Clean(workspaceRoot), c) || c == path.Clean(workspaceRoot) {
			return "", fmt.Errorf("%s: absolute mountPath %q is inside the workspace %q; use a relative mountPath instead", v.path, mp, workspaceRoot)
		}
		return c, nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%s: mountPath %q escapes the workspace", v.path, mp)
	}
	return c, nil
}

// isPathAncestor reports whether a is a strict ancestor directory of b. Both
// must already be cleaned and of the same kind (both absolute or both relative).
func isPathAncestor(a, b string) bool {
	if a == "/" {
		return b != "/"
	}
	return strings.HasPrefix(b, a+"/")
}
