# Agent-owned named volumes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give agents named, declared volumes so a job's persistent state is explicit and identical on host agents and Kubernetes agents, and make the undeclared workspace ephemeral everywhere.

**Architecture:** Volumes are declared in the agent's own config and advertised to the controller like a capability. A job names a volume (or a candidate set) and a mount path; the controller expands the names at queue time and routes the run only to an agent that advertises a free candidate; the agent owns exclusion, mounting, and lifecycle. The controller stores no volume state beyond what each agent tells it.

**Tech Stack:** Go, PostgreSQL (pgx), Kubernetes client-go, YAML DSL.

**Spec:** `docs/superpowers/specs/2026-10-20-agent-named-volumes-design.md`

## Global Constraints

- Everything written in this repository is in English: code, comments, commit messages, docs, YAML descriptions, log and error strings.
- Every commit is made from a dedicated git worktree, never from the main working tree.
- `docs/reference/field-reference.md` and `schemas/unified-cd.schema.json` are generated. Never hand-edit; run `go generate ./...` and commit the diff.
- A DSL or config change is not done until `docs/`, `examples/`, and `templates/` all agree with the code.
- No personally identifying information in any file. Use `/path/to/unified-cd` and `your-username` as placeholders.
- `mode` values are exactly `exclusive` and `shared`.
- The `size` field is required on a k8s-agent volume and advisory (warn-only) on a host-agent volume.
- The step environment variable is `UNIFIED_VOLUME_<ALIAS>`, alias upper-cased with `-` replaced by `_`.
- The controller holds no volume state of its own: everything it knows comes from an agent's registration or its claim request.

## Review Focus

Input classes the spec implies but that no task's happy path exercises. Each has a test pinned to the task that owns the code.

1. **A volume name that no agent advertises.** The run must sit `Queued` with a schedulability reason naming the volume, never fail at apply time and never hang with no explanation. — Task 4.
2. **An absolute `mountPath` that lands inside the workspace, or a relative one containing `..`.** Both must be rejected at apply time; the second is the escape `pathguard` exists to stop. — Task 1.
3. **Two slots on one agent racing for the last free exclusive volume.** Exactly one may reserve it; the loser waits rather than mounting the same volume twice. — Task 6.
4. **An agent that dies mid-run holding a volume.** On restart the host agent must treat the volume as free and must remove the symlink it left in the host filesystem, without deleting anything it did not create. — Task 6.
5. **Two aliases that collapse to one environment variable** (`go-cache` and `go_cache` both yield `UNIFIED_VOLUME_GO_CACHE`), and an alias that collides with a reserved name. Both must be rejected at apply time rather than silently shadowing. — Task 8.

---

### Task 1: DSL types and validation for `spec.volumes`

**Files:**
- Modify: `internal/dsl/types.go` (add to `Spec`, extend `WorkspaceConfig`)
- Create: `internal/dsl/volumes.go`
- Create: `internal/dsl/volumes_test.go`
- Modify: `internal/dsl/parse.go` (call the new validator)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type VolumeMount struct { Name StringOrSlice; As string; MountPath string }` with yaml tags `name`, `as`, `mountPath`.
  - `Spec.Volumes []VolumeMount` (yaml `volumes,omitempty`). There is no `spec.workspace.volume` shorthand and no `NormalizeVolumes`: `WorkspaceConfig` lives only under `spec.podTemplate.workspace`, which `native: true` forbids, so `mountPath: "."` is the only spelling. Later tasks read `spec.Volumes` directly.
  - `func ValidateVolumes(spec Spec) error`.
  - `func VolumeAlias(v VolumeMount) string` — `v.As`, or the single literal name.

- [ ] **Step 1: Write the failing tests**

In `internal/dsl/volumes_test.go`, table-driven over YAML specs:

```go
func TestValidateVolumes(t *testing.T) {
    // accepted
    //   single literal name, relative mountPath
    //   candidate list with `as`
    //   templated name with `as`
    //   mountPath "." alone
    //   mountPath "." plus a second entry at "cache"
    //   absolute mountPath /go/pkg/mod
    // rejected, asserting on the error substring
    //   candidate list without `as`            -> "requires `as`"
    //   templated name without `as`            -> "requires `as`"
    //   mountPath ".." or "a/../.."                   -> "escapes the workspace"
    //   absolute mountPath inside the workspace       -> "is inside the workspace"
    //   two entries with the same mountPath           -> "duplicate mountPath"
    //   nested mountPaths "cache" and "cache/go"      -> "nested mountPath"
    //   two aliases colliding after upper-casing      -> "collide"
    //   volumes together with workspace.pvc           -> (removed in Task 10; skip here)
    //   volumes on spec.detached: true                -> "not supported on a detached job"
    //   volumes with podTemplate.reuse: true          -> "not supported with podTemplate.reuse"
}

```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/dsl/ -run 'Volumes' -v`
Expected: FAIL, undefined `ValidateVolumes`.

- [ ] **Step 3: Add the types and implement the validator**

Add `Volumes` to `Spec` in `internal/dsl/types.go`. Implement `ValidateVolumes` and `VolumeAlias` in `internal/dsl/volumes.go`.

Containment for a relative `mountPath` reuses the existing guard (`ContainWithinSlash`, `internal/agent/pathguard.go`) semantics — reject `..` escapes — but volumes are NOT routed through that function, because it also rejects absolute paths, which §4.6 of the spec allows. Write the containment check in `volumes.go` and say in a comment why it is separate.

`StringOrSlice` must accept both `name: foo` and `name: [a, b]`; if the repository has no such helper yet, add it in `volumes.go` with a `UnmarshalYAML`.

- [ ] **Step 4: Wire the validator into parse**

Call `ValidateVolumes` from the job validation path in `internal/dsl/parse.go`, beside the existing spec validators.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/dsl/ -run 'Volumes|Parse' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/types.go internal/dsl/volumes.go internal/dsl/volumes_test.go internal/dsl/parse.go
git commit -m "feat(dsl): add spec.volumes with validation"
```

---

### Task 2: Expand volume names at queue time and infer required volumes

**Files:**
- Modify: `internal/dsl/template.go` (beside `ExpandConcurrency`, line 217)
- Modify: `internal/dsl/capabilities.go`
- Create: `internal/dsl/volumes_expand_test.go`

**Interfaces:**
- Consumes: `VolumeMount`, `Spec.Volumes` (Task 1).
- Produces:
  - `func ExpandVolumes(vs []VolumeMount, params map[string]string) ([]VolumeMount, error)` — expands every candidate in `Name`, same error-wrapping style as `ExpandConcurrency`.
  - `func RequiredVolumes(spec Spec) [][]string` — one candidate set per entry, in declaration order, from an already-expanded spec. Empty slice when the job declares none.

- [ ] **Step 1: Write the failing tests**

```go
func TestExpandVolumes(t *testing.T) {
    // {{ .Params.LIB }} with LIB=unity-a expands to "unity-a"
    // a list expands element-wise
    // a malformed template returns an error naming volumes[0].name
}

func TestRequiredVolumes(t *testing.T) {
    // one entry, single name  -> [["unity-main"]]
    // one entry, three names  -> [["go-cache-1","go-cache-2","go-cache-3"]]
    // no volumes -> empty
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/dsl/ -run 'ExpandVolumes|RequiredVolumes' -v`
Expected: FAIL.

- [ ] **Step 3: Implement `ExpandVolumes` and `RequiredVolumes`**

`ExpandVolumes` follows `ExpandConcurrency`'s shape: copy the struct, expand each string through `ExpandTemplate` with `TemplateData{Params: params}`, wrap errors as `volumes[%d].name: %w`.

Leave `RequiredCaps` unchanged — volumes are a second, separate dimension, not a capability string.

- [ ] **Step 4: Locate, but do not yet wire, the call site**

This task ships the two functions and their tests only. The call site that expands volumes and persists the result needs `runs.required_volumes`, which Task 3's migration creates — wiring it here would write to a column that does not exist yet.

Find the place that already calls `ExpandConcurrency` with a run's params (the `tryQueueRun` path in `internal/store/postgres.go`) and record the file and line in your report, so Task 3 can wire it without searching. Change nothing there.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/dsl/ -run 'Volume|Concurrency' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/template.go internal/dsl/capabilities.go internal/dsl/volumes_expand_test.go
git commit -m "feat(dsl): expand volume names at queue time"
```

---

### Task 3: Store — advertise agent volumes and filter claims by them

**Files:**
- Create: `internal/store/migrations/022_agent_volumes.up.sql`
- Create: `internal/store/migrations/022_agent_volumes.down.sql`
- Modify: `internal/store/postgres.go` (`UpsertAgent` ~1472, `ClaimNextRun` ~789-810)
- Create: `internal/store/volumes_claim_test.go`

**Interfaces:**
- Consumes: `RequiredVolumes` (Task 2).
- Produces:
  - `UpsertAgent(..., volumes []string, ...)` — one extra parameter after `capabilities`.
  - `ClaimNextRun(..., busyVolumes []string)` — one extra parameter.
  - Column `agents.volumes text[] NOT NULL DEFAULT '{}'`.
  - Column `runs.required_volumes jsonb NOT NULL DEFAULT '[]'` — an array of candidate-name arrays.

- [ ] **Step 1: Write the failing tests**

In `internal/store/volumes_claim_test.go` (integration, same harness as the existing claim tests):

```go
func TestClaimNextRun_VolumeRouting(t *testing.T) {
    // agent advertises {a}; run requires [["a"]]        -> claimed
    // agent advertises {a}; run requires [["b"]]        -> not claimed
    // agent advertises {a}; run requires [["a","b"]]    -> claimed (any-of)
    // agent advertises {a,b}; run requires [["a"],["b"]] -> claimed (all entries)
    // agent advertises {a}; run requires [["a"],["b"]]  -> not claimed
    // agent advertises {a,b}; busy {a}; run requires [["a"]]      -> not claimed
    // agent advertises {a,b}; busy {a}; run requires [["a","b"]]  -> claimed
    // run requires []; agent advertises {} -> claimed (unchanged behaviour)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run TestClaimNextRun_VolumeRouting -v`
Expected: FAIL (columns and parameters do not exist).

- [ ] **Step 3: Write migration 022**

`agents.volumes text[] NOT NULL DEFAULT '{}'` and `runs.required_volumes jsonb NOT NULL DEFAULT '[]'`. The down migration drops both.

- [ ] **Step 4: Wire the queue-time expansion**

Task 2 left this deliberately unwired because the column did not exist. At the `ExpandConcurrency` call site Task 2's report names (`internal/store/postgres.go:630`, inside `tryQueueRun`), call `dsl.ExpandVolumes` with the same params and persist `dsl.RequiredVolumes` of the expanded spec into `runs.required_volumes`. Reuse the error handling already there: roll back, append a system log, mark the run `Failed`, return `(false, nil)`.

Treat a name that expands to the empty string — from an unset param, say — as an expansion error and fail the run the same way. `ExpandVolumes` deliberately does not judge its own output, mirroring `ExpandConcurrency`. An empty name matches no agent's advertised volume, so without this check the run would sit `Queued` forever with a reason naming no volume.

Add to the test list in step 1:

```go
func TestTryQueueRun_EmptyVolumeNameFailsTheRun(t *testing.T) {
    // a volume name of "{{ .Params.MISSING }}" marks the run Failed with a
    // reason naming spec.volumes, not left Queued
}

func TestTryQueueRun_NoVolumesPersistsAnEmptyArray(t *testing.T) {
    // runs.required_volumes reads back as [] and never JSON null:
    // dsl.RequiredVolumes returns a nil slice for a job with no volumes,
    // and the column is NOT NULL DEFAULT '[]'
}
```

- [ ] **Step 5: Extend `UpsertAgent` and `ClaimNextRun`**

`UpsertAgent` stores `volumes` the way it stores `labels` (a `text[]`), not the way it stores `capabilities` (nullable for legacy agents) — an agent that declares none sends an empty array.

`ClaimNextRun` ANDs one more clause beside the existing `r.required_caps <@ a.capabilities`. The available set is the agent's volumes minus the busy list; every candidate set must overlap it:

```sql
AND NOT EXISTS (
  SELECT 1 FROM jsonb_array_elements(r.required_volumes) AS entry
  WHERE NOT (
    ARRAY(SELECT jsonb_array_elements_text(entry))
    && ARRAY(SELECT unnest(a.volumes) EXCEPT SELECT unnest($busy::text[]))
  )
)
```

Put a comment above it pointing at the schedulability query (~line 1697), which is deliberately label-only and must NOT gain this clause either, for the reason stated there.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'Claim' -v`
Expected: PASS, existing claim tests included.

- [ ] **Step 7: Commit**

```bash
git add internal/store/migrations/022_agent_volumes.*.sql internal/store/postgres.go internal/store/volumes_claim_test.go
git commit -m "feat(store): route runs to agents that advertise a free volume"
```

---

### Task 4: Controller — persist required volumes and explain an unschedulable run

**Files:**
- Modify: `internal/controller/api_runs.go:79-90,167`
- Modify: `internal/controller/api_webhooks.go:282-290`
- Modify: `internal/controller/scheduler.go:151,192`
- Modify: `internal/controller/schedulability.go`
- Create: `internal/controller/schedulability_volumes_test.go`

**Interfaces:**
- Consumes: `RequiredVolumes` (Task 2), the store columns (Task 3).
- Produces: `Schedulability.RequiredVolumes [][]string` and a `Reason` string for the unsatisfiable case.

- [ ] **Step 1: Write the failing test**

```go
func TestSchedulability_UnknownVolume(t *testing.T) {
    // no agent advertises "unity-main"
    // Satisfiable == false
    // Reason contains "unity-main" and the words "no agent declares"
    // a candidate set is satisfiable when ANY candidate is advertised
}
```

This is Review Focus item 1: the failure must be visible, not silent.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestSchedulability_UnknownVolume -v`
Expected: FAIL.

- [ ] **Step 3: Thread required volumes through run creation and report them**

`Schedulability` gains the field and the reason. It does NOT persist `required_volumes`: Task 3 already writes that column in `tryQueueRun`, and queue time is the only place where the params a name expands from exist. Every site that computes `dsl.RequiredCaps(spec)` computes `dsl.RequiredVolumes(spec)` here only to report it, not to store it. Note in a comment that an unknown volume is deliberately not an apply-time error: the controller holds no volume state (spec §4.2), so the only honest answer is an unschedulable run with a reason.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'Schedulab|Runs' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/ && git commit -m "feat(controller): report an unschedulable run's missing volume"
```

---

### Task 5: Agent configuration and the volume manager

**Files:**
- Modify: `internal/config/agent.go` (the `AgentFile` struct, ~line 81-129)
- Create: `internal/agent/volumes.go`
- Create: `internal/agent/volumes_test.go`
- Modify: `cmd/unified-cd-agent/main.go` (pass the parsed volumes into `agent.Agent`)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `type VolumeConfig struct { Name string; Mode string; Path string; Size string; StorageClassName string }` in `internal/config/agent.go`, yaml `volumes`.
  - `type VolumeManager struct { ... }` in `internal/agent/volumes.go` with:
    - `func NewVolumeManager(base string, cfgs []config.VolumeConfig) (*VolumeManager, error)` — validates and resolves `Path` (default `<workspaceDir>/volumes/<name>`), creates directories.
    - `func (m *VolumeManager) Names() []string` — what the agent advertises.
    - `func (m *VolumeManager) Reserve(runID string, names []string) (string, func(), error)` — picks a free candidate atomically and returns the chosen name with its release func. The manager knows each volume's mode from its own config; callers do not pass it. It is **re-entrant per run**: if `runID` already holds a candidate from `names`, that same name comes back with a no-op release. One job may name the same volume in several entries (mounting one volume at two paths is legal — Task 1 rejects duplicate mount paths, not duplicate names), and a per-entry reservation would otherwise have the second entry block on a volume the same run already holds, deadlocking the run against itself.
    - `func (m *VolumeManager) ReservePath(hostPath string) (func(), error)` — the same reservation for an absolute native mount path, which is a machine-wide resource (spec §4.6).
    - `func (m *VolumeManager) Busy() []string` — the claim-time report: occupied exclusive volume names plus occupied absolute mount paths.
    - `func (m *VolumeManager) ModeOf(name string) string`.
    - `func (m *VolumeManager) PathOf(name string) string`.

- [ ] **Step 1: Write the failing tests**

```go
func TestVolumeManager_CreatesDirectories(t *testing.T)
func TestVolumeManager_DefaultPath(t *testing.T)      // <base>/volumes/<name>
func TestVolumeManager_RejectsUnknownMode(t *testing.T)
func TestVolumeManager_RejectsDuplicateName(t *testing.T)
func TestVolumeManager_RejectsPathUnderAWorkingDir(t *testing.T)
    // a resolved path that is, or sits under, "<base>/working<N>" is refused:
    // gcWorkspaces globs wsBase/working* and would delete it
func TestGcWorkspaces_CannotReachAVolume(t *testing.T)
    // in internal/agent/workspace_gc_test.go: a volume at <base>/volumes/x,
    // older than the retention, survives the sweep
func TestVolumeManager_UsageMeasurementToleratesPermissionErrors(t *testing.T)
    // a root-owned subtree an isolated run left behind: warn, do not fail
func TestVolumeManager_SharedAllowsConcurrentReserve(t *testing.T)
func TestVolumeManager_ExclusiveBlocksSecondReserve(t *testing.T)
func TestVolumeManager_ReservePicksFreeCandidate(t *testing.T) // [a,b], a busy -> b
func TestVolumeManager_BusyListsExclusiveNamesAndAbsolutePaths(t *testing.T)
func TestVolumeManager_ReservePathBlocksSecondHolder(t *testing.T)
func TestVolumeManager_ReserveIsReentrantForTheSameRun(t *testing.T)
    // run r reserves exclusive "a", then reserves ["a","b"] again: it gets "a"
    // back, not "b" and not a block. Releasing both leaves "a" free.
func TestVolumeManager_ReserveStillBlocksADifferentRun(t *testing.T)
    // run r holds "a"; run s asking for ["a"] blocks
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/ -run TestVolumeManager -v`
Expected: FAIL.

- [ ] **Step 3: Implement `VolumeConfig` and `VolumeManager`**

`volumes` is file-only; it gets no flag and no env var, unlike the scalar settings around it. Say so in the struct comment.

`Reserve` holds one mutex over the whole candidate scan and the mark, so two slots cannot both pick the same free name — this is the atomicity the spec requires in §4.7.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agent/ ./internal/config/ -run 'Volume|Agent' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/agent.go internal/agent/volumes.go internal/agent/volumes_test.go cmd/unified-cd-agent/main.go
git commit -m "feat(agent): declare volumes in the agent config"
```

---

### Task 6: Host agent — advertise, reserve, mount, tear down

**Files:**
- Modify: `internal/agent/agent.go` (`runLoop` ~404-500, registration/heartbeat)
- Modify: `internal/agent/client.go` (`claim` ~200-211 — add a request body)
- Modify: `internal/api` (the `ClaimRequest` type carrying `busyVolumes`)
- Modify: the controller's claim HTTP handler — reads `busyVolumes` from the body and passes it to `ClaimNextRun`, which Task 3 left unwired
- Create: `internal/agent/volumes_mount.go`
- Create: `internal/agent/volumes_mount_test.go`

**Interfaces:**
- Consumes: `VolumeManager` (Task 5), `ClaimNextRun(..., busyVolumes)` (Task 3), `Spec.Volumes` (Task 1).
- Produces:
  - `func mountVolumes(workDir string, mounts []dsl.VolumeMount, m *VolumeManager, native bool) (resolved map[string]string, cleanup func() error, err error)` — `resolved` maps alias to absolute path, for Task 8.
  - `func sweepStaleVolumeLinks(m *VolumeManager) error` — called at startup.

- [ ] **Step 1: Write the failing tests**

```go
func TestMountVolumes_RelativeNative_Symlink(t *testing.T)
func TestMountVolumes_AbsoluteNative_Symlink(t *testing.T)
func TestMountVolumes_RefusesToClobberExistingPath(t *testing.T)
    // target exists and is not a link this agent made -> error naming the path,
    // and the existing content is still there afterwards
func TestMountVolumes_CleanupRemovesLinks(t *testing.T)
func TestSweepStaleVolumeLinks_RemovesOursLeavesTheirs(t *testing.T)
    // Review Focus 4: a link we created is swept; an unrelated symlink is not
func TestRunLoop_SecondSlotWaitsForExclusiveVolume(t *testing.T)
    // Review Focus 3: two slots, one exclusive volume; the loser does not mount
func TestMountVolumes_RejectsSharedAtWorkspaceRoot(t *testing.T)
    // the volume's agent-side mode is shared and mountPath is "." -> error.
    // This cannot be an apply-time rule: the job never sees the mode.
func TestMountVolumes_AbsoluteNativePathIsReservedMachineWide(t *testing.T)
    // two native runs wanting the same absolute path with different volumes:
    // the second waits; it does not repoint the link
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/ -run 'MountVolumes|SweepStale|SecondSlot' -v`
Expected: FAIL.

- [ ] **Step 3: Implement mounting and teardown**

Native uses symlinks for every mount path, relative or absolute. Isolated passes the volume path to the container backend as an extra mount at `mountPath` — follow how the workspace mount itself is built in `internal/agent/claim_pod.go`.

A link this agent created is identified by its target resolving inside the volume root; nothing else is ever removed.

Re-check here what apply time could not. Task 1's "absolute `mountPath` is inside the workspace" rule was evaluated against the *container* workspace root, because a host agent's workspace path is not knowable at apply time. On a native run, re-check the absolute path against the actual workspace directory and fail the run if it falls inside.

On Windows, `os.Symlink` needs privilege. Fall back to a directory junction, and if neither works, fail the run at mount time with an error that names the limitation.

- [ ] **Step 4: Advertise volumes and report busy ones**

Both halves of the wire format land in this task so they cannot drift: the agent client sends the body, and the controller's claim handler reads it and passes it to `ClaimNextRun` (Task 3 changed the store function and deliberately left the handler alone).

Registration sends `m.Names()`. The claim request gains a body carrying `m.Busy()` — today `claim` posts a nil body (`internal/agent/client.go:209`), so this is the first one. After a successful claim, `runLoop` calls `m.Reserve(runID, ...)` for each entry, and `m.ReservePath(...)` for each absolute mount path on a native run; a lost race waits for release rather than returning the run, because no endpoint exists to return it (spec §4.7). Two entries naming the same volume get the same reservation back (Task 5), and the volume is mounted at each of their mount paths.

**The wait must be cancellable.** Task 5's `Reserve` takes no context, so as written a run waiting for an exclusive volume cannot be abandoned — and the agent's drain path (`--drain-timeout`, SIGTERM) would block on it, holding a slot until the holder finishes, which for a Unity build is tens of minutes. Add a context-aware entry point and wait on the run context, so cancellation and drain both break the wait. A run whose wait is cancelled reports the cancellation; it does not proceed unmounted.

```go
func TestRunLoop_VolumeWaitIsAbandonedOnDrain(t *testing.T)
    // a run waiting for a busy exclusive volume stops waiting when the agent
    // drains, rather than holding its slot until the holder finishes
```

**Release at run end, not per entry.** A re-entrant `Reserve` returns a no-op release (Task 5): only the call that actually took the volume frees it. Release every reservation once, when the run finishes, rather than as each entry is unmounted — otherwise a volume named by two entries is freed by the first unmount while the run is still using it.

**Three things to settle in Task 5's code before you build on it:**

1. **Make `VolumeManager`'s methods nil-safe.** `Agent.Volumes` is documented as "nil means the agent declares none", but every method dereferences the receiver's fields, so `Names()`, `Busy()` and `Reserve()` panic on a nil manager — and the existing tests construct `Agent{}` without one. You are the first caller on the claim and advertise path. Return empty results on a nil receiver; `Reserve` on a nil manager with a non-empty candidate set returns an error.
2. **Normalise `ReservePath`'s key to a POSIX spelling.** It accepts a leading `/` but keys on `filepath.Clean`, which turns `/go/cache` into `\go\cache` on Windows. `Busy()` then reports a spelling that cannot match the DSL's POSIX mount path, and the controller compares those strings. Use `path.Clean(filepath.ToSlash(...))` for the key and the report, and fix `volumes_test.go:824`, which currently asserts the native spelling.
3. **Widen the GC-reach refusal.** It covers `working*`, but the agent also removes `<base>/detached/<runID>` after a run (`internal/agent/workspace.go:78`, `agent.go:457-462`) and keeps the `ucd-sh` shim in `<base>/.ucd-tools`. A volume path placed in either would be destroyed or would collide. Refuse a `detached` or dot-prefixed first component too.

Two comments Task 3 left for you to correct: the `nil` placeholders in `internal/controller/api_agent.go` (register ~63, claim ~148) are the wiring you are adding, and `ClaimNextRunDetached`'s doc comment (`internal/store/postgres.go` ~851) describes behaviour for detached runs requiring volumes, which cannot happen — a detached job's `spec.volumes` is rejected at apply time, so the clause is vacuous there. Reword it rather than leaving a future reader thinking busy tracking is missing. Call `sweepStaleVolumeLinks` once during startup, before the first claim loop starts; every volume is free at that point because the agent has lost all its runs (spec §7).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/agent/ -run 'Volume|Claim|RunLoop' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/ internal/api/ && git commit -m "feat(agent): mount named volumes and gate claims on them"
```

---

### Task 7: The default workspace becomes ephemeral

**Files:**
- Modify: `internal/agent/workspace.go` (`prepareWorkspace` ~92, `claimWorkDir` ~66)
- Modify: `internal/agent/agent.go:497` (the `clean` argument)
- Modify: `internal/k8sagent/agent.go:292` (reused-pod workspace clearing)
- Modify: `internal/agent/workspace_test.go`

**Interfaces:**
- Consumes: `VolumeManager` (Task 5), `workspaceVolumeDir` (Task 6).
- Produces: `prepareWorkspace(ctx, workDir, mode string, volumeBacked bool, clean bool, ...)` — one extra parameter — and `finishWorkspace(...)` for the after-run sweep.

**What Task 6 left you, and it is more than a signature change.** A volume mounted at `"."` becomes the workspace itself: the run works in the volume directory and `runLoop` does not call `prepareWorkspaceFn` **at all**. So today a volume-backed workspace gets no `.ucd-mode` read or write, no mode-flip check, no clean and no sweep, and `cleanWorkspace` is not applied — `runLoop` reports a System log line saying it was skipped (Task 6's fix) and proceeds. Restructuring that call is this task's job, not just adding a parameter. Derive whether the workspace is volume-backed with `workspaceVolumeDir(resp.Volumes, a.Volumes) != ""`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPrepareWorkspace_CleansByDefault(t *testing.T)
    // a file left by a previous run is gone, with clean=false
func TestFinishWorkspace_SweepsAfterTheRun(t *testing.T)
    // the run's own output is gone once the run ends, so disk is not held
    // between runs of the same job
func TestFinishWorkspace_SweepFailureIsNotFatal(t *testing.T)
    // the sweep cannot remove the tree: warn, the run's status is unchanged
func TestFinishWorkspace_LeavesAVolumeBackedWorkspaceAlone(t *testing.T)
func TestPrepareWorkspace_VolumeBackedIsNotCleaned(t *testing.T)
    // volumeBacked=true, clean=false: the previous run's file survives
func TestPrepareWorkspace_VolumeBackedModeFlipFailsAndKeepsData(t *testing.T)
    // marker says "isolated", mode is "native", volumeBacked=true
    // -> error naming the volume and both modes; the directory is untouched
func TestPrepareWorkspace_VolumeBackedMarkerIsNotWrittenIntoTheVolume(t *testing.T)
func TestPrepareWorkspace_CleanWorkspaceWipesAVolume(t *testing.T)
    // clean=true, volumeBacked=true: contents are gone (the documented escape hatch)
func TestPrepareWorkspace_CleanWorkspaceFallsBackToTheCleanupContainer(t *testing.T)
    // a root-owned tree an isolated run left in the volume: plain RemoveAll
    // fails, containerCleanup runs, the wipe succeeds
```

The third is the 200GB-deletion path the design calls out; it must fail, not wipe.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/ -run TestPrepareWorkspace -v`
Expected: FAIL.

- [ ] **Step 3: Make cleaning unconditional and protect volumes**

For a workspace with no volume at `"."`, clean before every run regardless of `clean`, and add `finishWorkspace` to sweep it again once the run ends so disk is not held between runs of the same job. The after-run sweep is best-effort: log a warning and leave the run's status alone if it fails, because the before-run clean is what guarantees a clean start.

For a volume-backed workspace, never clean before or after unless `clean` is explicitly true, keep `.ucd-mode` in the agent's own state directory keyed by volume name, and return an error on a mode flip instead of calling `RemoveAll`. When `clean` IS true, reuse the existing `containerCleanup` fallback — an isolated run on a rootful runtime leaves root-owned files that plain `RemoveAll` cannot touch.

- [ ] **Step 4: Match the behaviour on reused k8s pods**

The reused-pod workspace clearing path must clear for the ephemeral case too, so a pooled pod does not leak one run's workspace into the next.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/agent/ ./internal/k8sagent/ -run 'Workspace|Pool' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/workspace.go internal/agent/agent.go internal/agent/workspace_test.go internal/k8sagent/agent.go
git commit -m "feat(agent): make the undeclared workspace ephemeral on every backend"
```

---

### Task 8: `UNIFIED_VOLUME_<ALIAS>` and the reserved-name guard

**Files:**
- Modify: `internal/agent/stepenv.go` (`SynthesizedStepEnv` ~127, `EnvWorkspace` ~114)
- Modify: `internal/agent/orchestrator.go:788` (extraEnv)
- Modify: `internal/k8sagent` (`imageStepEnv`)
- Modify: `internal/dsl/vars_parse.go` (`ReservedVarNames`, ~line 30-60)
- Modify: `internal/dsl/volumes.go` (alias validation from Task 1)
- Modify: `internal/agent/stepenv_test.go`

**Interfaces:**
- Consumes: `resolved map[string]string` from `mountVolumes` (Task 6), `VolumeAlias` (Task 1).
- Produces: `func VolumeEnvName(alias string) string` in `internal/dsl` — `"UNIFIED_VOLUME_" + strings.ToUpper(strings.ReplaceAll(alias, "-", "_"))`. Both the agent and the k8s-agent use it; neither builds the name inline.

- [ ] **Step 1: Write the failing tests**

```go
func TestVolumeEnvName(t *testing.T)   // go-cache -> UNIFIED_VOLUME_GO_CACHE
func TestStepEnv_ExposesVolumePaths(t *testing.T)
func TestReservedVarNames_CoversVolumePrefix(t *testing.T)
    // a Vars manifest declaring UNIFIED_VOLUME_ANYTHING is refused at apply time
func TestValidateVolumes_RejectsCollidingAliases(t *testing.T)
    // Review Focus 5: "go-cache" and "go_cache" in one spec -> error
func TestValidateVolumes_RejectsReservedAlias(t *testing.T)
    // an alias yielding UNIFIED_WORKSPACE or UNIFIED_AGENT_OS -> error
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/dsl/ ./internal/agent/ -run 'VolumeEnv|ReservedVarNames|CollidingAlias' -v`
Expected: FAIL.

- [ ] **Step 3: Implement the name, the exposure, and the reservation**

`UNIFIED_VOLUME_*` is a prefix, not a fixed name, so `ReservedVarNames` needs prefix matching rather than a new list entry, and `varsDenied` picks it up by derivation. `SynthesizedStepEnv()` documents itself as the complete set; extend its doc comment to say that volume names are generated per run and are covered by the prefix rule, so the existing `TestVarsDenied_AgreesWithApplyTimeValidation` stays honest.

This is the repository's most-repeated defect class — a hand-maintained list that must track a second source. Make the prefix rule the single source and assert it from both sides.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/dsl/ ./internal/agent/ ./internal/k8sagent/ -run 'Vars|Env|Volume' -v`
Expected: PASS, `TestVarsDenied_AgreesWithApplyTimeValidation` included.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/stepenv.go internal/agent/orchestrator.go internal/k8sagent/ internal/dsl/ && git commit -m "feat: expose volume paths as UNIFIED_VOLUME_<ALIAS>"
```

---

### Task 9: k8s-agent — PVCs, pod mounts, occupancy from live pods

**Files:**
- Create: `internal/k8sagent/volumes.go`
- Create: `internal/k8sagent/volumes_test.go`
- Modify: `internal/k8sagent/podbuilder.go` (`buildWorkspaceVolume` ~627, the mount loop ~584-624)
- Modify: `internal/k8sagent/agent.go` (startup ~99, claim ~356)

**Interfaces:**
- Consumes: `config.VolumeConfig` (Task 5), `Spec.Volumes` (Task 1).
- Produces:
  - `func EnsurePVCs(ctx, client, agentName string, cfgs []config.VolumeConfig) error` — creates one ordinary PVC per volume, named `<agentName>-<volume>`, RWO for `exclusive` and RWX for `shared`, `storageClassName` left nil when unset.
  - `func BusyVolumes(ctx, client, agentName, namespace string) ([]string, error)` — derived from live claim pods labelled with the volume they hold.

- [ ] **Step 1: Write the failing tests**

Using the fake clientset the package's other tests use:

```go
func TestEnsurePVCs_CreatesOnePerVolume(t *testing.T)
func TestEnsurePVCs_ExclusiveIsRWO_SharedIsRWX(t *testing.T)
func TestEnsurePVCs_UnsetStorageClassLeavesTheFieldNil(t *testing.T)
    // the §10 defect: an empty string means "no StorageClass" and must not be sent
func TestEnsurePVCs_RequiresSize(t *testing.T)
func TestEnsurePVCs_PrefixesWithAgentName(t *testing.T)
func TestEnsurePVCs_DoesNotDeleteAPVCForARemovedVolume(t *testing.T)
func TestBusyVolumes_FromLivePods(t *testing.T)
    // two replicas under one agent ID see the same occupancy
func TestBuildPod_MountsNamedVolumes(t *testing.T)
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/k8sagent/ -run 'EnsurePVCs|BusyVolumes|MountsNamed' -v`
Expected: FAIL.

- [ ] **Step 3: Implement PVC provisioning and pod mounting**

`EnsurePVCs` runs at startup, beside `pool.Restore`. A volume dropped from the config is logged as an orphan and left alone — never deleted (spec §6.1).

Each claim pod gets a `volumeMounts` entry per declared volume and carries a label naming the volumes it holds, which is what `BusyVolumes` reads. Occupancy comes from that pod list and not from process memory, so two k8s-agent processes under one agent ID agree.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/k8sagent/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/ && git commit -m "feat(k8s-agent): provision and mount agent-owned volumes"
```

---

### Task 10: Remove `workspace.pvc`, and make docs, examples, and templates agree

**Files:**
- Modify: `internal/dsl/types.go:544-553` (delete `WorkspacePVC`, the `PVC` field)
- Modify: `internal/k8sagent/podbuilder.go:627-666` (delete the PVC branches; `buildWorkspaceVolume` becomes emptyDir plus named volumes)
- Modify: `docs/**`, `examples/**`, `templates/**`, `README.md`
- Create: `docs/operator-manual/migrations/2026-10-20-named-volumes.md`
- Regenerate: `docs/reference/field-reference.md`, `schemas/unified-cd.schema.json`

**Interfaces:**
- Consumes: everything above.
- Produces: no `workspace.pvc` anywhere outside historical specs and plans.

- [ ] **Step 1: Write the failing tests**

```go
func TestParse_WorkspacePVCIsRejected(t *testing.T)
    // a spec with workspace.pvc fails with an error pointing at the migration guide
```

Then run the existing gates, which should fail while examples and templates still use the field:

Run: `go test ./internal/dsl/ -run 'Templates|Examples' -v`
Expected: FAIL for every file still carrying `workspace.pvc`.

- [ ] **Step 2: Delete the type and the k8s branches**

`WorkspaceConfig.MountPath` stays; `WorkspaceConfig.PVC` and `WorkspacePVC` go. The `storageClassName: ""` defect (spec §10) disappears with the ephemeral-PVC branch; confirm nothing else constructs a PVC with a non-nil empty storage class.

- [ ] **Step 3: Update examples and templates**

Every `workspace.pvc` use becomes a named volume. Check each example's README and each template's header comment for stale instructions. External template references are pinned by tag or SHA, so a breaking template change needs a new tag and updated example refs.

- [ ] **Step 4: Update the documentation**

- Reference pages: `docs/user-guide/writing-jobs/job-structure.md`, `docs/user-guide/resources/job.md`, `docs/operator-manual/agents.md`, `docs/reference/configuration.md`, `docs/operator-manual/kubernetes-integration.md`.
- Narrative pages: `docs/getting-started/quickstart.md` (walk the steps; the workspace is no longer persistent), `docs/troubleshooting/controller-and-database.md` (one symptom entry per new failure mode, each with the exact grep-able error string: unschedulable volume, clobber refusal, mode flip, RWX unavailable, Windows symlink), `docs/operator-manual/operations.md` (volume disk hygiene is a new operator responsibility).
- `README.md` feature summary.
- `AGENTS.md`: its Development section tells contributors to run `docker compose -f docker-compose.dev.yaml up -d`, and no such file exists — the repository has `docker-compose.yaml`. Fix the filename, and note that the store's integration tests need only Docker, because `NewTestPostgres` (`internal/store/testutil.go:149`) starts its own PostgreSQL container. Found while implementing this plan, unrelated to the feature, but it is a stale instruction a new contributor will hit immediately.
- The migration guide: a before/after table for both breaking changes — the workspace is no longer persistent by default, and `workspace.pvc` is gone — with the exact validation errors a user will see.

- [ ] **Step 5: Regenerate and verify**

```bash
go generate ./...
go test ./... 
grep -rn "workspace.pvc\|WorkspacePVC" docs/ examples/ templates/ README.md
grep -rn "pvc:" examples/ templates/
```

Expected: tests pass; both greps return nothing outside `docs/superpowers/`.

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat!: replace workspace.pvc with agent-owned named volumes"
```
