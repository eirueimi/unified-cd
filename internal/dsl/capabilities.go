package dsl

// Agent capability vocabulary. An agent advertises the subset it can do; the
// controller infers a run's required capability from its spec (RequiredCaps)
// and only an agent whose capabilities are a superset may claim the run.
const (
	CapNative    = "native"    // run a step as a host process (standard agent)
	CapContainer = "container" // run a step in an isolated container (docker/podman/k8s)
	CapPod       = "pod"       // build a Kubernetes Pod (k8s agent only)
)

// LegacyAgentCapabilities is what an agent is taken to be able to run when it
// has never advertised a capability set: {native, container}. An agents row
// with NULL capabilities (or no row at all) can only belong to an agent that
// predates capability advertisement, which was a host agent; the Kubernetes
// agent has always advertised pod. The claim query substitutes this set for the
// NULL so such an agent keeps claiming what it always could, and can never be
// handed a pod run, which a host agent would silently degrade. It returns a
// fresh slice each call.
func LegacyAgentCapabilities() []string {
	return []string{CapNative, CapContainer}
}

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

// RequiredVolumes returns the candidate volume names each of a job's volumes
// needs, one set per entry in declaration order. A run needs one free volume
// from every set. It is a second dimension beside RequiredCaps, not a
// capability string. The spec must already be expanded (ExpandVolumes); it
// returns nil when the job declares no volumes. The result shares no memory
// with spec.
func RequiredVolumes(spec Spec) [][]string {
	if len(spec.Volumes) == 0 {
		return nil
	}
	out := make([][]string, len(spec.Volumes))
	for i, v := range spec.Volumes {
		out[i] = append([]string(nil), v.Name...)
	}
	return out
}
