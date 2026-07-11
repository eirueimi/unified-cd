package dsl

// HostSupportedContainerFields lists the podTemplate container keys the host
// (standard) agent's claim pod honors. Any other key on a container
// (command, args, volumeMounts, ports, securityContext, envFrom, ...) is
// silently dropped by the host backend, so its presence means the job can only
// run correctly on a Kubernetes agent. This is the single source of truth for
// that set: the host claim-pod builder (internal/agent/claim_pod.go) and the
// controller's routing predicate (PodTemplateNeedsKubernetes) both read it.
var HostSupportedContainerFields = map[string]bool{
	"name": true, "image": true, "env": true, "resources": true,
}

// PodTemplateNeedsKubernetes reports whether pt uses any feature the host
// agent's claim pod cannot honor, so a run carrying it must be pinned to a
// Kubernetes agent (the controller auto-appends the "kubernetes" label).
//
// The host claim pod degrades workspace.pvc to a per-claim bind mount by
// design, so a PVC (and mountPath, reuse) does NOT force kubernetes. Everything
// the host silently drops does: a named agent-side template, an override patch,
// any pod-level spec key beyond "containers", and any container field outside
// HostSupportedContainerFields.
func PodTemplateNeedsKubernetes(pt *PodTemplate) bool {
	if pt == nil {
		return false
	}
	// A named agent-side template resolves only in the k8s-agent's config.
	if pt.Name != "" {
		return true
	}
	// The host builder reads only pt.Spec["containers"]; an override patch
	// (extra containers/volumes) would be dropped.
	if pt.Override != nil {
		return true
	}
	for key := range pt.Spec {
		if key != "containers" {
			// volumes, nodeSelector, affinity, initContainers, tolerations,
			// securityContext, serviceAccountName, ... — all host-unsupported.
			return true
		}
	}
	containers, _ := pt.Spec["containers"].([]any)
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for field := range c {
			if !HostSupportedContainerFields[field] {
				return true
			}
		}
	}
	return false
}
