package dsl

// Sidecar log lines reuse the existing logs pipeline via dedicated sentinel
// step_index values that never collide with real steps ([0,N)) or the System
// stream (-1). The agent ships a sidecar's output under its index; the
// controller synthesizes a matching pseudo-step so the UI renders the name.
// Both sides compute the index from the same podTemplate container order, so no
// mapping is exchanged. See docs/superpowers/specs/2026-07-13-sidecar-logs-design.md.
const (
	// SidecarLogIndexBase is the step_index of the first user podTemplate
	// sidecar (declared order, excluding the primary "job"); the k-th sidecar
	// uses SidecarLogIndexBase + k. 100000 is far above any real step count.
	SidecarLogIndexBase = 100000
	// ArtifactLogIndex is the step_index for the injected artifact/cache
	// sidecar's exec output (moved off the shared step 0). 90000 sits below the
	// user-sidecar base so 90000..99999 is reserved for internal sources.
	ArtifactLogIndex = 90000
)

// SidecarLogIndex returns the log step_index for the user sidecar at the given
// 0-based ordinal among non-"job" podTemplate containers (declared order).
func SidecarLogIndex(ordinal int) int { return SidecarLogIndexBase + ordinal }

// SidecarContainerNames returns the names of pt's user sidecar containers —
// every container in pt.Spec["containers"] except the primary "job" — in
// declared order. The k-th name here maps to SidecarLogIndex(k). Nil-safe:
// returns nil for a nil template, a template with no containers, or one whose
// only container is "job".
func SidecarContainerNames(pt *PodTemplate) []string {
	if pt == nil {
		return nil
	}
	containers, _ := pt.Spec["containers"].([]any)
	var out []string
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := c["name"].(string)
		if name == "" || name == "job" {
			continue
		}
		out = append(out, name)
	}
	return out
}
