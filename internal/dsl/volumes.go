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

// VolumeEnvPrefix starts the name of every variable that carries a mounted
// volume's path to a step. It is also a reserved variable prefix
// (reservedVarPrefixes), which is what keeps a Vars manifest from shadowing one.
const VolumeEnvPrefix = "UNIFIED_VOLUME_"

// VolumeEnvName is the environment variable that holds the absolute path of the
// volume the job knows by alias: UNIFIED_VOLUME_ plus the alias upper-cased,
// with "-" as "_". It is built from the alias and never from the resolved
// volume name, because a job that lists several candidates cannot predict
// which one it gets. Both agents call this; neither builds the name inline.
func VolumeEnvName(alias string) string {
	return VolumeEnvPrefix + strings.ToUpper(strings.ReplaceAll(alias, "-", "_"))
}

// validateVolumeEnvName checks that the variable an alias produces is one a
// step can read and one nothing else owns. The reserved-name half cannot fire
// for a name with the volume prefix while that prefix is itself reserved, and
// is kept so that stays true if an exact name under the prefix is ever
// reserved for another purpose.
func validateVolumeEnvName(p string, v VolumeMount) (string, error) {
	alias := VolumeAlias(v)
	env := VolumeEnvName(alias)
	if !varKeyRe.MatchString(env) {
		return "", fmt.Errorf("%s: the alias %q (`as`, or the name when there is no `as`) cannot be part of an environment variable name (%s): use letters, digits, '-' and '_'", p, alias, env)
	}
	if reservedVarNames[env] {
		return "", fmt.Errorf("%s: the alias %q would expose the volume as %s, which is reserved", p, alias, env)
	}
	return env, nil
}

// ValidateVolumes checks the syntax of a job's volume declarations. It is an
// apply-time check and deliberately limited to what the job alone can decide:
// the controller holds no volume state and `mode` lives in the agent's config,
// so whether a named volume exists, and whether a shared volume may sit at the
// workspace root, are decided later (the run stays Queued, or the agent
// rejects the mount).
func ValidateVolumes(spec Spec) error {
	vols := spec.Volumes
	if len(vols) == 0 {
		return nil
	}
	if spec.Detached {
		return fmt.Errorf("spec.volumes is not supported on a detached job: a detached run's workspace is deleted when the run finishes, which contradicts a persistent volume")
	}
	if spec.PodTemplate != nil && spec.PodTemplate.Reuse {
		return fmt.Errorf("spec.volumes is not supported with podTemplate.reuse: a pooled pod keeps the volumes it was created with, but each run may need different ones")
	}

	root := defaultWorkspaceRoot
	if pt := spec.PodTemplate; pt != nil && pt.Workspace != nil && pt.Workspace.MountPath != "" {
		root = pt.Workspace.MountPath
	}

	cleaned := make([]string, len(vols))
	envNames := make(map[string]int, len(vols))
	for i, v := range vols {
		if err := validateVolumeName(volumePath(i), v); err != nil {
			return err
		}
		// Two aliases that upper-case to one variable would leave a step
		// reading whichever volume happened to be written last.
		env, err := validateVolumeEnvName(volumePath(i), v)
		if err != nil {
			return err
		}
		if first, dup := envNames[env]; dup {
			return fmt.Errorf("%s: alias %q and %s (%q) both become %s: aliases are upper-cased and '-' becomes '_', so they must differ by more than case or '-' versus '_'", volumePath(i), VolumeAlias(v), volumePath(first), VolumeAlias(vols[first]), env)
		}
		envNames[env] = i
		c, err := cleanVolumeMountPath(volumePath(i), v, root)
		if err != nil {
			return err
		}
		cleaned[i] = c
	}

	for i := range vols {
		for j := i + 1; j < len(vols); j++ {
			if cleaned[i] == cleaned[j] {
				return fmt.Errorf("%s: duplicate mountPath %q (also used by %s)", volumePath(j), vols[j].MountPath, volumePath(i))
			}
			// The workspace root may be combined with any other entry, so
			// "." is never an ancestor for this purpose.
			if cleaned[i] != "." && cleaned[j] != "." &&
				(isPathAncestor(cleaned[i], cleaned[j]) || isPathAncestor(cleaned[j], cleaned[i])) {
				return fmt.Errorf("%s: nested mountPath %q overlaps %s (%q): a volume cannot be mounted inside another volume", volumePath(j), vols[j].MountPath, volumePath(i), vols[i].MountPath)
			}
		}
	}
	return nil
}

// volumePath is the YAML path of the i-th spec.volumes entry, for errors.
func volumePath(i int) string {
	return fmt.Sprintf("spec.volumes[%d]", i)
}

// validateVolumeName checks the name and the `as` rule: a missing name, and a
// candidate list or templated name with no `as`.
func validateVolumeName(p string, v VolumeMount) error {
	if len(v.Name) == 0 {
		return fmt.Errorf("%s: name is required", p)
	}
	for _, n := range v.Name {
		if n == "" {
			return fmt.Errorf("%s: name is required (empty candidate in %v)", p, []string(v.Name))
		}
	}
	if v.As == "" && singleLiteralName(v) == "" {
		return fmt.Errorf("%s: a name that is a list of candidates or contains a template requires `as`: the chosen name is not known when the job is written", p)
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
//
// The "inside the workspace" check can only compare against the container
// workspace root, because that is all the job can know at apply time. A native
// host agent's workspace is a host directory chosen by the agent; that case is
// checked at mount time, not here.
func cleanVolumeMountPath(p string, v VolumeMount, workspaceRoot string) (string, error) {
	mp := v.MountPath
	if mp == "" {
		return "", fmt.Errorf("%s: mountPath is required", p)
	}
	// path.IsAbs("C:/cache") is false, so a drive-qualified path would be taken
	// for a relative one and joined onto the workspace, to surface much later as
	// an opaque mkdir error on the agent. A mountPath is a POSIX path on every
	// agent, Windows included.
	if len(mp) >= 2 && mp[1] == ':' && (mp[0] >= 'a' && mp[0] <= 'z' || mp[0] >= 'A' && mp[0] <= 'Z') {
		return "", fmt.Errorf("%s: mountPath %q starts with a Windows drive letter; mountPath must be a POSIX path (relative to the workspace, or absolute and starting with \"/\")", p, mp)
	}
	// A backslash is not a separator in a POSIX path, but the Kubernetes agent
	// turns it into one when it mounts the volume. Refusing it here keeps the
	// path the job wrote and the path that is mounted the same string, so no
	// check of the one can be dodged through the other.
	if strings.Contains(mp, `\`) {
		return "", fmt.Errorf("%s: mountPath %q contains a backslash; mountPath must be a POSIX path (relative to the workspace, or absolute and starting with \"/\")", p, mp)
	}
	c := path.Clean(mp)
	if path.IsAbs(c) {
		if isPathAncestor(path.Clean(workspaceRoot), c) || c == path.Clean(workspaceRoot) {
			return "", fmt.Errorf("%s: absolute mountPath %q is inside the workspace %q; use a relative mountPath instead", p, mp, workspaceRoot)
		}
		return c, nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%s: mountPath %q escapes the workspace", p, mp)
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
