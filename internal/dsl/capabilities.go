package dsl

// Agent capability vocabulary. An agent advertises the subset it can do; the
// controller infers a run's required capability from its spec (RequiredCaps)
// and only an agent whose capabilities are a superset may claim the run.
const (
	CapNative    = "native"    // run a step as a host process (standard agent)
	CapContainer = "container" // run a step in an isolated container (docker/podman/k8s)
	CapPod       = "pod"       // build a Kubernetes Pod (k8s agent only)
)

// ValidCapability reports whether s is a known capability string.
func ValidCapability(s string) bool {
	return s == CapNative || s == CapContainer || s == CapPod
}

// RequiredCaps infers the single capability a run of spec needs from an agent:
//   - native: true                 -> native (host process)
//   - no podTemplate (isolated)    -> container
//   - podTemplate host can't honor -> pod   (PodTemplateNeedsKubernetes, from 8ca1567)
//   - podTemplate host CAN honor   -> container
//
// native takes precedence: a native job never runs in a container/pod.
func RequiredCaps(spec Spec) []string {
	switch {
	case spec.Native:
		return []string{CapNative}
	case spec.PodTemplate != nil && PodTemplateNeedsKubernetes(spec.PodTemplate):
		return []string{CapPod}
	default:
		return []string{CapContainer}
	}
}
