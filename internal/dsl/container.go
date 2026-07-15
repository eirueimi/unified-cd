package dsl

import "fmt"

// Reserved container/volume names are injected/owned by the system, never
// user- or template-supplied. PrimaryContainerName is the default exec target
// for a step with no container:. ArtifactSidecarContainerName is the internal
// artifact/cache sidecar. UcdShimContainerName is the k8s-agent init container
// that installs the ucd-sh shim (see internal/k8sagent/podbuilder.go's
// ucdShimContainerName, which aliases this). WorkspaceVolumeName is the
// injected workspace volume; UcdToolsVolumeName carries the ucd-sh shim (see
// internal/k8sagent/podbuilder.go, whose constants alias these). A uses:
// template may not inject any of them, and the reserved container names are
// always valid container: targets even when absent from a podTemplate's
// container list (note: UcdShimContainerName is an init container that exits
// before the job runs, so an exec into it would fail at runtime — it is
// listed as reserved, not as a sane exec target).
const (
	PrimaryContainerName         = "job"
	ArtifactSidecarContainerName = "unified-artifact"
	UcdShimContainerName         = "ucd-shim"
	WorkspaceVolumeName          = "workspace"
	UcdToolsVolumeName           = "ucd-tools"
)

// IsReservedContainerName reports whether name is a system-reserved container name.
func IsReservedContainerName(name string) bool {
	return name == PrimaryContainerName || name == ArtifactSidecarContainerName || name == UcdShimContainerName
}

// IsReservedVolumeName reports whether name is a system-reserved volume name.
func IsReservedVolumeName(name string) bool {
	return name == WorkspaceVolumeName || name == UcdToolsVolumeName
}

// PodTemplateContainers returns pt's container definition maps (from
// pt.Spec["containers"]) in declared order. Nil-safe; skips non-map entries.
func PodTemplateContainers(pt *PodTemplate) []map[string]any {
	return podTemplateDefs(pt, "containers")
}

// PodTemplateVolumes returns pt's volume definition maps (from
// pt.Spec["volumes"]) in declared order. Nil-safe; skips non-map entries.
func PodTemplateVolumes(pt *PodTemplate) []map[string]any {
	return podTemplateDefs(pt, "volumes")
}

func podTemplateDefs(pt *PodTemplate, key string) []map[string]any {
	if pt == nil {
		return nil
	}
	raw, _ := pt.Spec[key].([]any)
	var out []map[string]any
	for _, r := range raw {
		if d, ok := r.(map[string]any); ok {
			out = append(out, d)
		}
	}
	return out
}

// DefName returns the "name" field of a container/volume definition map, or "".
func DefName(def map[string]any) string {
	n, _ := def["name"].(string)
	return n
}

// ValidateContainerReferences checks that every step's container: reference in
// spec resolves to a real target: empty (defaults to the primary container), a
// reserved name, or a container defined in spec.PodTemplate. Scope-tagged steps
// (ScopeID set) run in their own scope pod, not the caller's pod, so their
// container is not checked here. Returns a descriptive error on the first
// invalid reference. Intended to run on a fully-resolved spec (after uses merge).
func ValidateContainerReferences(spec Spec) error {
	defined := map[string]bool{}
	for _, c := range PodTemplateContainers(spec.PodTemplate) {
		if n := DefName(c); n != "" {
			defined[n] = true
		}
	}
	check := func(stepName, container, scopeID string) error {
		if container == "" || scopeID != "" {
			return nil
		}
		if IsReservedContainerName(container) || defined[container] {
			return nil
		}
		return fmt.Errorf("step %q references container %q, which is not defined in the job's podTemplate", stepName, container)
	}
	walk := func(entries []StepEntry) error {
		for _, e := range entries {
			if err := check(e.Name, e.Container, e.ScopeID); err != nil {
				return err
			}
			for _, p := range e.Parallel {
				if err := check(p.Name, p.Container, p.ScopeID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(spec.Steps); err != nil {
		return err
	}
	return walk(spec.Finally)
}
