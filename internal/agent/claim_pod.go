package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"

	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"k8s.io/apimachinery/pkg/api/resource"
)

// containerDef is the host-supported subset of a podTemplate container: the
// fields the host agent can honor when backing a runsIn.container step or a
// claim pod sidecar. Every other k8s container field is ignored with a WARN
// in parseContainerDef.
type containerDef struct {
	Name     string
	Image    string
	Env      []string // KEY=VALUE
	CPULimit string   // cores, e.g. "0.5" (CreateSpec.CPULimit); empty = no limit
	MemLimit string   // bytes, e.g. "268435456" (CreateSpec.MemLimit); empty = no limit
}

// containerSupportedFields lists the podTemplate container keys the host honors.
// Anything else present on a container triggers a WARN (see parseContainerDef).
var containerSupportedFields = map[string]bool{
	"name": true, "image": true, "env": true, "resources": true,
}

// parseContainerDef extracts a containerDef from a raw podTemplate container
// map, keeping only host-supported fields. Host-unsupported fields (command,
// args, volumeMounts, ports, securityContext, envFrom, ...) are logged once
// per container and dropped.
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
// limit). Used by parseContainerDef.
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

const primaryContainerName = "job"

// claimContainerDefs returns every container the claim pod must run, in
// podTemplate order, injecting the default runner as the "job" (primary)
// container when the template does not define one. A nil podTemplate yields
// just the injected primary — the host twin of k8s defaultPodSpec.
//
// A podTemplate with two containers sharing the same name is malformed, but
// is not rejected at apply time (k8s itself would reject it as a Pod spec,
// but the host agent parses the raw map itself). Rather than silently
// letting a later duplicate overwrite an earlier container's handle (which
// would leak the first container — started, but no longer reachable via
// Exec's name lookup, and never torn down by name), the first definition
// for a given name wins and every later duplicate is dropped with a WARN.
func claimContainerDefs(pt *dsl.PodTemplate, runnerImage string) []containerDef {
	var defs []containerDef
	seen := map[string]bool{}
	if pt != nil {
		containers, _ := pt.Spec["containers"].([]any)
		for _, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := c["name"].(string)
			if name == "" {
				continue
			}
			if seen[name] {
				slog.Warn("podTemplate has more than one container with the same name; keeping the first and dropping the duplicate", "container", name)
				continue
			}
			seen[name] = true
			defs = append(defs, parseContainerDef(name, c))
		}
	}
	for _, d := range defs {
		if d.Name == primaryContainerName {
			return defs
		}
	}
	return append(defs, containerDef{Name: primaryContainerName, Image: runnerImage})
}

// claimPodManager emulates a k8s pod on the host runtime for one claim: a
// pause container owns the network namespace; every podTemplate container
// (plus the injected "job" primary) joins it via --network container: and
// bind-mounts the claim workspace. Sidecars are therefore reachable on
// localhost from every step, and two concurrent claims can never collide on
// ports (separate netns, nothing published).
type claimPodManager struct {
	rt          crt.ContainerRuntime
	workDir     string
	mountPath   string
	pauseImage  string
	runnerImage string

	mu    sync.Mutex
	pause crt.ContainerHandle
	open  map[string]crt.ContainerHandle // container name → handle
}

func newClaimPodManager(rt crt.ContainerRuntime, workDir, mountPath, pauseImage, runnerImage string) *claimPodManager {
	return &claimPodManager{rt: rt, workDir: workDir, mountPath: mountPath,
		pauseImage: pauseImage, runnerImage: runnerImage, open: map[string]crt.ContainerHandle{}}
}

// Start builds the claim pod eagerly: pause first (netns owner), then every
// container def. Sidecars must be listening before any step runs, which is
// why this is claim-start eager, not step-time lazy.
func (m *claimPodManager) Start(ctx context.Context, pt *dsl.PodTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pause, err := m.rt.Create(ctx, crt.CreateSpec{Image: m.pauseImage})
	if err != nil {
		return fmt.Errorf("claim pod: start pause container (image %q): %w", m.pauseImage, err)
	}
	m.pause = pause
	for _, def := range claimContainerDefs(pt, m.runnerImage) {
		h, err := m.rt.Create(ctx, crt.CreateSpec{
			Image:            def.Image,
			Env:              def.Env,
			CPULimit:         def.CPULimit,
			MemLimit:         def.MemLimit,
			WorkDir:          m.mountPath,
			Mounts:           []crt.Mount{{HostPath: m.workDir, ContainerPath: m.mountPath}},
			NetworkContainer: pause.ID,
		})
		if err != nil {
			m.closeAllLocked(ctx)
			return fmt.Errorf("claim pod: start container %q (image %q): %w", def.Name, def.Image, err)
		}
		m.open[def.Name] = h
	}
	return nil
}

// Exec runs script in the named claim-pod container; "" targets the primary
// ("job") container, mirroring k8s exec's empty-container fallback
// (internal/k8sagent/executor.go).
func (m *claimPodManager) Exec(ctx context.Context, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	if container == "" {
		container = primaryContainerName
	}
	m.mu.Lock()
	h, ok := m.open[container]
	m.mu.Unlock()
	if !ok {
		return -1, fmt.Errorf("container %q is not defined in the job's podTemplate", container)
	}
	return m.rt.Exec(ctx, h, crt.ExecSpec{Script: script, Env: env}, stdout, stderr)
}

func (m *claimPodManager) CloseAll(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeAllLocked(ctx)
}

func (m *claimPodManager) closeAllLocked(ctx context.Context) {
	for name, h := range m.open {
		if err := m.rt.Remove(ctx, h); err != nil {
			slog.Warn("claim pod teardown: container remove failed", "container", name, "error", err)
		}
	}
	m.open = map[string]crt.ContainerHandle{}
	if m.pause.ID != "" {
		if err := m.rt.Remove(ctx, m.pause); err != nil {
			slog.Warn("claim pod teardown: pause remove failed", "error", err)
		}
		m.pause = crt.ContainerHandle{}
	}
}
