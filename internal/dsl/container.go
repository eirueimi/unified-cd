package dsl

// Reserved container/volume names are injected/owned by the system, never
// user- or template-supplied. PrimaryContainerName is the default exec target
// for a step with no container:. ArtifactSidecarContainerName is the internal
// artifact/cache sidecar. WorkspaceVolumeName is the injected workspace volume;
// UcdToolsVolumeName carries the ucd-sh shim (see internal/k8sagent/podbuilder.go,
// whose constants alias these). A uses: template may not inject any of them, and
// the reserved container names are always valid container: targets even when
// absent from a podTemplate's container list.
const (
	PrimaryContainerName         = "job"
	ArtifactSidecarContainerName = "unified-artifact"
	WorkspaceVolumeName          = "workspace"
	UcdToolsVolumeName           = "ucd-tools"
)

// IsReservedContainerName reports whether name is a system-reserved container name.
func IsReservedContainerName(name string) bool {
	return name == PrimaryContainerName || name == ArtifactSidecarContainerName
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
