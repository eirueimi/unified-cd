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
	// Entrypoint is the podTemplate container's command (ENTRYPOINT override,
	// CreateSpec.Entrypoint); nil = use the image's ENTRYPOINT. Args is its
	// args (CMD override, CreateSpec.Args); nil = use the image's CMD. A
	// service sidecar (mysql, redis, ...) that sets neither runs its image's
	// own entrypoint. claimPodManager.Start forces the primary "job"
	// container's Entrypoint to ucd-sh pause regardless of what the
	// podTemplate set, so it stays alive as an exec target.
	Entrypoint []string
	Args       []string
}

// parseContainerDef extracts a containerDef from a raw podTemplate container
// map, keeping only host-supported fields. command/args are parsed into
// Entrypoint/Args respectively (see the containerDef.Entrypoint/Args doc
// comment); they are listed in dsl.HostSupportedContainerFields, so a
// podTemplate that sets them is host-supported and is no longer forced onto
// a Kubernetes agent by PodTemplateNeedsKubernetes. Other host-unsupported
// fields (volumeMounts, ports, securityContext, envFrom, ...) are logged
// once per container and dropped.
func parseContainerDef(name string, c map[string]any) (containerDef, error) {
	def := containerDef{Name: name}
	def.Image, _ = c["image"].(string)

	for k := range c {
		if !dsl.HostSupportedContainerFields[k] {
			slog.Warn("podTemplate container field is not supported on the host agent and is ignored",
				"container", name, "field", k)
		}
	}

	def.Entrypoint = stringSlice(c["command"])
	def.Args = stringSlice(c["args"])

	if envs, ok := c["env"].([]any); ok {
		for _, raw := range envs {
			e, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			en, _ := e["name"].(string)
			if en == "" {
				continue
			}
			rawVal, present := e["value"]
			if !present {
				// valueFrom / fieldRef etc. — not resolvable on the host.
				slog.Warn("podTemplate container env without a literal value is ignored on the host agent",
					"container", name, "env", en)
				continue
			}
			ev, ok := rawVal.(string)
			if !ok {
				// A malformed job: k8s hard-errors on this (json.Unmarshal into
				// EnvVar{Value string}); the host matches instead of silently dropping.
				return containerDef{}, fmt.Errorf("podTemplate container %q env %q: value must be a string (got %T); quote the value", name, en, rawVal)
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
		if reqs, ok := res["requests"].(map[string]any); ok && len(reqs) > 0 {
			slog.Warn("podTemplate container resources.requests is not supported on the host agent "+
				"(docker/podman have no request concept) and is ignored; use resources.limits or route to a Kubernetes agent",
				"container", name)
		}
	}
	return def, nil
}

// stringSlice converts a raw podTemplate container's "command" or "args"
// value (decoded as []any of strings, mirroring k8s' []string fields) to
// []string, skipping non-string entries. A missing/wrong-typed key or an
// empty list yields nil. Used by parseContainerDef.
func stringSlice(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
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
func claimContainerDefs(pt *dsl.PodTemplate, runnerImage string) ([]containerDef, error) {
	var defs []containerDef
	seen := map[string]bool{}
	if pt != nil {
		containers, _ := pt.Spec["containers"].([]any)
		for idx, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := c["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("podTemplate container at index %d has no name", idx)
			}
			if seen[name] {
				slog.Warn("podTemplate has more than one container with the same name; keeping the first and dropping the duplicate", "container", name)
				continue
			}
			seen[name] = true
			def, err := parseContainerDef(name, c)
			if err != nil {
				return nil, err
			}
			defs = append(defs, def)
		}
	}
	for _, d := range defs {
		if d.Name == primaryContainerName {
			return defs, nil
		}
	}
	return append(defs, containerDef{Name: primaryContainerName, Image: runnerImage}), nil
}

// claimNeedsRunnerImage reports whether building the claim pod for pt would
// inject the default primary "job" container backed by RunnerImage. It mirrors
// claimContainerDefs' injection rule: false when pt already defines a container
// named primaryContainerName, in which case RunnerImage is unused and may be
// empty.
func claimNeedsRunnerImage(pt *dsl.PodTemplate) bool {
	if pt == nil {
		return true
	}
	containers, _ := pt.Spec["containers"].([]any)
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := c["name"].(string); name == primaryContainerName {
			return false
		}
	}
	return true
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
	// toolsDir is the host directory the agent wrote the embedded ucd-sh
	// shim into at startup (see Agent.InstallShim). Every container the
	// claim pod creates bind-mounts it read-only at /.ucd (ucdToolsMount),
	// so the shim is available as the exec target for the shell argv
	// default and as the pause/keep-alive binary. Empty means "no shim
	// mount" — used by tests that never exec anything shim-dependent; a
	// real agent (via cmd/agent's InstallShim wiring) always sets it.
	toolsDir string

	mu    sync.Mutex
	pause crt.ContainerHandle
	open  map[string]crt.ContainerHandle // container name → handle

	// podTemplate is the pt Start was called with, retained so SidecarHandles
	// can enumerate sidecar ordinals via dsl.SidecarContainerNames — the SAME
	// helper the controller and the k8s agent use. Deriving order any other
	// way (e.g. from claimContainerDefs, which de-dups same-named containers)
	// would let the host's ordinals diverge from the controller's for a
	// malformed podTemplate with duplicate container names.
	podTemplate *dsl.PodTemplate
}

func newClaimPodManager(rt crt.ContainerRuntime, workDir, mountPath, pauseImage, runnerImage, toolsDir string) *claimPodManager {
	return &claimPodManager{rt: rt, workDir: workDir, mountPath: mountPath,
		pauseImage: pauseImage, runnerImage: runnerImage, toolsDir: toolsDir, open: map[string]crt.ContainerHandle{}}
}

// ucdShPause is the Go keep-alive: it replaces "sleep infinity" for every
// container that must stay running as an exec target rather than run its
// image's own entrypoint (see crt.CreateSpec.Entrypoint) — the pause container
// (owns the claim pod's netns for its whole lifetime) and the primary "job"
// container (the exec target for container:-less steps). Unlike sleep
// infinity it requires no binary in the target image (it IS the shim,
// bind-mounted read-only at /.ucd — see ucdToolsMount), reaps zombies as
// PID 1, and exits promptly on SIGTERM instead of having to be killed.
var ucdShPause = []string{"/.ucd/ucd-sh", "pause"}

// ucdDefaultShell is the effective shell argv a container-targeted exec uses
// when the step carries no ClaimStep.Shell (the controller never writes
// this path itself — see api.ClaimStep.Shell's doc comment — so the agent
// applies it here, the one place that knows about /.ucd).
var ucdDefaultShell = []string{"/.ucd/ucd-sh", "-c"}

// effectiveShell returns shell if non-empty, else the shim default. Shared
// by every host exec path (claim pod, scope containers) so "nil/empty Shell
// means the shim default" is decided in exactly one place.
func effectiveShell(shell []string) []string {
	if len(shell) > 0 {
		return shell
	}
	return ucdDefaultShell
}

// ucdToolsMount returns the read-only /.ucd bind mount for toolsDir, or nil
// when toolsDir is empty (see claimPodManager.toolsDir's doc comment).
func ucdToolsMount(toolsDir string) []crt.Mount {
	if toolsDir == "" {
		return nil
	}
	return []crt.Mount{{HostPath: toolsDir, ContainerPath: "/.ucd", ReadOnly: true}}
}

// Start builds the claim pod eagerly: pause first (netns owner), then every
// container def. Sidecars must be listening before any step runs, which is
// why this is claim-start eager, not step-time lazy.
func (m *claimPodManager) Start(ctx context.Context, pt *dsl.PodTemplate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.podTemplate = pt
	// The pause container only owns the netns (nothing execs into it), but it
	// must outlive the whole claim: without an explicit keep-alive it would
	// run its image's default entrypoint and could exit immediately,
	// collapsing the netns every other claim container shares. It also needs
	// the /.ucd mount: ucdShPause IS the shim binary, not something the pause
	// image is expected to provide.
	pause, err := m.rt.Create(ctx, crt.CreateSpec{Image: m.pauseImage, Entrypoint: ucdShPause, Mounts: ucdToolsMount(m.toolsDir)})
	if err != nil {
		return fmt.Errorf("claim pod: start pause container (image %q): %w", m.pauseImage, err)
	}
	m.pause = pause
	defs, err := claimContainerDefs(pt, m.runnerImage)
	if err != nil {
		m.closeAllLocked(ctx)
		return fmt.Errorf("claim pod: %w", err)
	}
	for _, def := range defs {
		// A service sidecar runs its own entrypoint/CMD (Entrypoint/Args from
		// its podTemplate); the primary "job" container is forced to the
		// ucd-sh pause keep-alive via Entrypoint (clearing whatever ENTRYPOINT
		// its image declares) so it stays alive as the step exec target.
		entrypoint, cargs := def.Entrypoint, def.Args
		if def.Name == primaryContainerName {
			entrypoint, cargs = ucdShPause, nil
		}
		mounts := append([]crt.Mount{{HostPath: m.workDir, ContainerPath: m.mountPath}}, ucdToolsMount(m.toolsDir)...)
		h, err := m.rt.Create(ctx, crt.CreateSpec{
			Image:            def.Image,
			Env:              def.Env,
			CPULimit:         def.CPULimit,
			MemLimit:         def.MemLimit,
			WorkDir:          m.mountPath,
			Mounts:           mounts,
			NetworkContainer: pause.ID,
			Entrypoint:       entrypoint,
			Args:             cargs,
		})
		if err != nil {
			m.closeAllLocked(ctx)
			return fmt.Errorf("claim pod: start container %q (image %q): %w", def.Name, def.Image, err)
		}
		m.open[def.Name] = h
	}
	return nil
}

// SidecarHandles returns the live user sidecar containers (every non-"job"
// container), each tagged with its ordinal so the caller can compute its log
// index via dsl.SidecarLogIndex. Ordinals are derived from
// dsl.SidecarContainerNames(m.podTemplate) — the SAME helper the controller
// (planned_steps.go) and the k8s agent use — rather than from
// claimContainerDefs' de-duplicated order, so a malformed podTemplate with a
// duplicate container name still yields ordinals the controller agrees with:
// a duplicate name here produces two SidecarHandle entries (ordinals k and
// k+1) that both resolve to the single open handle claimContainerDefs
// actually created (it keeps the first and drops the duplicate with a WARN —
// see claimContainerDefs' doc comment). Names with no open handle (should not
// happen in practice, since claimContainerDefs creates one for every name
// SidecarContainerNames returns) are skipped.
func (m *claimPodManager) SidecarHandles() []SidecarHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SidecarHandle
	for k, name := range dsl.SidecarContainerNames(m.podTemplate) {
		if h, ok := m.open[name]; ok {
			out = append(out, SidecarHandle{Name: name, Ordinal: k, Handle: h})
		}
	}
	return out
}

// Exec runs script in the named claim-pod container; "" targets the primary
// ("job") container, mirroring k8s exec's empty-container fallback
// (internal/k8sagent/executor.go). shell is the step's effective interpreter
// argv (nil/empty resolves to the shim default — see effectiveShell); it is
// always set explicitly here so the runtime layer stays dumb (crt.ExecSpec's
// own fallback is only for callers outside the agent).
func (m *claimPodManager) Exec(ctx context.Context, container, script string, shell, env []string, stdout, stderr io.Writer) (int, error) {
	if container == "" {
		container = primaryContainerName
	}
	m.mu.Lock()
	h, ok := m.open[container]
	m.mu.Unlock()
	if !ok {
		return -1, fmt.Errorf("container %q is not defined in the job's podTemplate", container)
	}
	return m.rt.Exec(ctx, h, crt.ExecSpec{Script: script, Shell: effectiveShell(shell), Env: env}, stdout, stderr)
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
