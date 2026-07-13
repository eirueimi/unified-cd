# Service Sidecar Log Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stream each user-defined podTemplate sidecar's own stdout/stderr into the run log store (both backends), and surface it in the run detail UI as a per-sidecar filterable entry with live/terminal status.

**Architecture:** Sidecar log lines reuse the existing `logs` pipeline keyed on a dedicated high sentinel `step_index` (no schema change): user sidecar k → `SidecarLogIndex(k) = 100000+k`, injected artifact sidecar → `ArtifactLogIndex() = 90000`. Both the agent (ships logs) and the controller (renders pseudo-steps) compute the same index from the same podTemplate container order, so no mapping is exchanged. The agent taps each sidecar container's output (host: new `ContainerRuntime.Logs`; k8s: `pods/log` `GetLogs().Stream`) into `LogPusher`s, and reports each sidecar's phase/exit-code to the controller. The controller emits sidecar pseudo-steps in the steps API; the UI renders a "Sidecars" sidebar group that filters the existing windowed log viewer by the sentinel index.

**Tech Stack:** Go (internal/dsl, internal/runtime, internal/agent, internal/k8sagent, internal/controller, internal/api), client-go (`pods/log`), Svelte (web/src/routes/RunDetail.svelte).

**Spec:** docs/superpowers/specs/2026-07-13-sidecar-logs-design.md — read it first; it is the authority on scope and semantics.

## Global Constraints

- Sentinel index scheme is EXACT: user podTemplate sidecar at declared ordinal k (0-based, over non-`job` containers in `podTemplate.spec.containers` order) → `100000 + k`; injected artifact sidecar → `90000`. Real steps use `[0,N)`; `-1` is System. These ranges must never collide.
- Both the agent and the controller derive a sidecar's index from `SidecarLogIndex(ordinal)` / `ArtifactLogIndex()` (Task 1) — never hardcode the literal elsewhere.
- Scope: only USER-declared podTemplate sidecars are streamed (every non-`job` container in `podTemplate.spec.containers`). The primary `job`, pause, and shim init containers are NOT streamed. The artifact sidecar's existing exec output is re-attributed from step 0 to `90000` (Task 4) but its idle process is not separately streamed.
- No change to the `logs` table schema. `logs.step_index` is a bare `integer NOT NULL`; sidecar log lines use sentinel indices storable today with no logs migration, and sidecar names are derived at read-time from the run's stored spec (`GetRunSpec` + `json.Unmarshal` into `dsl.Spec`), never a new logs column. (Task 5 adds a small, separate `sidecar_status` table for live phase/exit state — new state that does not exist in the spec blob; this is the only new migration and does not touch `logs`.)
- All sidecar streaming is BEST-EFFORT: a failed/dropped stream logs a warning and never fails the run; a non-zero sidecar exit is reported for display only and never fails the run. Teardown MUST cancel every stream and wait for its goroutine (no leaks).
- The repo vendors nothing (module mode); local runs may need `GOFLAGS=-mod=mod`. Do NOT commit go.mod/go.sum changes unless a task explicitly adds a dependency. English prose everywhere. Per-task gates: `go build ./...`, `go vet ./...`, named test packages `-count=1`; frontend task runs the web build.

---

### Task 1: Sidecar log-index helper + container enumeration (dsl)

**Files:**
- Create: `internal/dsl/sidecar_logs.go`
- Test: `internal/dsl/sidecar_logs_test.go`

**Interfaces:**
- Produces: `const SidecarLogIndexBase = 100000`; `const ArtifactLogIndex = 90000`; `func SidecarLogIndex(ordinal int) int`; `func SidecarContainerNames(pt *PodTemplate) []string` (declared-order names of non-`job` containers; nil-safe).

- [ ] **Step 1: Write the failing test**

`internal/dsl/sidecar_logs_test.go`:

```go
package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSidecarLogIndex_DistinctFromStepsAndSystem(t *testing.T) {
	assert.Equal(t, 100000, SidecarLogIndex(0))
	assert.Equal(t, 100001, SidecarLogIndex(1))
	assert.Equal(t, 90000, ArtifactLogIndex)
	// Must not collide with real step indices [0,N) or System (-1).
	assert.Greater(t, SidecarLogIndex(0), 1000)
	assert.NotEqual(t, -1, SidecarLogIndex(0))
	assert.NotEqual(t, ArtifactLogIndex, SidecarLogIndex(0))
}

func TestSidecarContainerNames_SkipsJob_KeepsOrder(t *testing.T) {
	pt := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "golang"},
		map[string]any{"name": "mysql", "image": "mysql:8"},
		map[string]any{"name": "redis", "image": "redis:7"},
	}}}
	assert.Equal(t, []string{"mysql", "redis"}, SidecarContainerNames(pt))
}

func TestSidecarContainerNames_NilAndEmpty(t *testing.T) {
	assert.Nil(t, SidecarContainerNames(nil))
	assert.Nil(t, SidecarContainerNames(&PodTemplate{}))
	// A template with only the job container has no sidecars.
	pt := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "golang"},
	}}}
	assert.Nil(t, SidecarContainerNames(pt))
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/dsl/ -run 'SidecarLog|SidecarContainer' -count=1`
Expected: compile failure (undefined `SidecarLogIndex`, `ArtifactLogIndex`, `SidecarContainerNames`).

- [ ] **Step 3: Implement `internal/dsl/sidecar_logs.go`**

```go
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/dsl/ -run 'SidecarLog|SidecarContainer' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/sidecar_logs.go internal/dsl/sidecar_logs_test.go
git commit -m "feat(dsl): sidecar log-index helper + container enumeration"
```

---

### Task 2: Runtime `Logs` streaming primitive (host runtime)

**Files:**
- Modify: `internal/runtime/runtime.go` (add `Logs` to the `ContainerRuntime` interface)
- Modify: `internal/runtime/ocicli.go` (implement `ociCLI.Logs`)
- Modify: `internal/runtime/apple.go` (implement `appleContainer.Logs`)
- Modify (add a stub method to each fake): `internal/runtime/apple_lifecycle_test.go`, `internal/runtime/lifecycle_contract_test.go`, `internal/agent/parity_host_test.go`, `internal/agent/workspace_test.go`, `internal/agent/scope_test.go`, `internal/agent/agent_scope_test.go`
- Test: `internal/runtime/ocicli_lifecycle_test.go` (or the existing `createArgs`/`Remove` argv test file — grep `func TestOCICLI` there)

**Interfaces:**
- Produces: `Logs(ctx context.Context, h ContainerHandle, stdout, stderr io.Writer) error` on `ContainerRuntime` — streams the container's stdout→`stdout` and stderr→`stderr` from container start, following until the container exits or `ctx` is cancelled. Blocking (run in a goroutine by the caller).

- [ ] **Step 1: Write the failing test**

Add to the ociCLI lifecycle test file (mirror an existing `execCommand`-swapping test — grep `execCommand` in `internal/runtime/*_test.go` for the pattern that fakes the binary). Assert the argv is `logs -f <id>` and that stdout/stderr are wired:

```go
func TestOCICLILogs_ArgvAndStreams(t *testing.T) {
	var gotArgs []string
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotArgs = append([]string{name}, args...)
		// A command that prints to stdout and exits 0, so Logs returns nil.
		return exec.CommandContext(ctx, "printf", "hello")
	}
	defer func() { execCommand = orig }()

	r := &ociCLI{bin: "docker"}
	var out, errBuf bytes.Buffer
	err := r.Logs(context.Background(), ContainerHandle{ID: "abc"}, &out, &errBuf)
	assert.NoError(t, err)
	assert.Equal(t, []string{"docker", "logs", "-f", "abc"}, gotArgs)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/runtime/ -run 'Logs' -count=1`
Expected: compile failure (`ociCLI` has no `Logs`; interface lacks it).

- [ ] **Step 3: Add `Logs` to the interface (runtime.go)**

In the `ContainerRuntime` interface, after `Remove`:

```go
	// Logs streams a running container's output — stdout to the stdout writer,
	// stderr to the stderr writer — starting from container start and following
	// until the container exits or ctx is cancelled. Blocking; callers run it in
	// a goroutine. Used by the host claim pod to ship user sidecar logs. A
	// non-nil return is an infrastructure error (the follow command failed to
	// start); a normal container exit or a ctx cancellation returns nil.
	Logs(ctx context.Context, h ContainerHandle, stdout, stderr io.Writer) error
```

- [ ] **Step 4: Implement `ociCLI.Logs` (ocicli.go)**

After `Remove`:

```go
func (r *ociCLI) Logs(ctx context.Context, h ContainerHandle, stdout, stderr io.Writer) error {
	cmd := execCommand(ctx, r.bin, "logs", "-f", h.ID)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	// `logs -f` exits 0 when the container stops; ctx cancellation kills it.
	// Neither is an error for the caller (the stream simply ended).
	if err == nil || ctx.Err() != nil {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil // container gone / logs ended non-zero — not our failure
	}
	return fmt.Errorf("%s logs -f: %w", r.bin, err)
}
```

- [ ] **Step 5: Implement `appleContainer.Logs` (apple.go)**

After `Remove` (Apple never backs a claim pod — see `Create`'s NetworkContainer rejection — so this is for interface completeness/parity; same argv shape):

```go
func (a *appleContainer) Logs(ctx context.Context, h ContainerHandle, stdout, stderr io.Writer) error {
	cmd := execCommand(ctx, "container", "logs", "-f", h.ID)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil || ctx.Err() != nil {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	return fmt.Errorf("container logs -f: %w", err)
}
```

- [ ] **Step 6: Add a `Logs` stub to every test fake**

The build breaks until every `crt.ContainerRuntime` fake implements `Logs`. Add this method (adjust receiver name/type to each fake) to each of the 6 fakes listed in Files:

```go
func (f *shellFakeRT) Logs(context.Context, crt.ContainerHandle, io.Writer, io.Writer) error {
	return nil
}
```

(For fakes in package `runtime` the type is `ContainerHandle`, not `crt.ContainerHandle`. Find each fake by building: `go build ./... 2>&1` lists every type missing `Logs`.)

- [ ] **Step 7: Run tests + build**

Run: `go build ./... && go test ./internal/runtime/ -run 'Logs' -count=1`
Expected: build clean, PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/runtime/ internal/agent/parity_host_test.go internal/agent/workspace_test.go internal/agent/scope_test.go internal/agent/agent_scope_test.go
git commit -m "feat(runtime): add Logs streaming primitive to ContainerRuntime"
```

---

### Task 3: Host claim-pod sidecar log streaming + exit codes

**Files:**
- Create: `internal/agent/sidecar_logs.go` (the streamer, backend-agnostic within the agent package)
- Modify: `internal/agent/claim_pod.go` (expose sidecar handles in order; capture exit codes on teardown)
- Modify: `internal/agent/backend_host.go` (start streams when the masker is installed; stop at teardown)
- Test: `internal/agent/sidecar_logs_test.go`; host docker-gated integration in `internal/agent/claim_pod_integration_test.go`

**Interfaces:**
- Consumes: `dsl.SidecarLogIndex` (Task 1); `runtime.ContainerRuntime.Logs` (Task 2); `NewLogPusher`/`StartAutoFlush`/`Flush` (`runner.go`); `claimPodManager.open` (`map[string]crt.ContainerHandle`), `claimPodManager.rt`.
- Produces: `sidecarLogPump` type with `Start(ctx)` and `Stop(ctx)`; `claimPodManager.SidecarHandles() []SidecarHandle` (ordered, non-`job`), `claimPodManager.ExitCode(ctx, name) (int, bool)`.

- [ ] **Step 1: Write the failing unit test**

`internal/agent/sidecar_logs_test.go` — drive the pump with a fake runtime whose `Logs` writes known lines, and a fake `Client` that records bulk appends; assert lines land at `dsl.SidecarLogIndex(0)`:

```go
package agent

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logFakeRT.Logs writes one stdout line then returns (container "exited").
type logFakeRT struct{ crtNoop }

func (logFakeRT) Logs(_ context.Context, h crt.ContainerHandle, stdout, _ io.Writer) error {
	io.WriteString(stdout, "sidecar line for "+h.ID+"\n")
	return nil
}

func TestSidecarLogPump_ShipsAtSidecarIndex(t *testing.T) {
	rec := newRecordingClient() // a test Client capturing AppendLogBulk calls; see helper below
	pump := newSidecarLogPump(logFakeRT{}, rec.client, "agent-1", "run-1", nil,
		[]SidecarHandle{{Name: "mysql", Ordinal: 0, Handle: crt.ContainerHandle{ID: "c1"}}})
	pump.Start(context.Background())
	pump.Stop(context.Background())

	lines := rec.linesForStep(dsl.SidecarLogIndex(0))
	require.NotEmpty(t, lines)
	assert.Contains(t, lines[0], "sidecar line for c1")
}
```

Add whatever minimal test scaffolding is missing (`crtNoop` embeddable no-op runtime implementing every `crt.ContainerRuntime` method incl. `Logs`; `recordingClient` — a `*Client` pointed at an `httptest.Server` that records `logs/bulk` bodies, mirroring existing agent client tests — grep `httptest.NewServer` in `internal/agent/*_test.go` for the pattern). Keep `crtNoop` in this test file.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agent/ -run 'SidecarLogPump' -count=1`
Expected: compile failure (`newSidecarLogPump`, `SidecarHandle` undefined).

- [ ] **Step 3: Implement the pump (`internal/agent/sidecar_logs.go`)**

```go
package agent

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/eirueimi/unified-cd/internal/dsl"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// SidecarHandle names one user podTemplate sidecar container to stream: its
// declared name, its 0-based ordinal among non-"job" containers (→ log index
// via dsl.SidecarLogIndex), and its runtime handle.
type SidecarHandle struct {
	Name    string
	Ordinal int
	Handle  crt.ContainerHandle
}

// sidecarLogPump streams each user sidecar container's stdout/stderr into the
// run log store under the sidecar's sentinel step index, for the run's lifetime.
// Best-effort: a stream that errors is logged and dropped; the run never fails.
type sidecarLogPump struct {
	rt       crt.ContainerRuntime
	client   *Client
	agentID  string
	runID    string
	masker   *secrets.Masker
	sidecars []SidecarHandle

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newSidecarLogPump(rt crt.ContainerRuntime, client *Client, agentID, runID string, masker *secrets.Masker, sidecars []SidecarHandle) *sidecarLogPump {
	return &sidecarLogPump{rt: rt, client: client, agentID: agentID, runID: runID, masker: masker, sidecars: sidecars}
}

// Start spawns one streaming goroutine per sidecar. Idempotent-safe to call once.
func (p *sidecarLogPump) Start(ctx context.Context) {
	streamCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	for _, sc := range p.sidecars {
		p.wg.Add(1)
		go p.stream(streamCtx, sc)
	}
}

// Stop cancels all streams and waits for their goroutines (flushing final logs).
func (p *sidecarLogPump) Stop(ctx context.Context) {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *sidecarLogPump) stream(ctx context.Context, sc SidecarHandle) {
	defer p.wg.Done()
	idx := dsl.SidecarLogIndex(sc.Ordinal)
	stdout := NewLogPusher(p.client, p.agentID, p.runID, idx, "stdout")
	stderr := NewLogPusher(p.client, p.agentID, p.runID, idx, "stderr")
	if p.masker != nil {
		stdout.SetMasker(p.masker)
		stderr.SetMasker(p.masker)
	}
	stdout.StartAutoFlush(ctx, logPusherAutoFlushEvery)
	stderr.StartAutoFlush(ctx, logPusherAutoFlushEvery)
	if err := p.rt.Logs(ctx, sc.Handle, stdout, stderr); err != nil {
		slog.Warn("sidecar log stream ended with error", "container", sc.Name, "error", err)
	}
	// Flush remainder with a live (non-cancelled) context so final lines ship.
	stdout.Flush(context.WithoutCancel(ctx))
	stderr.Flush(context.WithoutCancel(ctx))
}
```

- [ ] **Step 4: Expose sidecar handles + exit-code capture on claimPodManager (`claim_pod.go`)**

Add, after the `Exec` method. `SidecarHandles` returns the non-`job` containers in declared order (the `open` map is unordered, so reuse `claimContainerDefs`' order by re-deriving; but the manager does not retain the podTemplate). Instead, record order at Start time: add a field `sidecarOrder []string` to `claimPodManager` (populated in `Start` as each non-`job` container is created), and:

In `claimPodManager` struct, add:
```go
	sidecarOrder []string // non-"job" container names, in podTemplate declared order
```

In `Start`, inside the `for _, def := range defs` loop, after `m.open[def.Name] = h`:
```go
		if def.Name != primaryContainerName {
			m.sidecarOrder = append(m.sidecarOrder, def.Name)
		}
```

Then add:
```go
// SidecarHandles returns the live user sidecar containers (every non-"job"
// container) in podTemplate declared order, each tagged with its ordinal so the
// caller can compute its log index via dsl.SidecarLogIndex.
func (m *claimPodManager) SidecarHandles() []SidecarHandle {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SidecarHandle
	for i, name := range m.sidecarOrder {
		if h, ok := m.open[name]; ok {
			out = append(out, SidecarHandle{Name: name, Ordinal: i, Handle: h})
		}
	}
	return out
}
```

Exit codes: the host runtime has no exit-code accessor today. Add `Logs`-adjacent capture by extending the runtime — but per YAGNI, get the exit code from the sidecar container at teardown via a new tiny runtime method is heavier than needed. Instead, capture it in `closeAllLocked` by running `<bin> wait <id>` through the existing `execCommand` seam is a runtime concern. To keep this task self-contained, add exit-code reporting to Task 5 (status) via the k8s-style approach and, for host, read it lazily. **Host exit-code capture is deferred to Task 5**, which adds a `ContainerRuntime.ExitCode` method used by both backends' status reporters. For THIS task, streaming + teardown-cancel is the deliverable.

- [ ] **Step 5: Wire start/stop into the host backend (`backend_host.go`)**

The masker becomes available when the orchestrator calls `hostBackend.SetMasker` (once, after secrets fetch, before steps). Start the pump there; stop it in the backend's scope-teardown. Read `backend_host.go`'s `SetMasker` and `CloseScopes` methods and:

- Add a field `sidecarPump *sidecarLogPump` to `hostBackend`.
- At the END of `SetMasker(m)` (after `b.masker = m`), if `b.pod != nil`:
```go
	if b.pod != nil {
		b.sidecarPump = newSidecarLogPump(b.pod.rt, b.a.Client, b.a.ID, b.runID, b.masker, b.pod.SidecarHandles())
		b.sidecarPump.Start(context.Background())
	}
```
- At the START of `CloseScopes(ctx)` (the claim-teardown hook RunClaim defers), if `b.sidecarPump != nil`, `b.sidecarPump.Stop(ctx)`.

(If `SetMasker`/`CloseScopes` signatures differ, place Start at the single point the masker is set and Stop at the single claim-teardown point — grep `func (b *hostBackend) SetMasker` and `func (b *hostBackend) CloseScopes`.)

- [ ] **Step 6: Run the unit test + build**

Run: `go build ./... && go test ./internal/agent/ -run 'SidecarLogPump' -count=1`
Expected: build clean, PASS.

- [ ] **Step 7: Docker-gated integration test**

Add to `internal/agent/claim_pod_integration_test.go` (mirror the existing `TestClaimPod_Integration_*` harness — skip helper, claim build, run a step). A sidecar whose entrypoint prints a known marker to stdout then sleeps; assert the marker line reaches the run log store at `dsl.SidecarLogIndex(0)` (the integration harness ships to a recording server — reuse whatever it already asserts logs against; grep the file for how it reads shipped logs).

```go
//go:build integration

// A user podTemplate sidecar's own stdout is streamed into the run log store
// at its sentinel index (dsl.SidecarLogIndex(0)), not attributed to any step.
func TestClaimPod_Integration_SidecarLogsStreamed(t *testing.T) {
	// podTemplate sidecar image busybox with command
	//   ["sh","-c","echo SIDECAR_MARKER; sleep infinity"]
	// build/start the claim, install shim, run a trivial default step, then
	// assert a shipped log line with StepIndex == dsl.SidecarLogIndex(0)
	// contains "SIDECAR_MARKER". Follow the existing integration harness's
	// log-capture assertion pattern verbatim.
}
```

Build the shim first (as the existing integration tests require): `GOOS=linux GOARCH=$(go env GOARCH) go build -o internal/shim/embedded/ucd-sh-$(go env GOARCH) ./cmd/ucd-sh`, run `go test -tags integration ./internal/agent/ -run 'Integration_SidecarLogsStreamed' -count=1 -v`, then restore the placeholder: `git checkout -- internal/shim/embedded/`.

- [ ] **Step 8: Commit**

```bash
git add internal/agent/sidecar_logs.go internal/agent/sidecar_logs_test.go internal/agent/claim_pod.go internal/agent/backend_host.go internal/agent/claim_pod_integration_test.go
git commit -m "feat(agent): stream host podTemplate sidecar logs to the run store"
```

---

### Task 4: K8s sidecar log streaming + artifact step-0 re-point

**Files:**
- Create: `internal/k8sagent/sidecar_logs.go` (pod-log streamer + pump)
- Modify: `internal/k8sagent/agent.go` (start pump after `WaitForPodRunning`; stop on teardown)
- Modify: `internal/k8sagent/backend.go` (re-point the artifact/cache `LogPusher` from stepIndex 0 to `dsl.ArtifactLogIndex`)
- Test: `internal/k8sagent/sidecar_logs_test.go`

**Interfaces:**
- Consumes: `dsl.SidecarLogIndex`/`dsl.ArtifactLogIndex`/`dsl.SidecarContainerNames` (Task 1); `agentlib.NewLogPusher`; `PodManager.client` (`kubernetes.Interface`); the run's `c.PodTemplate` (`*dsl.PodTemplate`, available in `agent.go`'s claim loop).
- Produces: `func streamPodContainerLogs(ctx, client kubernetes.Interface, ns, pod, container string, stdout io.Writer) error` (a single container's merged log stream); a `k8sSidecarPump` mirroring Task 3's pump shape over pod containers.

- [ ] **Step 1: Write the failing unit test**

`internal/k8sagent/sidecar_logs_test.go` — use a `k8sfake.NewSimpleClientset` (client-go fake) with a pod; assert `streamPodContainerLogs` copies the fake log stream to the writer. (The client-go fake returns a canned `"fake logs"` body from `GetLogs().Stream`.)

```go
package k8sagent

import (
	"bytes"
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamPodContainerLogs_CopiesStream(t *testing.T) {
	client := k8sfake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns"},
	})
	var out bytes.Buffer
	err := streamPodContainerLogs(context.Background(), client, "ns", "p1", "mysql", &out)
	require.NoError(t, err)
	assert.NotEmpty(t, out.String()) // fake returns a canned body
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/k8sagent/ -run 'StreamPodContainerLogs' -count=1`
Expected: compile failure (`streamPodContainerLogs` undefined).

- [ ] **Step 3: Implement the streamer (`internal/k8sagent/sidecar_logs.go`)**

```go
package k8sagent

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/secrets"
)

// streamPodContainerLogs copies a single pod container's log stream (k8s merges
// stdout+stderr) to w, following until the container ends or ctx is cancelled.
// Best-effort: returns the stream-open error, if any.
func streamPodContainerLogs(ctx context.Context, client kubernetes.Interface, ns, pod, container string, w io.Writer) error {
	req := client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Follow: true})
	rc, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(w, rc)
	return nil
}

// k8sSidecarPump streams every user sidecar container's logs into the run store
// under the sidecar's sentinel index. Mirrors the host sidecarLogPump.
type k8sSidecarPump struct {
	client   kubernetes.Interface
	logs     *agentlib.Client
	ns       string
	pod      string
	agentID  string
	runID    string
	masker   *secrets.Masker
	sidecars []string // user sidecar container names, declared order

	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func (p *k8sSidecarPump) Start(ctx context.Context) {
	streamCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	for i, name := range p.sidecars {
		p.wg.Add(1)
		go p.stream(streamCtx, i, name)
	}
}

func (p *k8sSidecarPump) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()
}

func (p *k8sSidecarPump) stream(ctx context.Context, ordinal int, name string) {
	defer p.wg.Done()
	pusher := agentlib.NewLogPusher(p.logs, p.agentID, p.runID, dsl.SidecarLogIndex(ordinal), "stdout")
	if p.masker != nil {
		pusher.SetMasker(p.masker)
	}
	pusher.StartAutoFlush(ctx, 2*time.Second) // reuse agentlib's cadence; import time
	if err := streamPodContainerLogs(ctx, p.client, p.ns, p.pod, name, pusher); err != nil {
		slog.Warn("k8s sidecar log stream error", "container", name, "error", err)
	}
	pusher.Flush(context.WithoutCancel(ctx))
}
```

(Import `"time"`; `bufio` is unused above — drop it if not needed. Confirm `agentlib.Client` is the exported agent client type k8s already uses via `b.a.client` — grep `agentlib.NewLogPusher` and the client field type in `internal/k8sagent`.)

- [ ] **Step 4: Wire start/stop into agent.go**

In `internal/k8sagent/agent.go`, right after `a.pm.WaitForPodRunning(ctx, podName)` succeeds (the pod is Running, all containers started) and after the masker is available (if the k8s masker is installed on the backend later, start with `nil` masker — k8s sidecar service logs rarely carry job secrets, and `GetLogs` replays so a later start would still capture history; but prefer starting after the backend's masker if a hook exists). Enumerate sidecars from `c.PodTemplate` and start the pump:

```go
	sidecarPump := &k8sSidecarPump{
		client: a.pm.client, logs: a.client, ns: a.cfg.Namespace,
		pod: podName, agentID: a.cfg.AgentID, runID: c.RunID,
		sidecars: dsl.SidecarContainerNames(c.PodTemplate),
	}
	sidecarPump.Start(ctx)
	defer sidecarPump.Stop()
```

Place the `defer sidecarPump.Stop()` alongside the existing pod-delete defer so streams are cancelled before the pod is removed. (`a.pm.client` is unexported — add a small accessor `func (pm *PodManager) Client() kubernetes.Interface { return pm.client }` if needed, or pass `a.pm` and read inside.)

- [ ] **Step 5: Re-point the artifact sidecar off step 0 (`backend.go`)**

In `sidecarExecArgv` and `sidecarExecArgvCapturingStdout`, replace the two `agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, 0, "stderr")` calls' `0` with `dsl.ArtifactLogIndex`:

```go
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, dsl.ArtifactLogIndex, "stderr")
```

Update the two doc comments that say "on stepIndex 0" to "on the artifact sidecar's dsl.ArtifactLogIndex (its own identity, not step 0's stream)". Add the `dsl` import to `backend.go` if absent.

- [ ] **Step 6: Run tests + build**

Run: `go build ./... && go test ./internal/k8sagent/ -run 'StreamPodContainerLogs|CacheRestore|Sidecar' -count=1`
Expected: build clean, PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/k8sagent/
git commit -m "feat(k8s): stream podTemplate sidecar logs; move artifact output off step 0"
```

---

### Task 5: Sidecar status + exit-code reporting (agent → controller)

**Files:**
- Modify: `internal/api/types.go` (add `SidecarStatusRequest`)
- Modify: `internal/runtime/runtime.go` + `ocicli.go` + `apple.go` + the 6 fakes (add `ExitCode` — host exit-code source)
- Modify: `internal/agent/client.go` (add `ReportSidecarStatus`)
- Modify: `internal/controller/server.go` (route) + `internal/controller/api_agent.go` (handler) + `internal/store/postgres.go` (+ migration) OR an in-memory-on-run map — see Step 1 decision
- Modify: `internal/agent/sidecar_logs.go` + `internal/k8sagent/sidecar_logs.go` (report `running` on start, `exited N` on stream end)
- Test: `internal/controller/api_agent_sidecar_test.go`; unit tests for the client method

**Interfaces:**
- Produces: `api.SidecarStatusRequest{RunID, Name string; Index int; Phase string; ExitCode *int}`; `Client.ReportSidecarStatus`; controller `handleAgentSidecarStatus`; store `UpsertSidecarStatus`/`GetSidecarStatuses`; `ContainerRuntime.ExitCode(ctx, h) (int, error)`.

- [ ] **Step 1: Decide status storage + write the failing controller test**

Sidecar status is small, per-run, mutable. Persist it in a new `sidecar_status` table (run_id, name, index, phase, exit_code, updated_at; PK (run_id, index)) so it survives controller restarts and the UI reads it uniformly. Migration `internal/store/migrations/00X_sidecar_status.up.sql` (+ `.down.sql`):

```sql
CREATE TABLE public.sidecar_status (
    run_id     uuid    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    idx        integer NOT NULL,
    name       text    NOT NULL,
    phase      text    NOT NULL,
    exit_code  integer,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (run_id, idx)
);
```

Write the failing handler test in `internal/controller/api_agent_sidecar_test.go` (mirror `api_agent_test.go`'s bulk-log test harness): POST a `SidecarStatusRequest` to `/api/v1/agents/a1/runs/{run}/sidecars`, then assert `store.GetSidecarStatuses(run)` returns it.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run 'SidecarStatus' -count=1`
Expected: compile failure (types/handler/store methods undefined).

- [ ] **Step 3: Add the type (`internal/api/types.go`)**

```go
// SidecarStatusRequest reports one user sidecar container's phase/exit to the
// controller for display. Phase is "running" or "exited". ExitCode is set only
// when Phase == "exited".
type SidecarStatusRequest struct {
	RunID    string `json:"runId"`
	Name     string `json:"name"`
	Index    int    `json:"index"`
	Phase    string `json:"phase"`
	ExitCode *int   `json:"exitCode,omitempty"`
}
```

- [ ] **Step 4: Store methods (`postgres.go`) + register migration**

```go
func (p *Postgres) UpsertSidecarStatus(ctx context.Context, runID string, idx int, name, phase string, exitCode *int) error {
	const q = `INSERT INTO sidecar_status (run_id, idx, name, phase, exit_code, updated_at)
	           VALUES ($1,$2,$3,$4,$5, now())
	           ON CONFLICT (run_id, idx) DO UPDATE SET phase=$4, exit_code=$5, updated_at=now()`
	_, err := p.pool.Exec(ctx, q, runID, idx, name, phase, exitCode)
	return err
}

func (p *Postgres) GetSidecarStatuses(ctx context.Context, runID string) ([]api.SidecarStatusRequest, error) {
	const q = `SELECT run_id, idx, name, phase, exit_code FROM sidecar_status WHERE run_id=$1 ORDER BY idx`
	rows, err := p.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.SidecarStatusRequest
	for rows.Next() {
		var s api.SidecarStatusRequest
		if err := rows.Scan(&s.RunID, &s.Index, &s.Name, &s.Phase, &s.ExitCode); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
```

Add both to the `Store` interface (grep the interface that `GetRunSteps`/`AppendLog` are declared on) and to any in-memory/fake store used in tests.

- [ ] **Step 5: Handler + route**

Handler in `api_agent.go` (mirror `handleAgentLogBulk`):

```go
func (s *Server) handleAgentSidecarStatus(w http.ResponseWriter, r *http.Request) {
	var req api.SidecarStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.UpsertSidecarStatus(r.Context(), req.RunID, req.Index, req.Name, req.Phase, req.ExitCode); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Route in `server.go`'s `/api/v1/agents` block:
```go
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/runs/{runId}/sidecars", s.handleAgentSidecarStatus)
```

- [ ] **Step 6: Client method (`client.go`)**

```go
func (c *Client) ReportSidecarStatus(ctx context.Context, agentID string, req api.SidecarStatusRequest) error {
	path := fmt.Sprintf("/api/v1/agents/%s/runs/%s/sidecars", agentID, req.RunID)
	_, err := c.do(ctx, http.MethodPost, path, req, nil)
	return err
}
```

- [ ] **Step 7: `ContainerRuntime.ExitCode` for host exit codes**

Add to the interface + `ociCLI`/`appleContainer` + the 6 fakes:

```go
// runtime.go interface
	ExitCode(ctx context.Context, h ContainerHandle) (int, error)
```
```go
// ocicli.go
func (r *ociCLI) ExitCode(ctx context.Context, h ContainerHandle) (int, error) {
	out, err := execCommand(ctx, r.bin, "inspect", "-f", "{{.State.ExitCode}}", h.ID).Output()
	if err != nil {
		return 0, fmt.Errorf("%s inspect exitcode: %w", r.bin, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
```
(apple.go: same with `"container"`; fakes: `return 0, nil`. Add `"strconv"` import.)

- [ ] **Step 8: Report phase transitions from both pumps**

Host `sidecarLogPump.stream`: report `running` before `p.rt.Logs`, and `exited N` after it returns (read the code via `p.rt.ExitCode(ctx, sc.Handle)`), via `p.client.ReportSidecarStatus`. K8s `k8sSidecarPump.stream`: report `running` before the stream; after it ends, read the exit code from `pod.Status.ContainerStatuses[].State.Terminated.ExitCode` (fetch the pod via the client) and report `exited N`. Both best-effort (ignore report errors with a warn). Pass `agentID` and the computed `index` into the report.

- [ ] **Step 9: Run tests + build**

Run: `go build ./... && go test ./internal/controller/ ./internal/agent/ ./internal/runtime/ -run 'SidecarStatus|SidecarLogPump|ExitCode' -count=1`
Expected: build clean, PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/api/ internal/runtime/ internal/agent/ internal/k8sagent/ internal/controller/ internal/store/
git commit -m "feat: report sidecar phase/exit-code to the controller"
```

---

### Task 6: Controller sidecar pseudo-steps

**Files:**
- Modify: `internal/controller/planned_steps.go` (synthesize sidecar `StepReport`s)
- Modify: `internal/controller/api_runs.go` (`handleGetRunSteps` merges sidecar status)
- Modify: `internal/api/types.go` (allow `StepReport.Kind = "sidecar"` — no struct change; document the value)
- Test: `internal/controller/planned_steps_test.go` (or a new `_sidecar_test.go`)

**Interfaces:**
- Consumes: `dsl.SidecarContainerNames`/`dsl.SidecarLogIndex` (Task 1); `store.GetSidecarStatuses` (Task 5); `GetRunSpec`.
- Produces: sidecar `StepReport`s in the `/runs/{id}/steps` response with `Kind:"sidecar"`, `Index:` the sentinel, `Section:"sidecars"`, `Name:` the container name, `Status`/`ExitCode` from the status table.

- [ ] **Step 1: Write the failing test**

```go
func TestPlannedSteps_IncludesSidecars(t *testing.T) {
	spec := dsl.Spec{PodTemplate: &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "golang"},
		map[string]any{"name": "mysql", "image": "mysql:8"},
	}}}}
	got := plannedSteps(spec)
	var mysql *api.StepReport
	for i := range got {
		if got[i].Kind == "sidecar" && got[i].Name == "mysql" {
			mysql = &got[i]
		}
	}
	require.NotNil(t, mysql)
	assert.Equal(t, dsl.SidecarLogIndex(0), mysql.Index)
	assert.Equal(t, "sidecars", mysql.Section)
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run 'PlannedSteps_IncludesSidecars' -count=1`
Expected: FAIL (no sidecar entries).

- [ ] **Step 3: Synthesize sidecar pseudo-steps (`planned_steps.go`)**

At the end of `plannedSteps`, before `return out`, append one entry per sidecar:

```go
	for k, name := range dsl.SidecarContainerNames(spec.PodTemplate) {
		out = append(out, api.StepReport{
			Index:   dsl.SidecarLogIndex(k),
			Name:    name,
			Kind:    "sidecar",
			Section: "sidecars",
			Status:  "Running", // overlaid with real status in handleGetRunSteps
		})
	}
	return out
```

Ensure `mergedRunSteps` passes these through: its final loop already appends reported rows whose index is not in the plan, and the planned sidecar rows are in `planned`, so they survive. Confirm sidecar rows are not clobbered by the `byIndex` merge (real step reports never carry a sidecar sentinel index, so no collision).

- [ ] **Step 4: Overlay live status (`handleGetRunSteps`)**

After `steps = mergedRunSteps(steps, spec)`, fetch and overlay sidecar statuses:

```go
	if scs, scErr := s.store.GetSidecarStatuses(r.Context(), id); scErr == nil {
		byIdx := map[int]api.SidecarStatusRequest{}
		for _, sc := range scs {
			byIdx[sc.Index] = sc
		}
		for i := range steps {
			if steps[i].Kind != "sidecar" {
				continue
			}
			if sc, ok := byIdx[steps[i].Index]; ok {
				steps[i].Status = sc.Phase   // "running" / "exited"
				steps[i].ExitCode = sc.ExitCode
			}
		}
	}
```

- [ ] **Step 5: Run tests + build**

Run: `go build ./... && go test ./internal/controller/ -run 'PlannedSteps|GetRunSteps' -count=1`
Expected: build clean, PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/
git commit -m "feat(controller): expose podTemplate sidecars as pseudo-steps with status"
```

---

### Task 7: RunDetail "Sidecars" group (frontend)

**Files:**
- Modify: `web/src/routes/RunDetail.svelte`
- Test: manual (see run step) — the repo has no Svelte unit harness for this route; the corpus/build is the gate.

**Interfaces:**
- Consumes: the `/runs/{id}/steps` response now including sidecar entries (`kind:"sidecar"`, `section:"sidecars"`, `index:` sentinel, `status`, `exitCode`).

- [ ] **Step 1: Extend `stepName` to resolve sidecar indices**

The existing `stepName` (lines ~464-468) already looks up `steps` by index; sidecar entries are in `steps`, so `stepName(sidecarIndex)` already returns the sidecar name — no change needed IF sidecar rows are in `steps`. Verify by inspection; keep `idx === -1 → "System"`.

- [ ] **Step 2: Add a "Sidecars" section to `stepSections`**

In the `stepSections` reactive block (lines ~133-152), split sidecar entries out and append a third section:

```js
  $: stepSections = (() => {
    const bySection = { main: [], finally: [], sidecars: [] };
    for (const s of steps) {
      if (s.kind === "sidecar") bySection.sidecars.push(s);
      else (s.section === "finally" ? bySection.finally : bySection.main).push(s);
    }
    const group = (arr) => {
      const map = new Map();
      for (const s of arr) {
        if (!map.has(s.stageIndex)) map.set(s.stageIndex, []);
        map.get(s.stageIndex).push(s);
      }
      return [...map.entries()].sort(([a], [b]) => a - b).map(([stageIndex, stageSteps]) => ({ stageIndex, steps: stageSteps }));
    };
    const out = [{ section: "main", label: "Steps", groups: group(bySection.main) }];
    if (bySection.finally.length) out.push({ section: "finally", label: "Finally", groups: group(bySection.finally) });
    if (bySection.sidecars.length) out.push({ section: "sidecars", label: "Sidecars", groups: [{ stageIndex: 0, steps: bySection.sidecars }] });
    return out;
  })();
```

- [ ] **Step 3: Render sidecar rows with status dot + exit label**

In the sidebar template, sidecar rows reuse the existing single-step row markup (`selectStep(s.index)` filters logs). Add a status indicator: a dot colored by `s.status` (`running` → success, `exited` with `exitCode` 0 → muted, non-zero → danger) and, when `s.kind === "sidecar"`, an `exited {exitCode}` / `running` label. Match the existing `.step-row` markup and CSS classes already in the file (grep `.step-row` and the status-dot classes); do not invent new visual primitives.

- [ ] **Step 4: Verify the build**

Run: `cd web && npm run build` (or the repo's web build command — check `web/package.json` scripts).
Expected: build succeeds, no type/lint errors.

- [ ] **Step 5: Commit**

```bash
git add web/src/routes/RunDetail.svelte
git commit -m "feat(web): Sidecars group in run detail with per-sidecar log filter + status"
```

---

### Task 8: Docs

**Files:**
- Modify: `docs/jobs.md` and/or `docs/kubernetes-integration.md` (sidecar logging behavior)
- Modify: `docs/troubleshooting.md` (how to view a failing sidecar's logs)

- [ ] **Step 1: Document the behavior**

In the podTemplate/sidecar docs: user sidecar container stdout/stderr is now streamed into the run's logs and shown under a "Sidecars" group in the run detail view, with per-sidecar status (`running` / `exited N`). Note: only user-declared sidecars (non-`job`) are streamed; a non-zero sidecar exit is shown but does not fail the run; logs persist after the pod/container is torn down. Mention the artifact sidecar's output now has its own entry (no longer mixed into the first step).

- [ ] **Step 2: Troubleshooting note**

Add: "A sidecar failed to start" → open the run, select the sidecar in the Sidecars group, read its container log; the `exited N` label shows the exit code.

- [ ] **Step 3: Confirm nothing regressed; commit**

Run: `go test ./internal/dsl/ -count=1`
Expected: PASS.

```bash
git add docs/
git commit -m "docs: service sidecar log visibility"
```

## Self-Review

**Spec coverage:** sentinel index scheme → T1 (helper) used by T3/T4/T6; host streaming → T3; k8s streaming + artifact re-point → T4; status/exit code → T5; controller pseudo-steps → T6; UI Sidecars group → T7; docs → T8; scope (user sidecars only, `job`/pause/shim excluded) → T1's `SidecarContainerNames` (skips `job`; pause/shim are not in `podTemplate.spec.containers`); uncapped volume → reuses `LogPusher` unchanged; no migration for logs → sentinel indices (a migration is added only for the small `sidecar_status` table in T5, which the spec's "no schema change" refers to the LOGS table — status is new state not in the spec blob). All spec sections covered.

**Placeholder scan:** T3 Step 7 and T7 Step 3 say "mirror the existing harness/markup" pointing at concrete existing patterns (the integration test harness in the same file; the `.step-row` markup) rather than reproducing large existing scaffolding — the new assertions/values are exact. T3 Step 4 defers host exit-code capture to T5 explicitly (not a placeholder — a stated cross-task dependency). All new logic (index helper, Logs, pumps, streamer, status type/store/handler/client, pseudo-steps) shows complete code.

**Type consistency:** `SidecarHandle{Name, Ordinal int, Handle}` used in T3 pump + claimPodManager; `dsl.SidecarLogIndex(ordinal)` / `dsl.ArtifactLogIndex` used identically in T1/T3/T4/T6; `api.SidecarStatusRequest{RunID, Name, Index, Phase, ExitCode *int}` used in T5 (type, store, handler, client) and read in T6; `Kind:"sidecar"` / `Section:"sidecars"` consistent between T6 (controller) and T7 (frontend `s.kind === "sidecar"`); `ContainerRuntime.Logs(ctx,h,stdout,stderr)` and `ExitCode(ctx,h)` consistent across T2/T5 interface + impls + fakes.

**Note for the executor:** T3 Step 5 and T4 Step 4 place the pump Start/Stop at the backend masker hook / post-`WaitForPodRunning` point respectively — the exact method bodies (`hostBackend.SetMasker`/`CloseScopes`, the k8s agent run flow) were not fully transcribed in the reference dump; the implementer must read those two methods and insert the named calls at the single masker-install and single claim-teardown points. This is the only wiring that requires reading beyond the provided code.
