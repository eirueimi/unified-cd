package dsl

import (
	"fmt"
	"regexp"
	"strings"
)

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

// dns1123LabelRe matches a valid Kubernetes container/volume name (DNS-1123
// label): lowercase alphanumerics and '-', starting and ending alphanumeric.
var dns1123LabelRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateDNS1123Label rejects a name that is not a valid DNS-1123 label
// (the shape Kubernetes requires for container and volume names). Catching
// this at parse time turns an opaque pod-build API error into a clear
// authoring error — and closes case/whitespace evasion of the reserved-name
// checks.
func ValidateDNS1123Label(name string) error {
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if len(name) > 63 {
		return fmt.Errorf("name %q exceeds 63 characters", name)
	}
	if !dns1123LabelRe.MatchString(name) {
		return fmt.Errorf("name %q is not a valid DNS-1123 label (lowercase alphanumerics and '-', must start/end alphanumeric)", name)
	}
	return nil
}

// IsReservedContainerName reports whether name is a system-reserved container
// name. Comparison is normalized (trimmed, lowercased) so case/whitespace
// variants cannot evade the reservation; shape validation rejects such
// variants outright, this is defense in depth.
func IsReservedContainerName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == PrimaryContainerName || n == ArtifactSidecarContainerName || n == UcdShimContainerName
}

// IsReservedVolumeName reports whether name is a system-reserved volume name
// (normalized like IsReservedContainerName).
func IsReservedVolumeName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == WorkspaceVolumeName || n == UcdToolsVolumeName
}

// validatePodTemplateNames checks every container and volume name declared in
// pt — including pt.Override's real, pod-build-merged containers/volumes
// (see internal/k8sagent/podbuilder.go mergeContainers/mergeVolumes) — for
// DNS-1123-label shape. Nil-safe.
func validatePodTemplateNames(pt *PodTemplate) error {
	for _, c := range PodTemplateContainers(pt) {
		if err := ValidateDNS1123Label(DefName(c)); err != nil {
			return fmt.Errorf("podTemplate container %w", err)
		}
	}
	for _, v := range PodTemplateVolumes(pt) {
		if err := ValidateDNS1123Label(DefName(v)); err != nil {
			return fmt.Errorf("podTemplate volume %w", err)
		}
	}
	if pt != nil && pt.Override != nil {
		for _, c := range pt.Override.Containers {
			if err := ValidateDNS1123Label(DefName(c)); err != nil {
				return fmt.Errorf("podTemplate override container %w", err)
			}
		}
		for _, v := range pt.Override.Volumes {
			if err := ValidateDNS1123Label(DefName(v)); err != nil {
				return fmt.Errorf("podTemplate override volume %w", err)
			}
		}
	}
	return nil
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

// validateStepTargetedWorkingDir rejects a workingDir on any container a step
// executes in (the primary "job" container or any container named by a step's
// container: field, in steps, parallel sub-steps, or finally). Steps always
// run at the workspace mount: the k8s executor inherits the container's
// workingDir, while artifact/cache resolution and UNIFIED_WORKSPACE use the
// workspace mount path — a divergent workingDir silently desynchronizes them,
// and the artifact sidecar can only reach files under the workspace volume.
// Sidecars (containers no step targets) may set workingDir freely.
func validateStepTargetedWorkingDir(spec Spec) error {
	targeted := map[string]bool{PrimaryContainerName: true}
	collect := func(entries []StepEntry) {
		for _, e := range entries {
			if e.Container != "" {
				targeted[e.Container] = true
			}
			for _, p := range e.Parallel {
				if p.Container != "" {
					targeted[p.Container] = true
				}
			}
		}
	}
	collect(spec.Steps)
	collect(spec.Finally)

	check := func(defs []map[string]any, where string) error {
		for _, c := range defs {
			name := DefName(c)
			if !targeted[name] {
				continue
			}
			if _, has := c["workingDir"]; has {
				return fmt.Errorf("%s container %q declares workingDir, but steps execute in it: steps always run at the workspace mount (artifact/cache paths and UNIFIED_WORKSPACE resolve there); move the cd into the step script, or put workingDir on a sidecar", where, name)
			}
		}
		return nil
	}
	if err := check(PodTemplateContainers(spec.PodTemplate), "podTemplate"); err != nil {
		return err
	}
	if spec.PodTemplate != nil && spec.PodTemplate.Override != nil {
		if err := check(spec.PodTemplate.Override.Containers, "podTemplate.override"); err != nil {
			return err
		}
	}
	return nil
}

// ValidateContainerReferences checks that every step's container: reference in
// spec resolves to a real target: empty (defaults to the primary container), a
// reserved name, or a container defined in spec.PodTemplate — including
// spec.PodTemplate.Override.Containers, which defines REAL containers merged
// into the pod at build time (see internal/k8sagent/podbuilder.go
// mergeContainers), not just spec.PodTemplate's base container list.
// Scope-tagged steps (ScopeID set) run in their own scope pod, not the
// caller's pod, so their container is not checked here. Returns a descriptive
// error on the first invalid reference. Intended to run on a fully-resolved
// spec (after uses merge).
func ValidateContainerReferences(spec Spec) error {
	defined := map[string]bool{}
	for _, c := range PodTemplateContainers(spec.PodTemplate) {
		if n := DefName(c); n != "" {
			defined[n] = true
		}
	}
	if spec.PodTemplate != nil && spec.PodTemplate.Override != nil {
		for _, c := range spec.PodTemplate.Override.Containers {
			if n := DefName(c); n != "" {
				defined[n] = true
			}
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
