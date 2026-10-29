# Host-agent `securityContext` — Implementation plan

Design: `docs/superpowers/specs/2026-10-23-host-security-context-design.md`. Section references (§) point there.
Execute after the open questions (§13) are answered. Each task assumes the
recommended answer; the notes say what changes if it is not.

Workflow (AGENTS.md:15-32): one worktree and branch for the whole feature
(`git worktree add ../unified-cd-host-secctx -b host-securitycontext`), one
commit per task, TDD inside each task (write the listed tests first, see them
fail, then implement). Each task is reviewable on its own. Tasks 1-3 are
dependency-free. Task 7 (routing) is deliberately late: until it lands, no host
ever receives a securityContext job, so tasks 2-6 can merge without changing
behaviour.

Concurrency with other in-flight work:

- `internal/k8sagent/podbuilder.go` (`resolveWorkspaceConfig`) and
  `internal/dsl/condition.go` plus the orchestrator `if:` handling are being edited
  elsewhere. This plan touches neither.
- The disk-space-gating design may also add `ucd-sh` subcommands next to `statfs`.
  Whichever branch lands second must rebase `cmd/ucd-sh/main.go` and regenerate
  `internal/shim/embedded/ucd-sh-*`.

Global verification, run at the end of every task:

```
go build ./... && go vet ./... && go test ./... -short -count=1
go build -tags integration ./internal/agent/... && go build -tags k8s ./internal/k8sagent/...
```

---

## Task 1 — DSL: resolve and classify `securityContext` (pure)

**Files**

- new `internal/dsl/securitycontext.go`
  - `type EffectiveSecurity struct{ UID, GID *int64; SupplementalGroups []int64; FSGroup *int64; FSGroupChangePolicy string; RunAsNonRoot bool; CapDrop, CapAdd []string; NoNewPrivileges, ReadOnlyRootfs bool }`
  - `ResolveSecurity(podSC, containerSC map[string]any) (EffectiveSecurity, error)`, with the precedence in design §5.1
  - `podSecurityNeedsKubernetes(sc map[string]any) bool` and `containerSecurityNeedsKubernetes(sc map[string]any) bool`, implementing the decision tables in §4.1 and §4.2 as allowlists
  - `HasSecurityContext(pt *PodTemplate) bool`
  - Integer decoding that accepts `int`, `int64` and integral `float64`, and errors on anything else
- `internal/dsl/parse.go`: apply-time validation (§4.3). Integer type, `0 <= id <= 2147483647`, `fsGroupChangePolicy` in {Always, OnRootMismatch}, `supplementalGroupsPolicy` in {Merge, Strict}. Error strings name the full path, e.g. `podTemplate.spec.securityContext.runAsUser: must be an integer between 0 and 2147483647 (got "1000")`.

**Tests first**

- `internal/dsl/securitycontext_test.go`: a precedence table (container over pod for UID/GID/runAsNonRoot; supplementary groups and fsGroup are pod-only); `float64(1000)` equals `int(1000)`; `1000.5` and `"1000"` are errors; the classification table has one row per field and value in §4. Tier B rows are table-driven so that turning tier B off flips one switch.
- Parse tests in the existing `internal/dsl/parse_test.go` style: each validation error string.

**Verify**: `go test ./internal/dsl/ -run 'Security|Parse' -count=1`

**Docs**: none yet (no behaviour change).

---

## Task 2 — Runtime: user, groups (and tier-B) flags; optional image inspector

**Files**

- `internal/runtime/runtime.go`: `CreateSpec` gains `User string`, `GroupAdd []string`, and the tier-B fields `CapDrop`, `CapAdd []string` and `NoNewPrivileges`, `ReadOnlyRootfs bool`, each with a doc comment. Add an optional interface `ImageInspector{ ImageUser(ctx, image string) (string, error) }`.
- `internal/runtime/ocicli.go`: `createArgs` emits `--user`, `--group-add` (one per entry), `--cap-drop`, `--cap-add`, `--security-opt no-new-privileges` and `--read-only` before `--entrypoint` and the image. For `bin == "podman"` with `ReadOnlyRootfs`, it also emits `--read-only-tmpfs=false` (design §11 V7). `ImageUser` runs `image inspect --format '{{.Config.User}}'`; on a "no such image" result it calls `Pull` and retries once.
- `internal/runtime/apple.go`: `createArgs` and `Create` return an error when any of the new fields is set (design §5.2). It never ignores them.

**Tests first**

- `internal/runtime/ocicli_test.go` / `ocicli_lifecycle_test.go`: argv golden for each field combination; flag position relative to the image; podman-only tmpfs flag; `ImageUser` pull-then-retry, using the existing `execCommand` seam.
- `internal/runtime/apple_test.go`: each new field yields an error.

**Verify**: `go test ./internal/runtime/ -count=1`

**Docs**: none yet.

---

## Task 3 — Shim: `fsgroup`, `chown-as`, `secctx-probe`

**Files**

- new `internal/shim/ownership.go` (Linux implementation) and `ownership_other.go` (returns "unsupported" on non-Linux; the shim only ever runs in Linux containers). The walkers take an injected `fsOps{Lstat, Lchown, Chmod}` for tests.
- new `internal/shim/probe.go`: prints `uid= gid= groups=`; creates a file in the target directory and prints `file_uid= file_gid= dir_mode=`; prints `uid_map=` / `gid_map=` when they are readable.
- `cmd/ucd-sh/main.go`: three new `case`s plus the usage line (`main.go:82`).
- Regenerate `internal/shim/embedded/ucd-sh-{amd64,arm64}` and `ucd-sh-source.sha256` with `go generate ./internal/shim/embedded/`. Prefer the Linux Docker invocation in `internal/shim/embedded/embed.go:24`.

**Before coding**: read upstream kubelet `pkg/volume/volume_linux.go` (`SetVolumeOwnership`, `changeFilePermission`, `skipPermissionChange`) at the Kubernetes version of CI's kind node image. Paste its masks into the code comment with the version.

**Tests first** (`internal/shim/ownership_test.go`)

- The fake-FS walk: file mode becomes `|0660`; dir mode becomes `|0770|setgid`; a symlink is lchowned but never chmodded or followed; `OnRootMismatch` skips the walk when the root already matches and walks when it does not.
- `chown-as`: the group comes from a setgid parent, otherwise from G; setgid propagates to subdirectories.
- A Linux-only, root-only real-FS test (`t.Skip` unless `os.Geteuid()==0`) that runs the walker on a temp dir and stats the result.
- `cmd/ucd-sh` arg-validation tests next to the existing ones.
- `internal/shim/embedded/freshness_test.go` must pass after regeneration.

**Verify**: `go test ./internal/shim/... ./cmd/ucd-sh/ -count=1`

**Docs**: none (internal).

---

## Task 4 — Claim pod: apply identity, privileged pause, `fsGroup` before start

Depends on 1, 2, 3.

**Files**

- `internal/agent/claim_pod.go`
  - `containerDef` gains `Security dsl.EffectiveSecurity`. `claimContainerDefs` resolves it from `pt.Spec["securityContext"]` and each container's `securityContext`. The injected default `job` container (`claim_pod.go:221`) gets the pod-level values.
  - `parseContainerDef` stops warning for `securityContext` (`claim_pod.go:49-54`), because it is now consumed.
  - `Start` (`claim_pod.go:328-377`): pause is created with `User: "0:0"` and the same workspace, volume and `/.ucd` mounts as the job containers. Then, when `FSGroup != nil`, it execs `ucd-sh fsgroup --policy=<p> <gid> <mountPath> <each volume ContainerPath>`, copying the output to the run log through a callback. Then come the `runAsNonRoot` and image-user checks (§5.3, through `crt.ImageInspector`). Then the containers are created with `User` / `GroupAdd` (and tier B) set.
  - A new `(*claimPodManager).HelperExec(ctx, argv []string)` targets pause. It uses the existing `ExecSpec{Shell: argv[:n-1], Script: argv[n-1]}` contract (`runtime.go:78-94`), so the runtime interface stays unchanged.
  - Rootless range check (design §5.4 limit): `Start` receives the probe's max mapped ID (task 6) and fails before any `Create` when an ID exceeds it.
- `internal/agent/agent.go` (`executeRun`, `agent.go:833-879`): passes the run-log callback and the probe result into the pod manager.

**Tests first** (`internal/agent/claim_pod_test.go`, using `podFakeRT`)

- Pause `CreateSpec.User == "0:0"`, and its mounts equal the job's mounts.
- Pod-level `runAsUser: 1000, fsGroup: 2000, supplementalGroups: [3000]`: `job` and each sidecar get `User "1000"` and `GroupAdd ["3000","2000"]`. A container-level `runAsUser: 0` on one sidecar overrides only that sidecar.
- The exec of `fsgroup` into the pause handle is recorded **before** the `job` Create.
- `runAsNonRoot` with an image user of `""` (that is, root) fails `Start` with the kubelet-style message, and no job or sidecar Create happened. A non-numeric image user without `runAsUser` fails the same way.
- `runAsGroup` without `runAsUser` and image user `node`: `User == "node:<G>"`.
- No securityContext: `User == ""` and `GroupAdd` is nil (decision 1 regression guard).
- `parseContainerDef` emits no WARN for `securityContext`.

**Verify**: `go test ./internal/agent/ -run 'ClaimPod|Claim' -count=1`

**Docs**: none yet (routing still pins these jobs; nothing reaches this path in production).

---

## Task 5 — Workspace: emptyDir mode, agent-write re-own, read fallback, cleanup hardening

Depends on 4.

**Files**

- `internal/agent/workspace.go`
  - `prepareWorkspace`: for `vol == nil && mode == "isolated"`, run `os.Chmod(workDir, 0o777)` after `MkdirAll` (`workspace.go:131`). A failure is an error on Linux and a WARN on Windows. (If Q2 is answered "no", gate this on `HasSecurityContext`.)
  - `containerCleanup`: `CreateSpec.User = "0:0"` (`workspace.go:232-237`).
- `internal/agent/workspace_gc.go:98`: replace `os.RemoveAll(jobDir)` with `wipeDir(ctx, jobDir, false, rtFn, toolsDir)`, threading the runtime func and tools dir in from the GC loop's caller.
- `internal/agent/backend_host.go`
  - Non-scoped `DownloadArtifact` (`:286-290`) and `CacheRestore` (`:216-226`): on success, when the claim has an identity field, call `pod.HelperExec("ucd-sh","chown-as", "<podU or 0>:<podG or 0>", <container path of the destination>)`. A new helper maps a host path to a container path, inverting `resolveInVolumeMount` (`volumes_mount.go:695`): workspace, then volumes.
  - Non-scoped `UploadArtifact` (`:271-275`) and `CacheSave` (`:252-260`): on `errors.Is(err, fs.ErrPermission)` and `b.pod != nil`, `CopyOut` from the pause handle into a temp dir and archive that. Volume exclusions still apply, computed in container coordinates.

**Tests first**

- `internal/agent/workspace_test.go`: an isolated ephemeral workspace is 0777 after `prepareWorkspace` even with umask 022 (Linux only); native stays 0755; a volume workspace is untouched.
- `workspace_gc_test.go`: a GC removal failure goes through the cleanup-container fallback (inject `removeAll`, as the existing test at `workspace.go:79` allows).
- `backend_host_test.go`: after a download the fake runtime records a `chown-as` exec with the pod-level identity, or `0:0` when only the container sets `runAsUser`; there is no exec when the claim has no securityContext; an upload whose first attempt returns `fs.ErrPermission` retries through `CopyOut`.

**Verify**: `go test ./internal/agent/ -run 'Workspace|Backend|GC' -count=1`

**Docs** (in this task, because the 0777 change is user-visible on its own):

- `docs/operator-manual/agents.md` "Workspace lifecycle" (`:748-760`): the workspace root is 0777 like an `emptyDir`; recommend `wsBase` 0750 or stricter.
- `docs/operator-manual/operations.md` (`:180-190`): the GC now uses the same cleanup fallback.

---

## Task 6 — Startup probe and capability advertisement

Depends on 3, 4.

**Files**

- new `internal/agent/secctx_probe.go`: `probeSecurityContext(ctx, rt, wsBase, toolsDir, pauseImage) (probeResult, error)`, implementing design §7.1 steps 1-5. `probeResult{OK bool; Reason string; Rootless bool; MaxMappedID int64}`.
- `internal/agent/agent.go`: `agentCapabilities` (`:241-247`) gains a parameter, so caps are `[native, container, securitycontext]` only when the probe is OK. Run the probe once, behind a `sync.Once` next to `containerRuntime()` (`:232-237`). `logRegistration` (`:262-268`) logs the reason when the probe fails, e.g. `container runtime cannot honour securityContext (<reason>); jobs that set securityContext will not be routed to this agent`.
- `cmd/unified-cd-agent/main.go`: no flag (Q10). Make sure the probe runs after `InstallShim` (`:166`).
- `internal/dsl/capabilities.go`: `CapSecurityContext`, `ValidCapability`, and `NormalizeCapabilities(caps []string) []string` (`pod` implies `securitycontext`). This lands here so the agent can reference the constant. The controller wiring is task 7.

**Tests first**

- `secctx_probe_test.go` against a scripted fake runtime: each failed expectation (uid, gid, missing group, file group not inherited, setgid missing, root helper not uid 0) produces `OK=false` with a specific reason. A uid_map of `0 1000 1\n1 100000 65536` gives `Rootless=true, MaxMappedID=65536`; `0 0 4294967295` gives rootful and unlimited.
- `agent_capabilities_test.go`: the capability set with and without the probe.

**Verify**: `go test ./internal/agent/ -run 'Probe|Capabilit' -count=1`

**Docs**: `docs/reference/configuration.md` "Job isolation notes" (`:350-364`): the probe, what it checks, how to read its WARN, and that Apple `container` never passes it.

---

## Task 7 — Routing: stop pinning honoured fields; controller normalization; k8s-agent advertises

Depends on 1, 6. **This is the behaviour change.** Tasks 8 and 9 ship in the same release.

**Files**

- `internal/dsl/podtemplate.go`
  - Pod-level loop (`:40-46`): allow the key `securityContext` when `!podSecurityNeedsKubernetes`. Update the comment at `:42-43`.
  - `HostSupportedContainerFields` (`:13-16`) gains `"securityContext": true`. `containerNeedsKubernetes` (`:84-144`) calls `containerSecurityNeedsKubernetes`, and the whole/part statement required by `:82-83` is added.
- `internal/dsl/capabilities.go`: `RequiredCaps` returns `[container, securitycontext]` when the podTemplate is host-runnable and `HasSecurityContext`. Update the doc comment's table (`:29-35`).
- `internal/controller/api_agent.go` (`:46`, `:207-219`): apply `dsl.NormalizeCapabilities` before storing. `internal/controller/schedulability.go:35-55`: normalize each agent's caps before `capsCover`.
- `internal/k8sagent/agent.go:92`: `k8sAgentCapabilities` returns `[pod, container, securitycontext]`.
- `internal/agent/claim_pod.go`: refresh the "Unreachable for any run created after the routing predicate..." comments (`:73-82`, `:106-115`) if their wording no longer holds.

**Tests first**

- `internal/dsl/podtemplate_needs_k8s_test.go`: new rows (honoured: false; pinned values: true; mixed: true). Everything already there must stay green.
- `internal/dsl/capabilities_test.go`: `RequiredCaps` rows.
- `internal/controller/scheduler_schedule_test.go` (`:277-318` style): a securityContext job creates a run with `[container, securitycontext]`.
- `internal/store/postgres_capability_routing_test.go`: an old host agent `[native, container]` cannot claim; a legacy NULL-caps agent cannot claim; a new host agent can; an old k8s agent `[pod, container]` whose stored caps were normalized can.
- `internal/controller` registration test: `securitycontext` is accepted; normalization is stored.
- `internal/dsl`: `TestTemplatesParse` / `TestExamplesParse` stay green, and a new assertion checks that `templates/buildkit-rootless-build-push.yaml` still yields `[pod]` (design §12).

**Verify**:

```
go test ./internal/dsl/ ./internal/controller/ ./internal/k8sagent/ -count=1
go test ./internal/store/ -run Capabilit -count=1   # needs Docker (NewTestPostgres)
```

**Docs** (same task, because routing is what users see):

- `docs/operator-manual/agents.md` "Capabilities and routing" (`:285-380`): the new capability; the bullet list of what yields `pod` (remove `securityContext` from `:294` and add the pinned subset); the unknown-capability sentence at `:375` (`native`/`container`/`pod` becomes four strings); normalization of old k8s agents.
- `docs/operator-manual/kubernetes-integration.md`: the "remaining intentional differences" list (`:34-60`) moves `securityContext` from "pinned" to "honoured on both, see …" and states the pinned subset. Replace `:452-468` "(`securityContext` is Kubernetes-only, so such a job is pinned…)" with the new behaviour and the CSI `fsGroupPolicy` caveat (design §6.5).
- `docs/user-guide/writing-jobs/isolation-and-containers.md:186-189`: `securityContext` is no longer in the "already require a Kubernetes agent" list. Add a subsection "Running as a non-root user", with the pod-level recommendation (§6.4), the `OnRootMismatch` advice for large volumes (§6.5), and the observable table from design §9 in a user-facing form.
- `docs/troubleshooting/steps-and-execution.md`
  - `:383-418` "Permission denied writing to a volume on Kubernetes": retitle to cover both agents; replace `:417-418` "a Kubernetes-only field, so … pinned"; fix `:397-398` ("On a host agent … it does not arise"), which is false for rootless and root agents.
  - `:97-117` root-owned cleanup: "Run rootless podman … don't occur in the first place" is false once non-root UIDs run. Rewrite it around `fsGroup` and the root fallback.
  - New entries with exact grep-able strings: the rootless out-of-range error (task 4), the probe WARN (task 6), the `runAsNonRoot` refusal (task 4), and the old-controller `400 unknown capability: securitycontext`.
- `docs/troubleshooting/index.md:32`: link text and anchor for the retitled entry.
- `docs/reference/configuration.md:353-357`: the rootless-podman recommendation keeps its point (no root-owned leftovers from root containers) but must stop claiming it sidesteps all ownership problems.
- `docs/operator-manual/operations.md:185-188`: the same correction ("Rootless podman avoids all of it").
- `docs/user-guide/writing-jobs/volumes.md:220-224`: the cleanWorkspace fallback wording also covers files owned by other UIDs.
- `templates/buildkit-rootless-build-push.yaml:14-16`: the header comment says it is pinned because of `securityContext`/`args`. It now stays pinned because of `seccompProfile`/`appArmorProfile: Unconfined` (`args` is host-supported). Fix the wording.
- `README.md`: the feature summary line about host/k8s parity, if it lists securityContext.
- Generated: `go generate ./...`. Commit any diff in `docs/reference/field-reference.md` / `schemas/unified-cd.schema.json`. There may be none, because `Spec` is `map[string]any`. Confirm.

---

## Task 8 — Migration note

**File**: new `docs/operator-manual/migrations/2026-10-23-host-security-context.md`, in the shape of `podtemplate-subfield-routing.md`:

- A before/after table: routing (`[pod]` becomes `[container, securitycontext]`), the host workspace root 0755 becomes 0777, and the new capability string.
- "Who is affected": the SQL from design §12 (note the `PodTemplate` JSON key and how to confirm it), plus the `GET /api/v1/jobs/<name>/schedulability` check. Say explicitly that `uses:`-resolved podTemplates are not visible to the SQL.
- What can break (implicit cluster DNS, ServiceAccount tokens, NetworkPolicy), and the remedy `agentSelector: [kubernetes]`.
- What starts working (securityContext jobs in host-only fleets).
- **Upgrade order**: controller first. A new host agent against an old controller is rejected with `400 unknown capability: securitycontext` and retries. Old k8s agents keep claiming (normalization). Old host agents never receive these jobs.
- Rootless limits: the ID range error, and that rootless no longer "avoids all ownership problems".
- Queued runs keep their old routing; replays recompute.

Also link it from `docs/operator-manual/kubernetes-integration.md` and the `mkdocs.yml` nav, if migrations are listed there (`grep -n migrations mkdocs.yml`).

**Verify**: `mkdocs build --strict` (the `docs-build` target in the Makefile).

---

## Task 9 — Real-runtime parity golden and CI legs

Depends on 4-7.

**Files**

- new `internal/paritycases/securitycontext.go`: `SecurityContextCases() []Case` (design §10). Cases:
  (a) pod-level U/G/fsGroup/supplementalGroups;
  (b) container-level override on one sidecar, with the step targeting it through `container:`;
  (c) U only, with an image that has a passwd entry for U, then one that has not (V2);
  (d) artifact download ownership, pod-level versus container-level identity (§6.4);
  (e) a volume with a root-owned leftover regrouped by `fsGroup` under `Always`, and skipped under `OnRootMismatch` when the root already matches;
  (f) no securityContext: `id -u` is 0 for a root image (decision 1).
  Output lines are fixed-format, and expectations use `LogMustContain`.
- new `internal/agent/secctx_parity_integration_test.go` (`//go:build integration`). It reads `UCD_TEST_CONTAINER_RUNTIME` (defaulting to `crt.Detect("")`) and follows `claim_pod_integration_test.go:124-150`.
- new `internal/k8sagent/secctx_parity_k8s_test.go` (`//go:build k8s`). It drives the same cases on kind, using the existing `testutil_k8s_test.go` helpers.
- `.github/workflows/ci.yml`: a new job, `host-runtime-integration`, with matrix `{docker-rootful, podman-rootless}`. It runs `go test -tags integration ./internal/agent/ -run 'SecCtx|ClaimPod' -count=1 -v` with `UCD_TEST_CONTAINER_RUNTIME` set, and needs the regenerated shim. A third leg, `docker-rootless`, uses `dockerd-rootless-setuptool.sh install` and may start as `continue-on-error: true` until it has proved stable.

**Verify**: the CI run. Locally on Linux:
`UCD_TEST_CONTAINER_RUNTIME=podman go test -tags integration ./internal/agent/ -run SecCtx -v`.
This task turns design §11 V1-V5 and V7-V8 into facts. Any V that fails sends you back to the design, not to a test tweak.

**Docs**: `CONTRIBUTING.md`, if it documents how to run integration tests. Add the env var.

---

## Task 10 (tier B, drop if Q1 = "pin") — hardening flags end to end

Tasks 1 and 2 already carry the table switch and the flags. This task flips the tier-B switch in `internal/dsl/securitycontext.go` to "honour". It wires `CapDrop`, `CapAdd`, `NoNewPrivileges` and `ReadOnlyRootfs` from `EffectiveSecurity` into `CreateSpec` in `claim_pod.go`, and adds golden cases to `SecurityContextCases`: `capsh --print` or `/proc/self/status` `CapEff`, `NoNewPrivs`, and `touch /x` failing under `readOnlyRootFilesystem`.

**Tests first**: routing rows flip from pin to honour; `claim_pod_test.go` CreateSpec assertions; golden cases.

**Docs**: the field tables in `isolation-and-containers.md` and `kubernetes-integration.md` (from task 7) gain the tier-B rows; a migration-note row.

---

## Final sweep (part of the last task's review)

```
grep -rn "securityContext" docs/ examples/ templates/ README.md | grep -v "docs/superpowers/"
grep -rn -i "Kubernetes-only" docs/ | grep -i "securityContext"
grep -rn -i "rootless podman" docs/ | grep -v "docs/superpowers/"
grep -rn "native\`/\`container\`/\`pod\`" docs/
```

There must be zero stale claims that `securityContext` pins a job to Kubernetes in
general (the pinned subset must be named), and zero claims that rootless podman
removes all ownership problems. Then re-run
`go test ./internal/dsl/ -run 'Templates|Examples'`.
