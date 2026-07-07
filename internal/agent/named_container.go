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

// namedContainerManager owns the long-lived, workspace-bind-mounted containers
// backing runsIn.container steps on the host agent, one per container name for
// the life of a claim. It is the mounted sibling of scopeManager: where a
// uses-scope container is isolated and needs copyIn/copyOut, a named container
// bind-mounts the host workDir at mountPath, so files it writes are already on
// the host and the non-scope cache/artifact/output paths see them directly.
//
// A claim's steps may run concurrently (parallel: stages are goroutines), and
// several may target the same container name. mu guards open across the
// check-and-create in ensure so a name is created at most once; see
// scopeManager's doc comment for the identical concurrency rationale.
type namedContainerManager struct {
	rt        crt.ContainerRuntime
	workDir   string
	mountPath string

	mu   sync.Mutex
	open map[string]crt.ContainerHandle
}

func newNamedContainerManager(rt crt.ContainerRuntime, workDir, mountPath string) *namedContainerManager {
	return &namedContainerManager{rt: rt, workDir: workDir, mountPath: mountPath, open: map[string]crt.ContainerHandle{}}
}

// ensure returns the container for def.Name, creating it on first use with the
// host workspace bind-mounted at mountPath. The lock is held across the
// check-and-create so concurrent callers racing on the same name never
// double-create.
func (m *namedContainerManager) ensure(ctx context.Context, def containerDef) (crt.ContainerHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.open[def.Name]; ok {
		return h, nil
	}
	h, err := m.rt.Create(ctx, crt.CreateSpec{
		Image:    def.Image,
		Env:      def.Env,
		CPULimit: def.CPULimit,
		MemLimit: def.MemLimit,
		WorkDir:  m.mountPath,
		Mounts:   []crt.Mount{{HostPath: m.workDir, ContainerPath: m.mountPath}},
	})
	if err != nil {
		return crt.ContainerHandle{}, fmt.Errorf("provision container %q (image %q): %w", def.Name, def.Image, err)
	}
	m.open[def.Name] = h
	return h, nil
}

func (m *namedContainerManager) exec(ctx context.Context, h crt.ContainerHandle, script string, env []string, stdout, stderr io.Writer) (int, error) {
	return m.rt.Exec(ctx, h, crt.ExecSpec{Script: script, Env: env}, stdout, stderr)
}

func (m *namedContainerManager) closeAll(ctx context.Context) {
	m.mu.Lock()
	handles := make([]crt.ContainerHandle, 0, len(m.open))
	for _, h := range m.open {
		handles = append(handles, h)
	}
	m.open = map[string]crt.ContainerHandle{}
	m.mu.Unlock()
	for _, h := range handles {
		if err := m.rt.Remove(ctx, h); err != nil {
			slog.Warn("named container teardown failed", "container", h.ID, "error", err)
		}
	}
}
