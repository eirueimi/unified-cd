package agent

import (
	"fmt"
	"log/slog"
	"strconv"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"k8s.io/apimachinery/pkg/api/resource"
)

// containerDef is the host-supported subset of a podTemplate container: the
// fields the host agent can honor when backing a runsIn.container step. Every
// other k8s container field is ignored with a WARN in namedContainerDef.
type containerDef struct {
	Name     string
	Image    string
	Env      []string // KEY=VALUE
	CPULimit string   // cores, e.g. "0.5" (CreateSpec.CPULimit); empty = no limit
	MemLimit string   // bytes, e.g. "268435456" (CreateSpec.MemLimit); empty = no limit
}

// containerSupportedFields lists the podTemplate container keys the host honors.
// Anything else present on a container triggers a WARN (see namedContainerDef).
var containerSupportedFields = map[string]bool{
	"name": true, "image": true, "env": true, "resources": true,
}

// namedContainerDef extracts the definition of the container named `name` from
// the job's podTemplate.spec.containers, keeping only host-supported fields.
// A nil podTemplate or an absent name is an error (the runsIn.container step
// cannot run). Host-unsupported fields (command, args, volumeMounts, ports,
// securityContext, envFrom, ...) are logged once per container and dropped.
func namedContainerDef(pt *dsl.PodTemplate, name string) (containerDef, error) {
	if pt == nil {
		return containerDef{}, fmt.Errorf("runsIn.container %q requires a podTemplate that defines it", name)
	}
	containers, _ := pt.Spec["containers"].([]any)
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cname, _ := c["name"].(string)
		if cname != name {
			continue
		}
		return parseContainerDef(name, c), nil
	}
	return containerDef{}, fmt.Errorf("container %q is not defined in the job's podTemplate", name)
}

func parseContainerDef(name string, c map[string]any) containerDef {
	def := containerDef{Name: name}
	def.Image, _ = c["image"].(string)

	for k := range c {
		if !containerSupportedFields[k] {
			slog.Warn("podTemplate container field is not supported on the host agent and is ignored",
				"container", name, "field", k)
		}
	}

	if envs, ok := c["env"].([]any); ok {
		for _, raw := range envs {
			e, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			en, _ := e["name"].(string)
			ev, hasVal := e["value"].(string)
			if en == "" {
				continue
			}
			if !hasVal {
				// valueFrom / fieldRef etc. — not resolvable on the host.
				slog.Warn("podTemplate container env without a literal value is ignored on the host agent",
					"container", name, "env", en)
				continue
			}
			def.Env = append(def.Env, en+"="+ev)
		}
	}

	if res, ok := c["resources"].(map[string]any); ok {
		if lim, ok := res["limits"].(map[string]any); ok {
			cpu, _ := lim["cpu"].(string)
			mem, _ := lim["memory"].(string)
			def.CPULimit, def.MemLimit = limitStrings(cpu, mem)
		}
	}
	return def
}

// limitStrings converts k8s quantity strings (e.g. "500m", "256Mi") to the
// CreateSpec representation: CPU in cores ("0.5") and memory in bytes
// ("268435456"). An empty or unparseable input yields an empty output (no
// limit). Shared by namedContainerDef and hostContainerLimits.
func limitStrings(cpu, mem string) (cpuCores, memBytes string) {
	if cpu != "" {
		if q, err := resource.ParseQuantity(cpu); err == nil {
			cpuCores = strconv.FormatFloat(float64(q.MilliValue())/1000.0, 'g', -1, 64)
		}
	}
	if mem != "" {
		if q, err := resource.ParseQuantity(mem); err == nil {
			memBytes = strconv.FormatInt(q.Value(), 10)
		}
	}
	return cpuCores, memBytes
}
