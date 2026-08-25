package dsl

// HostSupportedContainerFields lists the podTemplate container keys the host
// (standard) agent's claim pod honors. command/args are honored: the host
// claim-pod builder (parseContainerDef in internal/agent/claim_pod.go) carries
// them into containerDef.Entrypoint/Args (ENTRYPOINT/CMD override,
// respectively), which become the container's argv. Every other key on a
// container (volumeMounts, ports, securityContext, envFrom, ...) is silently
// dropped by the host backend, so its presence means the job can only run
// correctly on a Kubernetes agent. This is the single source of truth for
// that set: the host claim-pod builder (internal/agent/claim_pod.go) and the
// controller's routing predicate (PodTemplateNeedsKubernetes) both read it.
var HostSupportedContainerFields = map[string]bool{
	"name": true, "image": true, "env": true, "resources": true,
	"command": true, "args": true,
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
		if containerNeedsKubernetes(c) {
			return true
		}
	}
	return false
}

// containerNeedsKubernetes reports whether a container whose every field NAME is
// host-supported still uses a sub-key the host cannot honor.
//
// HostSupportedContainerFields compares field names. Two of those fields have
// sub-keys that split across the backend boundary:
//
//   - resources: the host maps resources.limits but drops resources.requests,
//     because docker/podman have no request concept.
//   - env: the host honors a literal value but cannot resolve valueFrom, which
//     needs a Kubernetes API to dereference.
//
// Without this check a container carrying only the unsupported half is
// classified host-runnable, routes to a standard agent, and is silently
// degraded there — see the two ignore warnings in
// internal/agent/claim_pod.go, whose remedy ("route to a Kubernetes agent")
// is advice at a point where routing has already been decided.
//
// Any future addition to HostSupportedContainerFields must state whether the
// host honors the whole field or only part of it.
func containerNeedsKubernetes(c map[string]any) bool {
	if res, ok := c["resources"].(map[string]any); ok {
		// An empty requests map requests nothing, so it is not a reason to
		// force kubernetes.
		if reqs, ok := res["requests"].(map[string]any); ok && len(reqs) > 0 {
			return true
		}
		// resources itself splits the same way its own sub-keys do: the host
		// builder (claim_pod.go parseContainerDef) only ever looks at
		// res["limits"] and res["requests"]. Any other top-level resources
		// key — "claims" (DRA) chief among them — has no host mapping at
		// all, so its presence alone means the container needs Kubernetes.
		for key := range res {
			if key != "limits" && key != "requests" {
				return true
			}
		}
		// resources.limits is host-honored only for a strict subset of one
		// sub-key: the host builder reads exactly res["limits"]["cpu"] and
		// res["limits"]["memory"], asserts each to a string, and silently
		// drops everything else in the map — any extended resource
		// (nvidia.com/gpu, ephemeral-storage, hugepages-*, ...), and a
		// cpu/memory value that isn't a string (bare `cpu: 1` is valid k8s
		// Quantity syntax the host's `.(string)` assertion just drops). So
		// "limits is supported" is true only for cpu/memory spelled as YAML
		// strings — anything else in limits means the container needs the
		// part of the spec the host cannot see.
		if lim, ok := res["limits"].(map[string]any); ok {
			for key, val := range lim {
				switch key {
				case "cpu", "memory":
					if _, ok := val.(string); !ok {
						return true
					}
				default:
					return true
				}
			}
		}
	}
	env, _ := c["env"].([]any)
	for _, raw := range env {
		e, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Mirror the host builder's order exactly (claim_pod.go): an entry with
		// no name is skipped before its value is ever considered, so it is not
		// a valueFrom the host drops — it is an entry neither backend uses.
		if name, _ := e["name"].(string); name == "" {
			continue
		}
		// A missing "value" KEY is unresolvable on the host; value: "" is a
		// literal empty string it honors. claim_pod.go makes the same
		// distinction with `rawVal, present := e["value"]`.
		if _, present := e["value"]; !present {
			return true
		}
	}
	return false
}
