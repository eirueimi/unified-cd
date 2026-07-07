# Host `runsIn.container` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `runsIn.container: X` execute on the host agent by running the step in a long-lived, workspace-bind-mounted container named `X`, sourced from the job's `podTemplate.spec.containers` — reaching parity with the k8s agent.

**Architecture:** Implement the existing `ExecBackend.RunNamedContainer` hook on `hostBackend`. A new claim-scoped `namedContainerManager` (sibling of `scopeManager`) lazily creates one bind-mounted container per referenced name and execs steps into it. Because the container bind-mounts the host `workDir`, `cache`/`uploadArtifact`/`downloadArtifact`/`outputs:` continue to use the existing non-scope host paths and "just work" (no copyIn/copyOut). The host's podTemplate hard-reject guard is relaxed to a WARN so such claims run.

**Tech Stack:** Go, `internal/runtime` (docker/podman/nerdctl/Apple `container` CLI drivers), `internal/agent` (host ExecBackend), `k8s.io/apimachinery/pkg/api/resource` (already a dependency).

## Global Constraints

- All code, comments, doc text, and commit messages are English-only (AGENTS.md).
- Work happens in the worktree `unified-cd-hostcontainer-spec` on branch `host-runsin-container`; commit there, never on `main`.
- Every commit message ends with the trailer: `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- A named container is kept alive with `sleep infinity` (reuse the existing `createArgs` behavior). A podTemplate container's `command`, if present, is host-unsupported → WARN and ignore (do NOT honor it in MVP).
- Host-supported container fields = `name`, `image`, `env` (literal name/value only), `resources.limits.cpu`/`.memory`. Every other container field is ignored with a WARN.
- Container mount path defaults to `/workspace`; if `podTemplate.workspace.mountPath` is set, use that.
- No new DSL fields are introduced (`runsIn.container` already parses), so **no JSON-schema regeneration is required**.
- Container paths are always Linux (forward-slash) regardless of host OS.

---

### Task 1: Runtime bind-mount support (`CreateSpec.Mounts`)

Add a bind-mount field to the container-create spec and emit `-v host:container` in both CLI drivers, so a long-lived container can share the host workspace.

**Files:**
- Modify: `internal/runtime/runtime.go` (add `Mount` type; add `Mounts []Mount` to `CreateSpec`)
- Modify: `internal/runtime/ocicli.go:70-86` (`createArgs`)
- Modify: `internal/runtime/apple.go:63-79` (`createArgs`)
- Test: `internal/runtime/ocicli_lifecycle_test.go`, `internal/runtime/apple_lifecycle_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `runtime.Mount{HostPath, ContainerPath string}`; `runtime.CreateSpec.Mounts []Mount`. Each mount emits `-v <HostPath>:<ContainerPath>` in `run -d` argv. Empty `Mounts` emits no `-v` (unchanged behavior).

- [ ] **Step 1: Write the failing test (ociCLI)**

Add to `internal/runtime/ocicli_lifecycle_test.go`:

```go
func TestOCICLICreateArgv_Mounts(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{
		Image:   "alpine",
		WorkDir: "/workspace",
		Mounts:  []Mount{{HostPath: "/host/ws", ContainerPath: "/workspace"}},
	})
	found := false
	for i, a := range got {
		if a == "-v" {
			found = true
			if i+1 >= len(got) || got[i+1] != "/host/ws:/workspace" {
				t.Fatalf("expected -v /host/ws:/workspace, argv = %v", got)
			}
		}
	}
	if !found {
		t.Fatalf("expected -v in argv, got %v", got)
	}
}

func TestOCICLICreateArgv_NoMountsWhenEmpty(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "alpine"})
	for _, a := range got {
		if a == "-v" {
			t.Fatalf("expected no -v flag when Mounts is empty, argv = %v", got)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/runtime/ -run TestOCICLICreateArgv_Mounts -v`
Expected: FAIL — compile error (`Mount` / `CreateSpec.Mounts` undefined).

- [ ] **Step 3: Add the `Mount` type and `CreateSpec.Mounts` field**

In `internal/runtime/runtime.go`, add after the `CreateSpec` struct (after line 38):

```go
// Mount is a host-path bind mount for a long-lived container: the host
// directory HostPath is made available inside the container at ContainerPath
// (docker/podman/Apple container's `run -v host:container`). Used to share the
// host workspace with a named runsIn.container container.
type Mount struct {
	HostPath      string
	ContainerPath string
}
```

Add the field to `CreateSpec` (inside the struct, after `WorkDir`):

```go
	// Mounts are host-path bind mounts (docker `run -v`). Empty means no bind
	// mounts (an isolated uses-scope container); a named runsIn.container
	// container sets one mount to share the host workspace.
	Mounts []Mount
```

- [ ] **Step 4: Emit `-v` in ociCLI.createArgs**

In `internal/runtime/ocicli.go`, in `createArgs`, insert after the `WorkDir` block (after line 80, before the `for _, e := range spec.Env` loop):

```go
	for _, m := range spec.Mounts {
		args = append(args, "-v", m.HostPath+":"+m.ContainerPath)
	}
```

- [ ] **Step 5: Emit `-v` in appleContainer.createArgs**

In `internal/runtime/apple.go`, in `createArgs`, insert the identical block after the `WorkDir` block (after line 73, before the Env loop):

```go
	for _, m := range spec.Mounts {
		args = append(args, "-v", m.HostPath+":"+m.ContainerPath)
	}
```

- [ ] **Step 6: Add the apple parity test**

Add to `internal/runtime/apple_lifecycle_test.go`:

```go
func TestAppleCreateArgv_Mounts(t *testing.T) {
	a := &appleContainer{}
	got := a.createArgs(CreateSpec{
		Image:   "alpine",
		WorkDir: "/workspace",
		Mounts:  []Mount{{HostPath: "/host/ws", ContainerPath: "/workspace"}},
	})
	found := false
	for i, s := range got {
		if s == "-v" && i+1 < len(got) && got[i+1] == "/host/ws:/workspace" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected -v /host/ws:/workspace, argv = %v", got)
	}
}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/runtime/ -v`
Expected: PASS (all runtime tests, including the new argv tests).

- [ ] **Step 8: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/ocicli.go internal/runtime/apple.go internal/runtime/ocicli_lifecycle_test.go internal/runtime/apple_lifecycle_test.go
git commit -m "$(cat <<'EOF'
feat(runtime): add bind-mount support to CreateSpec

Mount type + CreateSpec.Mounts, emitted as `-v host:container` in the
ociCLI and appleContainer `run -d` argv. Empty Mounts is unchanged.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: podTemplate container extraction (`namedContainerDef`)

Parse a named container definition out of the job's `podTemplate.spec.containers` map, keeping only host-supported fields and WARNing on the rest.

**Files:**
- Create: `internal/agent/named_container.go` (the `containerDef` type + `namedContainerDef` + a limit-conversion helper)
- Modify: `internal/agent/agent.go:26-41` (refactor `hostContainerLimits` to share the limit-conversion helper — DRY)
- Test: `internal/agent/named_container_test.go`

**Interfaces:**
- Consumes: `dsl.PodTemplate` (`Spec map[string]any`, `Workspace *dsl.WorkspaceConfig`).
- Produces:
  - `type containerDef struct { Name, Image string; Env []string; CPULimit, MemLimit string }`
  - `func namedContainerDef(pt *dsl.PodTemplate, name string) (containerDef, error)`
  - `func limitStrings(cpu, mem string) (cpuCores, memBytes string)` — converts k8s quantity strings to CreateSpec cores/bytes (used by `namedContainerDef` and `hostContainerLimits`).

- [ ] **Step 1: Write the failing tests**

Create `internal/agent/named_container_test.go`:

```go
package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func podTmpl(containers ...map[string]any) *dsl.PodTemplate {
	cs := make([]any, len(containers))
	for i, c := range containers {
		cs[i] = c
	}
	return &dsl.PodTemplate{Spec: map[string]any{"containers": cs}}
}

func TestNamedContainerDef_Found(t *testing.T) {
	pt := podTmpl(
		map[string]any{"name": "other", "image": "busybox"},
		map[string]any{
			"name":  "tools",
			"image": "node:20",
			"env":   []any{map[string]any{"name": "FOO", "value": "bar"}},
			"resources": map[string]any{
				"limits": map[string]any{"cpu": "500m", "memory": "256Mi"},
			},
		},
	)
	def, err := namedContainerDef(pt, "tools")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if def.Name != "tools" || def.Image != "node:20" {
		t.Fatalf("unexpected def: %+v", def)
	}
	if len(def.Env) != 1 || def.Env[0] != "FOO=bar" {
		t.Fatalf("env = %v, want [FOO=bar]", def.Env)
	}
	if def.CPULimit != "0.5" {
		t.Fatalf("CPULimit = %q, want 0.5", def.CPULimit)
	}
	if def.MemLimit != "268435456" {
		t.Fatalf("MemLimit = %q, want 268435456", def.MemLimit)
	}
}

func TestNamedContainerDef_NoPodTemplate(t *testing.T) {
	if _, err := namedContainerDef(nil, "tools"); err == nil {
		t.Fatal("expected error when podTemplate is nil")
	}
}

func TestNamedContainerDef_UnknownName(t *testing.T) {
	pt := podTmpl(map[string]any{"name": "tools", "image": "node:20"})
	if _, err := namedContainerDef(pt, "missing"); err == nil {
		t.Fatal("expected error when container name is absent")
	}
}

func TestLimitStrings(t *testing.T) {
	cpu, mem := limitStrings("500m", "256Mi")
	if cpu != "0.5" {
		t.Fatalf("cpu = %q, want 0.5", cpu)
	}
	if mem != "268435456" {
		t.Fatalf("mem = %q, want 268435456", mem)
	}
	if c, m := limitStrings("", ""); c != "" || m != "" {
		t.Fatalf("empty in must yield empty out, got %q %q", c, m)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run 'TestNamedContainerDef|TestLimitStrings' -v`
Expected: FAIL — compile error (`namedContainerDef` / `limitStrings` / `containerDef` undefined).

- [ ] **Step 3: Create `internal/agent/named_container.go` with the extraction logic**

```go
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
```

- [ ] **Step 4: Refactor `hostContainerLimits` to reuse `limitStrings` (DRY)**

In `internal/agent/agent.go`, replace the body of `hostContainerLimits` (lines 26-41) with:

```go
func hostContainerLimits(rs *dsl.ResourceSpec) (cpu, mem string) {
	if rs == nil || rs.Limits == nil {
		return "", ""
	}
	return limitStrings(rs.Limits.CPU, rs.Limits.Memory)
}
```

If this leaves `strconv` and/or `resource` unused in `agent.go`, remove those imports (the compiler will name any now-unused import; delete exactly those).

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run 'TestNamedContainerDef|TestLimitStrings' -v`
Expected: PASS.

- [ ] **Step 6: Run the full agent package to confirm the refactor is safe**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ 2>&1 | tail -5`
Expected: PASS (no regression from the `hostContainerLimits` refactor).

- [ ] **Step 7: Commit**

```bash
git add internal/agent/named_container.go internal/agent/named_container_test.go internal/agent/agent.go
git commit -m "$(cat <<'EOF'
feat(agent): extract host-supported container def from podTemplate

namedContainerDef reads name/image/env/resources.limits from
podTemplate.spec.containers and WARNs on host-unsupported fields.
hostContainerLimits now shares the limitStrings quantity converter.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: `namedContainerManager`

A claim-scoped manager that lazily creates one workspace-bind-mounted container per name and execs into it — the mounted sibling of `scopeManager`.

**Files:**
- Modify: `internal/agent/named_container.go` (append the manager)
- Test: `internal/agent/named_container_test.go` (append; may reuse `fakeRT` from `scope_test.go`, same package)

**Interfaces:**
- Consumes: `containerDef` (Task 2); `runtime.CreateSpec.Mounts` (Task 1); `crt.ContainerRuntime`.
- Produces:
  - `func newNamedContainerManager(rt crt.ContainerRuntime, workDir, mountPath string) *namedContainerManager`
  - `(*namedContainerManager).ensure(ctx, def containerDef) (crt.ContainerHandle, error)` — one Create per `def.Name`, bind-mounting `workDir` at `mountPath`; cached thereafter.
  - `(*namedContainerManager).exec(ctx, h, script string, env []string, stdout, stderr io.Writer) (int, error)`
  - `(*namedContainerManager).closeAll(ctx)` — Remove every open container, best-effort.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/named_container_test.go` (add imports `context`, `io`, `sync`, `sync/atomic`, and `crt "github.com/eirueimi/unified-cd/internal/runtime"`):

```go
// recordingRT records every CreateSpec it is asked to Create, and is safe for
// concurrent use.
type recordingRT struct {
	mu      sync.Mutex
	specs   []crt.CreateSpec
	creates atomic.Int64
	removes atomic.Int64
}

func (r *recordingRT) Name() string                                   { return "recording" }
func (r *recordingRT) Available() bool                                { return true }
func (r *recordingRT) Pull(context.Context, string) error            { return nil }
func (r *recordingRT) Run(context.Context, crt.RunSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (r *recordingRT) Create(_ context.Context, spec crt.CreateSpec) (crt.ContainerHandle, error) {
	r.creates.Add(1)
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	n := len(r.specs)
	r.mu.Unlock()
	return crt.ContainerHandle{ID: fmt.Sprintf("c%d", n)}, nil
}
func (r *recordingRT) Exec(context.Context, crt.ContainerHandle, crt.ExecSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (r *recordingRT) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (r *recordingRT) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (r *recordingRT) Remove(context.Context, crt.ContainerHandle) error {
	r.removes.Add(1)
	return nil
}

func TestNamedContainerManager_ReusesPerNameAndBindMounts(t *testing.T) {
	rt := &recordingRT{}
	m := newNamedContainerManager(rt, "/host/ws", "/workspace")
	ctx := context.Background()
	def := containerDef{Name: "tools", Image: "node:20"}

	h1, err := m.ensure(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := m.ensure(ctx, def)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("expected same handle for same name, got %v and %v", h1, h2)
	}
	if got := rt.creates.Load(); got != 1 {
		t.Fatalf("expected 1 Create for a reused name, got %d", got)
	}
	spec := rt.specs[0]
	if spec.WorkDir != "/workspace" {
		t.Fatalf("WorkDir = %q, want /workspace", spec.WorkDir)
	}
	if len(spec.Mounts) != 1 || spec.Mounts[0].HostPath != "/host/ws" || spec.Mounts[0].ContainerPath != "/workspace" {
		t.Fatalf("Mounts = %+v, want one /host/ws:/workspace", spec.Mounts)
	}

	m.closeAll(ctx)
	if got := rt.removes.Load(); got != 1 {
		t.Fatalf("expected 1 Remove, got %d", got)
	}
}

// TestNamedContainerManager_ConcurrentSameName must be run with -race: many
// goroutines racing to ensure() the same name (parallel: steps sharing a
// container) must produce exactly one Create.
func TestNamedContainerManager_ConcurrentSameName(t *testing.T) {
	rt := &recordingRT{}
	m := newNamedContainerManager(rt, "/host/ws", "/workspace")
	def := containerDef{Name: "tools", Image: "node:20"}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.ensure(context.Background(), def); err != nil {
				t.Errorf("ensure: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := rt.creates.Load(); got != 1 {
		t.Fatalf("expected exactly 1 Create under concurrency, got %d", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run TestNamedContainerManager -v`
Expected: FAIL — compile error (`newNamedContainerManager` undefined).

- [ ] **Step 3: Append the manager to `internal/agent/named_container.go`**

Add these imports to the file's import block: `context`, `io`, `sync`, and `crt "github.com/eirueimi/unified-cd/internal/runtime"`. Then append:

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run TestNamedContainerManager -race -v`
Expected: PASS (including the `-race` concurrency test).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/named_container.go internal/agent/named_container_test.go
git commit -m "$(cat <<'EOF'
feat(agent): add namedContainerManager for host runsIn.container

Lazily creates one workspace-bind-mounted container per name for the
life of a claim, mutex-guarded for parallel: steps. Mounted sibling of
scopeManager (no copyIn/copyOut — the workspace is shared).

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Relax the host podTemplate guard; thread podTemplate into `hostBackend`

Stop hard-rejecting podTemplate claims on the host, and give the backend the podTemplate it needs to resolve named containers. This task changes only construction + the guard; `RunNamedContainer` still returns its stub error until Task 5.

**Files:**
- Modify: `internal/agent/backend_host.go:22-36` (struct fields + `newHostBackend` signature)
- Modify: `internal/agent/agent.go:278-289` (`executeRun` guard → WARN)
- Modify: `internal/agent/orchestrator.go:42-44` (update the stale doc comment referencing "the host agent's podTemplate rejection")
- Modify callers: `internal/agent/agent_cache_test.go` (lines 57, 75, 95, 108, 120), `internal/agent/agent_cache_stageindex_test.go:43`, `internal/agent/backend_host_test.go` (lines 18, 29, 40, 68, 84) — add the trailing `nil` argument
- Test: `internal/agent/agent_runsin_test.go` (add the guard-relaxation test)

**Interfaces:**
- Consumes: `dsl.PodTemplate` (from `api.ClaimResponse.PodTemplate`).
- Produces: `newHostBackend(a *Agent, runID, workDir string, podTemplate *dsl.PodTemplate) *hostBackend`; `hostBackend.podTemplate *dsl.PodTemplate`; plus unused-yet `hostBackend.named`/`namedMu` fields consumed by Task 5.

- [ ] **Step 1: Write the failing test (guard relaxation)**

Add to `internal/agent/agent_runsin_test.go` (it already imports `net/http`, `net/http/httptest`, `sync`, `api`, `dsl`, `assert`):

```go
// TestExecuteRun_PodTemplate_NotRejectedOnHost verifies the host agent no
// longer hard-fails a claim merely because it carries a podTemplate: with the
// guard relaxed, an empty-stage podTemplate claim finishes Succeeded (the
// podTemplate is only consulted to resolve runsIn.container definitions).
func TestExecuteRun_PodTemplate_NotRejectedOnHost(t *testing.T) {
	const agentID = "pt-agent"
	const runID = "run-podtemplate"

	finishCh := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Status string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	claim := api.ClaimResponse{
		RunID:       runID,
		JobName:     "pt-job",
		PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{}}},
		// No stages: nothing to run, so the claim should finish Succeeded.
	}

	a.executeRun(context.Background(), claim, t.TempDir())

	select {
	case s := <-finishCh:
		assert.Equal(t, "Succeeded", s, "a podTemplate claim must no longer be rejected on the host agent")
	default:
		t.Fatal("FinishRun was not called")
	}
}
```

Note: this test uses `json` — `agent_runsin_test.go` already imports `encoding/json`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run TestExecuteRun_PodTemplate_NotRejectedOnHost -v`
Expected: FAIL — the guard reports `Failed` (and/or `newHostBackend` arity compile error once Step 4 is partially applied). Before any source change it fails on the assertion (`Failed` != `Succeeded`).

- [ ] **Step 3: Add the podTemplate + named-manager fields and update `newHostBackend`**

In `internal/agent/backend_host.go`, replace the `hostBackend` struct and `newHostBackend` (lines 22-36) with:

```go
// hostBackend is the ExecBackend implementation for the host (bare-process)
// agent. It owns the claim-scoped scopeManager (uses-scope containers) and the
// namedContainerManager (runsIn.container containers), both created lazily on
// first use, plus the secret masker used by StepLogWriters. podTemplate is the
// claim's podTemplate (nil for a plain claim); it is consulted only to resolve
// runsIn.container definitions.
type hostBackend struct {
	a           *Agent
	runID       string
	workDir     string
	podTemplate *dsl.PodTemplate

	scopesMu sync.Mutex
	scopes   *scopeManager

	namedMu sync.Mutex
	named   *namedContainerManager

	masker *secrets.Masker
}

// newHostBackend constructs the ExecBackend for one claim's executeRun call.
// podTemplate is api.ClaimResponse.PodTemplate (nil when the claim has none).
func newHostBackend(a *Agent, runID, workDir string, podTemplate *dsl.PodTemplate) *hostBackend {
	return &hostBackend{a: a, runID: runID, workDir: workDir, podTemplate: podTemplate}
}
```

Add `"github.com/eirueimi/unified-cd/internal/dsl"` to the import block of `backend_host.go`.

- [ ] **Step 4: Relax the guard in `executeRun`**

In `internal/agent/agent.go`, replace the `executeRun` body (lines 278-289) with:

```go
func (a *Agent) executeRun(ctx context.Context, c api.ClaimResponse, workDir string) {
	if c.PodTemplate != nil {
		// The host cannot honor k8s-only podTemplate features (PVC workspace,
		// extra pod-spec containers/volumes, the artifact sidecar). It uses the
		// podTemplate only to resolve runsIn.container definitions; every other
		// step runs on the host workspace. Warn once so a misrouted k8s job is
		// diagnosable, but do not reject it (a host agent may legitimately be
		// selected for a runsIn.container job).
		slog.Warn("host agent ignores host-unsupported podTemplate features (PVC workspace, extra pod-spec containers/volumes, artifact sidecar); podTemplate is used only to resolve runsIn.container definitions",
			"runId", c.RunID, "job", c.JobName)
	}

	backend := newHostBackend(a, c.RunID, workDir, c.PodTemplate)
	RunClaim(ctx, a.Client, a.ID, c, backend)
}
```

Also update the `executeRun` doc comment (lines 270-277) so it no longer says a podTemplate claim "is rejected here": change the sentence describing the "ONE thing only the host agent needs to decide" to state that a podTemplate claim is accepted with a WARN (host-unsupported features ignored) and the podTemplate is threaded into `hostBackend` for runsIn.container resolution.

- [ ] **Step 5: Fix the stale RunClaim doc comment**

In `internal/agent/orchestrator.go`, lines 42-44 read "... and any host/k8s-only rejection (e.g. the host agent's podTemplate rejection, which stays in its wrapper since only the host cares)." Replace that parenthetical with: "(e.g. the host agent's podTemplate handling — it warns that host-unsupported features are ignored and threads the podTemplate into hostBackend for runsIn.container resolution)."

- [ ] **Step 6: Update all `newHostBackend` call sites**

Add a trailing `nil` (no podTemplate) to each existing caller:
- `internal/agent/agent_cache_test.go` lines 57, 75, 95, 108, 120: `newHostBackend(a, "r1", "")` → `newHostBackend(a, "r1", "", nil)`
- `internal/agent/agent_cache_stageindex_test.go` line 43: same change
- `internal/agent/backend_host_test.go` lines 18, 29, 40, 68, 84: `newHostBackend(<args>, t.TempDir())` → `newHostBackend(<args>, t.TempDir(), nil)`

Run `grep -rn 'newHostBackend(' internal/` and confirm every non-definition call passes 4 arguments.

- [ ] **Step 7: Run the guard test and the full package**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run TestExecuteRun_PodTemplate_NotRejectedOnHost -v`
Expected: PASS.

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ 2>&1 | tail -5`
Expected: PASS. Note `TestExecuteRun_RunsInContainer_HostAgentHardError` still passes: with no podTemplate, `RunNamedContainer`'s stub still errors, so that step is still Failed with exit -1.

- [ ] **Step 8: Commit**

```bash
git add internal/agent/backend_host.go internal/agent/agent.go internal/agent/orchestrator.go internal/agent/agent_cache_test.go internal/agent/agent_cache_stageindex_test.go internal/agent/backend_host_test.go internal/agent/agent_runsin_test.go
git commit -m "$(cat <<'EOF'
feat(agent): accept podTemplate claims on the host (WARN, don't reject)

Relax executeRun's hard podTemplate rejection to a WARN and thread the
claim's podTemplate into hostBackend so runsIn.container can resolve
container definitions. Adds the (still-unused) named-manager fields.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Implement `hostBackend.RunNamedContainer` (+ teardown, post-hook, agent OS)

Wire the manager and extraction together so a `runsIn.container` step actually runs in its named container, tears down at claim end, runs its `post:` in the same container, and reports `UNIFIED_AGENT_OS=linux`.

**Files:**
- Modify: `internal/agent/backend_host.go` (`RunNamedContainer`, a `namedContainers()` lazy ctor, a `hostNamedMountPath` helper, `CloseScopes`, `RunPostHook`)
- Modify: `internal/agent/agent_os.go:14-19` (`agentOSForStep`)
- Modify: `internal/agent/agent_runsin_test.go` (rename/retarget the stale hard-error test; add the success test)
- Test: `internal/agent/backend_host_test.go` (add `agentOSForStep` case if a host-side OS test lives here) and `internal/agent/agent_runsin_test.go`

**Interfaces:**
- Consumes: `namedContainerDef` (Task 2), `newNamedContainerManager`/`ensure`/`exec`/`closeAll` (Task 3), `hostBackend.podTemplate`/`named`/`namedMu` (Task 4).
- Produces: a working `RunNamedContainer`; `agentOSForStep` returns `"linux"` for a `runsIn.container` step.

- [ ] **Step 1: Write the failing tests**

In `internal/agent/agent_runsin_test.go`, **replace** `TestExecuteRun_RunsInContainer_HostAgentHardError` (its premise — "runsIn.container is never supported on the host" — is now false) with a version that pins the genuinely-still-erroring case (a `runsIn.container` step with **no** podTemplate defining it), and **add** a success test. Seed a fake runtime the way `agent_scope_test.go` does (`a.runtimeOnce.Do(func(){})` + set `a.resolvedRuntime`).

First, add a package-level fake exec target for the success path. Append these tests:

```go
// TestExecuteRun_RunsInContainer_NoPodTemplate: a runsIn.container step whose
// container is not defined in any podTemplate must fail (exit -1), never
// silently running its command on the host.
func TestExecuteRun_RunsInContainer_NoPodTemplate(t *testing.T) {
	const agentID = "runsin-agent"
	const runID = "run-runsin-nopt"

	var mu sync.Mutex
	var reportedStatus string
	var reportedExitCode int
	finishCh := make(chan string, 1)

	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "marker.txt")

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", func(w http.ResponseWriter, r *http.Request) {
		var req api.StepReportRequest
		json.NewDecoder(r.Body).Decode(&req) //nolint:errcheck
		if req.Status == "Running" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		mu.Lock()
		reportedStatus = req.Status
		reportedExitCode = req.ExitCode
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/logs/bulk", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/steps/0/logs/bulk", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/"+runID+"/finish", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Status string `json:"status"` }
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		select {
		case finishCh <- body.Status:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}
	claim := api.ClaimResponse{
		RunID:   runID,
		JobName: "test-runsin",
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{
			Index: 0, StageIndex: 0, Name: "container-step",
			RunsIn: &dsl.RunsIn{Container: "tools"},
			Run:    "echo ran > " + shellQuote(markerPath),
		}}},
	}

	a.executeRun(context.Background(), claim, tmpDir)

	select {
	case s := <-finishCh:
		assert.Equal(t, "Failed", s, "runsIn.container without a defining podTemplate must fail")
	default:
		t.Fatal("FinishRun was not called")
	}
	mu.Lock()
	status, exitCode := reportedStatus, reportedExitCode
	mu.Unlock()
	assert.Equal(t, "Failed", status)
	assert.Equal(t, -1, exitCode)
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker must not exist; step must not run on host (stat err: %v)", err)
	}
}
```

Add a unit test for `RunNamedContainer` success using a seeded fake runtime (reuse `recordingRT` from `named_container_test.go`, same package):

```go
func TestHostBackend_RunNamedContainer_ExecsIntoNamedContainer(t *testing.T) {
	rt := &recordingRT{}
	a := &Agent{ID: "a1"}
	a.runtimeOnce.Do(func() {}) // mark runtime as resolved
	a.resolvedRuntime = rt

	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "tools", "image": "node:20"},
	}}}
	b := newHostBackend(a, "r1", "/host/ws", pt)

	step := api.ClaimStep{Index: 0, Name: "s", RunsIn: &dsl.RunsIn{Container: "tools"}}
	ec, err := b.RunNamedContainer(context.Background(), step, "tools", "echo hi", nil, nil, nil)
	if err != nil {
		t.Fatalf("RunNamedContainer: %v", err)
	}
	if ec != 0 {
		t.Fatalf("exit = %d, want 0", ec)
	}
	if got := rt.creates.Load(); got != 1 {
		t.Fatalf("expected 1 Create, got %d", got)
	}
	if len(rt.specs) != 1 || len(rt.specs[0].Mounts) != 1 || rt.specs[0].Mounts[0].HostPath != "/host/ws" {
		t.Fatalf("expected workspace bind mount from /host/ws, got %+v", rt.specs)
	}
	b.CloseScopes(context.Background())
	if got := rt.removes.Load(); got != 1 {
		t.Fatalf("expected teardown Remove, got %d", got)
	}
}

func TestHostBackend_RunNamedContainer_UnknownContainer(t *testing.T) {
	rt := &recordingRT{}
	a := &Agent{ID: "a1"}
	a.runtimeOnce.Do(func() {})
	a.resolvedRuntime = rt
	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{}}}
	b := newHostBackend(a, "r1", "/host/ws", pt)

	step := api.ClaimStep{Name: "s", RunsIn: &dsl.RunsIn{Container: "missing"}}
	if _, err := b.RunNamedContainer(context.Background(), step, "missing", "echo hi", nil, nil, nil); err == nil {
		t.Fatal("expected error for a container not in the podTemplate")
	}
}

func TestAgentOSForStep_RunsInContainer(t *testing.T) {
	step := api.ClaimStep{RunsIn: &dsl.RunsIn{Container: "tools"}}
	if got := agentOSForStep(step, "windows"); got != "linux" {
		t.Fatalf("agentOSForStep = %q, want linux for runsIn.container", got)
	}
}
```

Confirm the field name for the seeded runtime: `agent_scope_test.go` sets it via `a.resolvedRuntime` (see lines ~133-139). If the field is named differently there, match that exact name.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run 'RunNamedContainer|AgentOSForStep_RunsInContainer|NoPodTemplate' -v`
Expected: FAIL — `RunNamedContainer` still returns the stub error; `agentOSForStep` returns `windows`.

- [ ] **Step 3: Implement `RunNamedContainer` + helpers in `backend_host.go`**

Replace the stub `RunNamedContainer` (lines 96-101) with:

```go
// hostNamedMountPath is the in-container path the host workspace is bind-mounted
// at for runsIn.container containers. It mirrors the k8s workspace mount
// (podbuilder.injectWorkspace): /workspace unless the podTemplate overrides it.
func hostNamedMountPath(pt *dsl.PodTemplate) string {
	if pt != nil && pt.Workspace != nil && pt.Workspace.MountPath != "" {
		return pt.Workspace.MountPath
	}
	return "/workspace"
}

// namedContainers returns the claim's namedContainerManager, creating it lazily
// on first use (mirrors getScopes). A missing container runtime is a hard error
// surfaced to the step (no silent host fallback).
func (b *hostBackend) namedContainers() (*namedContainerManager, error) {
	b.namedMu.Lock()
	defer b.namedMu.Unlock()
	if b.named != nil {
		return b.named, nil
	}
	rt, err := b.a.containerRuntime()
	if err != nil {
		return nil, fmt.Errorf("runsIn.container requires a container runtime: %w", err)
	}
	b.named = newNamedContainerManager(rt, b.workDir, hostNamedMountPath(b.podTemplate))
	return b.named, nil
}

// RunNamedContainer runs a runsIn.container step in a long-lived container named
// `container`, defined in the claim's podTemplate and sharing the host
// workspace via a bind mount. This is the host counterpart to the k8s agent's
// exec-into-named-pod-container behavior.
func (b *hostBackend) RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error) {
	def, err := namedContainerDef(b.podTemplate, container)
	if err != nil {
		return -1, err
	}
	nm, err := b.namedContainers()
	if err != nil {
		return -1, err
	}
	h, err := nm.ensure(ctx, def)
	if err != nil {
		return -1, err
	}
	return nm.exec(ctx, h, script, env, stdout, stderr)
}
```

- [ ] **Step 4: Tear down named containers in `CloseScopes`**

Replace `CloseScopes` (lines 126-133) with:

```go
// CloseScopes tears down every scope container and every named runsIn.container
// container opened during the claim.
func (b *hostBackend) CloseScopes(ctx context.Context) {
	b.scopesMu.Lock()
	scopes := b.scopes
	b.scopesMu.Unlock()
	if scopes != nil {
		scopes.closeAll(ctx)
	}

	b.namedMu.Lock()
	named := b.named
	b.namedMu.Unlock()
	if named != nil {
		named.closeAll(ctx)
	}
}
```

- [ ] **Step 5: Route `post:` hooks into the named container**

Replace `RunPostHook` (lines 254-266) with:

```go
// RunPostHook runs a step's post: script after the step succeeds. A scoped step
// runs its post inside the same scope container; a runsIn.container step
// (container != "") runs its post inside that named container, which is still
// alive (torn down only at claim end); every other step runs on the host
// workspace.
func (b *hostBackend) RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, env []string) error {
	if sm, h, ok := unwrapHostScope(scope); ok {
		_, err := sm.exec(ctx, h, script, env, nil, nil)
		return err
	}
	if container != "" {
		def, err := namedContainerDef(b.podTemplate, container)
		if err != nil {
			return err
		}
		nm, err := b.namedContainers()
		if err != nil {
			return err
		}
		h, err := nm.ensure(ctx, def) // returns the container already opened by the step
		if err != nil {
			return err
		}
		_, err = nm.exec(ctx, h, script, env, nil, nil)
		return err
	}
	_, _, err := RunStepCapture(ctx, script, nil, env, b.workDir)
	return err
}
```

- [ ] **Step 6: Extend `agentOSForStep` for runsIn.container**

In `internal/agent/agent_os.go`, update the condition (line 15):

```go
	if step.ScopeID != "" || (step.RunsIn != nil && (step.RunsIn.Image != "" || step.RunsIn.Container != "")) {
		return "linux"
	}
```

Also update the doc comment above it so it says a uses-scope step, a `runsIn.image` step, **or a `runsIn.container` step** executes in a Linux container and reports "linux".

- [ ] **Step 7: Run the new tests**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -run 'RunNamedContainer|AgentOSForStep_RunsInContainer|NoPodTemplate' -race -v`
Expected: PASS.

- [ ] **Step 8: Run the full agent package**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ 2>&1 | tail -5`
Expected: PASS (no regressions; the old hard-error test is gone, replaced by the no-podTemplate test).

- [ ] **Step 9: Commit**

```bash
git add internal/agent/backend_host.go internal/agent/agent_os.go internal/agent/agent_runsin_test.go
git commit -m "$(cat <<'EOF'
feat(agent): run runsIn.container on the host agent

Implement hostBackend.RunNamedContainer: exec the step into a long-lived,
workspace-bind-mounted container from the job's podTemplate. Tear down at
claim end, run post: in the same container, and report UNIFIED_AGENT_OS=linux.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Docker-gated integration test + docs

Prove end-to-end workspace sharing against a real runtime, and document that `runsIn.container` now works on the host.

**Files:**
- Create: `internal/agent/named_container_integration_test.go`
- Modify: docs — `docs/jobs.md` and/or `docs/resources.md` and `docs/kubernetes-integration.md` (whichever currently documents `runsIn.container` / podTemplate; confirm with grep)
- Test: the new integration test (build-tag / runtime-detect gated)

**Interfaces:**
- Consumes: the full feature (Tasks 1-5).
- Produces: no new exported API.

- [ ] **Step 1: Find the existing runsIn.container / podTemplate docs and the scope integration-test gating pattern**

Run:
```bash
cd unified-cd-hostcontainer-spec
grep -rln 'runsIn.container\|runsIn:\|podTemplate' docs/
sed -n '1,40p' internal/agent/agent_scope_integration_test.go
```
Expected: the doc file(s) mentioning `runsIn.container`, and the gating idiom used by the existing scope integration test (how it skips when no runtime is available — reuse that exact idiom, e.g. `runtime.Detect("")` erroring → `t.Skip`).

- [ ] **Step 2: Write the docker-gated integration test**

Create `internal/agent/named_container_integration_test.go`, mirroring the skip-gating from `agent_scope_integration_test.go`. It must: build a claim with a `podTemplate` defining a container (image `alpine`, or the image that test already uses) and two steps — step 0 `runsIn.container: tools` writes a file under the workspace, step 1 (plain host step) reads it — then assert step 1 sees the file (proving the bind mount shares the workspace). Also assert the container step observes `UNIFIED_AGENT_OS=linux`. Gate the whole test with the same runtime-detect skip the scope integration test uses.

Use this shape (adapt names/imports to the scope integration test's conventions):

```go
//go:build integration
// +build integration

package agent

// TestHostRunsInContainer_SharesWorkspace runs a real container via the
// detected runtime: a runsIn.container step writes into the workspace and a
// following host step reads it back, proving the bind mount shares the tree.
// Skips when no container runtime is available.
```

Fill the body following the existing integration test's server/claim scaffolding. Assert: run finishes Succeeded; the marker written by the container step is readable by the host step; the container step's captured stdout for `echo "$UNIFIED_AGENT_OS"` equals `linux`.

- [ ] **Step 3: Run the integration test (gated)**

Run: `cd unified-cd-hostcontainer-spec && go test ./internal/agent/ -tags integration -run TestHostRunsInContainer_SharesWorkspace -v`
Expected: PASS if a container runtime (docker/podman) is installed; SKIP otherwise. If it SKIPs on the dev machine, note that in the task report — the assertion logic is still reviewed statically.

- [ ] **Step 4: Update the docs**

In the doc file(s) found in Step 1, update the `runsIn.container` description to state that it now works on the **host agent** as well as k8s: the named container is taken from the job's `podTemplate.spec.containers`, the host bind-mounts the job workspace into it (so `cache`/artifacts/`outputs` are shared with surrounding steps), and it reports `UNIFIED_AGENT_OS=linux`. Document the MVP limits explicitly: single-container only (no sidecar/`localhost` networking), and host-unsupported podTemplate fields (PVC workspace, extra pod-spec, `command`, non-literal env) are ignored with a WARN. Keep terminology consistent with the repo's binary-naming rule (this project / controller = `unified-cd`; the CLI = `unified-cli`).

- [ ] **Step 5: Verify docs mention nothing stale**

Run: `cd unified-cd-hostcontainer-spec && grep -rn 'not supported on the host\|k8s agent only\|requires the k8s agent' docs/`
Expected: no remaining claim that `runsIn.container` is k8s-only. Fix any that remain.

- [ ] **Step 6: Full build + vet + test sweep**

Run:
```bash
cd unified-cd-hostcontainer-spec
go build ./... && go vet ./... && go test ./... 2>&1 | tail -20
```
Expected: build clean, vet clean, all non-integration tests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/named_container_integration_test.go docs/
git commit -m "$(cat <<'EOF'
test+docs: host runsIn.container workspace-sharing integration + docs

Docker-gated integration test proving a runsIn.container step and a
following host step share the workspace and UNIFIED_AGENT_OS=linux.
Document host support and its single-container MVP limits.

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
)"
```

---

## Self-Review

**1. Spec coverage:**
- Guard relaxation (spec component 0) → Task 4. ✅
- `CreateSpec` bind mount (component 1) → Task 1. ✅
- `namedContainerManager` (component 2) → Task 3. ✅
- podTemplate extraction + WARN (component 3) → Task 2. ✅
- `RunNamedContainer` + threading podTemplate (component 4) → Tasks 4 (threading) + 5 (impl). ✅
- Post hooks in named container (component 5) → Task 5, Step 5. ✅
- `agentOSForStep` linux (component 6) → Task 5, Step 6. ✅
- Cache/artifact/outputs no change (component 7) → covered by the bind mount; asserted by Task 6 integration. ✅
- Workspace & mount path (`/workspace` or `podTemplate.workspace.mountPath`) → `hostNamedMountPath`, Task 5 Step 3. ✅
- Error handling table (runtime absent, no podTemplate, unknown name, start failure, unsupported→WARN, teardown best-effort, parallel same-name mutex) → Tasks 2/3/5. ✅
- Testing (runtime argv, manager, def, RunNamedContainer, integration) → Tasks 1/2/3/5/6. ✅
- Out-of-scope (sidecar networking, eager start) → not implemented (YAGNI). ✅

**2. Placeholder scan:** No TBD/TODO; every code step shows complete code. Task 6's integration-test body is described rather than fully transcribed because it must mirror the repo's existing integration-test scaffold (discovered in Step 1) — the assertions it must make are enumerated explicitly.

**3. Type consistency:** `containerDef{Name, Image, Env, CPULimit, MemLimit}`, `namedContainerDef(pt, name)`, `newNamedContainerManager(rt, workDir, mountPath)`, `ensure(ctx, def)`, `newHostBackend(a, runID, workDir, podTemplate)`, `runtime.Mount{HostPath, ContainerPath}`, `CreateSpec.Mounts` — all consistent across tasks. `limitStrings` shared by Tasks 2 and reused in `hostContainerLimits`. `hostNamedMountPath` and `namedContainers()` defined and used in Task 5.
