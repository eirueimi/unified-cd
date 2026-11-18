# Controller-declared Agent Volumes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Declare agent named volumes on the controller as a `kind: AgentVolume` resource, deliver each agent its label-selected set through the heartbeat response, and have host and Kubernetes agents apply additions, removals and mode changes without a restart; remove `volumes:` from the agent config.

**Architecture:** The controller stores `AgentVolume` definitions (the `kind: Vars` pattern) and answers every heartbeat with the agent's desired set, computed from the authenticated principal's `AuthorizedLabels`, plus a revision. Agents diff the desired set against their live state in a now-mutable volume manager, deferring removals and mode changes until no run holds the volume, and keep advertising only what is ready, so routing (`agents.volumes`, `claimNextRun`) is unchanged. Not-ready volumes and their reasons travel back on the heartbeat request.

**Tech Stack:** Go, PostgreSQL migrations (`internal/store/migrations`), chi router, client-go (k8s-agent), yaml.v3.

**Spec:** `docs/superpowers/specs/2026-11-18-controller-declared-volumes-design.md`

## Global Constraints

- Kind name `AgentVolume`, `apiVersion: unified-cd/v1`; fields `spec.mode` (`exclusive`|`shared`, required), `spec.size` (required, k8s quantity, positive), `spec.storageClassName` (optional), `spec.agentSelector` ([]string, same label syntax as a Job's `agentSelector`, e.g. `role:unity`; empty = every agent). Unknown fields rejected.
- `metadata.name` = volume name: DNS-1123 label (which also makes it a single path segment).
- Host path is always `<workspaceDir>/volumes/<name>`; no `path` field anywhere.
- Existing directory / PVC at the volume's location is used as-is; nothing ever deletes or recreates volume data.
- Desired set is computed from `AgentPrincipal.AuthorizedLabels`, never from the request body or the `agents` row; a delivered set (including empty) is authoritative.
- Agent sends its first heartbeat immediately at startup; heartbeat interval stays 15s.
- Controller unreachable / heartbeat error: agent keeps its last known set.
- One invalid or failed volume is reported not ready; the rest of the set is applied.
- k8s: existing PVC size/storageClass/access-mode mismatch only warns; no PVC expansion, no new RBAC.
- Agent config containing `volumes:` (host or k8s) refuses to start with an error naming `docs/operator-manual/migrations/2026-11-18-controller-declared-volumes.md`.
- New agent vs old controller (heartbeat 204, no body): offer no volumes, log one warning that the controller must be upgraded. Old agent vs new controller keeps working (it ignores the body; routing uses its advertised list).
- Next migration number is `031`; add its sentinel to `internal/store/verify.go`.
- English only in the repo. Docs are updated in the task that changes the behaviour they describe.
- Commit with `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit` and trailers `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV`. Never commit `.superpowers/`. Do not push.
- The checkout uses autocrlf: verify gofmt on committed blobs (`git diff --check`, no CR in `git show HEAD:<file>`), not with `gofmt -l` on the working tree.

## Review Focus

1. An `AgentVolume` is deleted (or its selector stops matching) while a run on that agent holds it → the run finishes normally, its links are removed, then the volume disappears from the advertised list and the directory stays on disk. (Task 5 test `TestApplyRemoveWhileHeld`, Task 7 test `TestK8sApplyRemoveWhileHeld`.)
2. Mode flipped exclusive→shared while a run holds the volume → no new claim gets the volume until the holder releases; then it is offered as shared. (Task 5 `TestApplyModeChangeWaitsForHolders`.)
3. Agent restarts while the controller is down → it offers no volumes and its claims do not wipe the stored `agents.volumes` list. (Task 4 `TestClaimWithoutVolumesKnownKeepsStoredVolumes`, Task 6 `TestAgentKeepsLastSetOnHeartbeatError`.)
4. Two AgentVolumes delivered, one of which fails (k8s claim name over 63 characters, or host `MkdirAll` error) → the other is provisioned and advertised; the failed one is reported with its reason in `GET /api/v1/agents`. (Task 5 `TestApplyOneBadVolumeDoesNotBlockOthers`, Task 7 `TestK8sApplyClaimNameTooLong`, Task 4 `TestAgentsListShowsVolumeStatus`.)
5. A Job references a volume name with no `AgentVolume` → schedulability says it is not declared, distinct from "declared but no matching agent has it ready". (Task 4 `TestSchedulabilityUndeclaredVolume`.)

---

### Task 1: `AgentVolume` DSL kind

**Files:**
- Create: `internal/dsl/agentvolume_types.go`, `internal/dsl/agentvolume_parse.go`, `internal/dsl/agentvolume_test.go`
- Modify: `internal/dslschema/dslschema.go` (`rootDisplayOrder`, cosmetic), regenerated `schemas/unified-cd.schema.json`, `docs/reference/field-reference.md` (`go generate ./...` / `cmd/schemagen`, `cmd/docgen`), `internal/schemakinds/schemakinds.go` if it lists kinds.

**Interfaces:**
- Produces:
  - `type AgentVolume struct { APIVersion, Kind string; Metadata Metadata; Spec AgentVolumeSpec }` (yaml/json tags like `Vars`)
  - `type AgentVolumeSpec struct { Mode string; Size string; StorageClassName string; AgentSelector []string }` (yaml/json `mode`, `size`, `storageClassName,omitempty`, `agentSelector,omitempty`)
  - `func ParseAgentVolume(r io.Reader) (*AgentVolume, error)` — KnownFields, checks apiVersion/kind, calls Validate.
  - `func (v *AgentVolume) Validate() error`
  - `func AgentVolumeMatches(selector, agentLabels []string) bool` — every selector entry present in agentLabels; empty selector → true. Must agree with the claim query `agent_selector <@ agentLabels`.
  - Constants `VolumeModeExclusive = "exclusive"`, `VolumeModeShared = "shared"` in dsl (agent package may alias them).

- [ ] **Step 1: Write failing tests** in `agentvolume_test.go`:
  - `TestParseAgentVolume_Valid`: the spec's example (name `unity-cache`, mode `exclusive`, size `200Gi`, storageClassName `fast-ssd`, selector `[role:unity]`) parses with those values.
  - `TestParseAgentVolume_Rejects`: each returns an error containing the field name: missing mode; mode `rw`; missing size; size `0`; size `abc`; name `Unity` (not DNS-1123); name `a/b`; unknown field `spec.path: /x`; kind `Vars`.
  - `TestAgentVolumeMatches`: `(nil, [a])`→true; `([role:unity], [role:unity, os:windows])`→true; `([role:unity, os:linux], [role:unity])`→false; `([role:unity], nil)`→false.
- [ ] **Step 2:** `go test ./internal/dsl/ -run AgentVolume` → FAIL (undefined).
- [ ] **Step 3:** Implement. Size parsing with `k8s.io/apimachinery/pkg/api/resource` as the agent does today (`internal/agent/volumes.go` ~119); DNS-1123 via the helper the k8s agent uses (`internal/k8sagent/volumes.go` ~75) or `k8s.io/apimachinery/pkg/util/validation.IsDNS1123Label`.
- [ ] **Step 4:** `go generate ./...` (or the schemagen/docgen commands), then `go test ./internal/dsl/... ./internal/dslschema/... ./internal/schemakinds/... ./internal/cli/...` → PASS (`internal/cli/export_kind_completeness_test.go` may now fail until Task 3; if so, note it and let Task 3 fix it — do not skip the test).
- [ ] **Step 5:** Commit `feat(dsl): add AgentVolume kind`.

### Task 2: Store — definitions table and agent volume status

**Files:**
- Create: `internal/store/migrations/031_agent_volumes.up.sql`, `031_agent_volumes.down.sql`
- Modify: `internal/store/store.go`, `internal/store/postgres.go`, `internal/store/verify.go`, store tests next to the Vars tests.

**Interfaces:**
- Produces:
  - Table `agent_volume_defs (name text primary key, spec jsonb not null, created_at, updated_at)`; column `agents.volume_status jsonb` (nullable).
  - `type AgentVolumeRecord struct { Name string; Spec []byte; UpdatedAt time.Time }`
  - `UpsertAgentVolume(ctx, name string, specJSON []byte) error`, `ListAgentVolumes(ctx) ([]AgentVolumeRecord, error)` (sorted by name), `GetAgentVolume(ctx, name) (*AgentVolumeRecord, error)` (nil,nil when absent — match `GetAppSource`'s convention), `DeleteAgentVolume(ctx, name) error` (absent is not an error).
  - `SetAgentVolumeStatus(ctx, agentID string, st []api.AgentVolumeStatus) error` — replaces wholesale, nil clears (same contract as `SetAgentStorage`).
  - `UpsertAgentOnClaim` gains `volumesKnown bool`: when true the volumes column is replaced even with an empty list; when false today's "empty means not supplied" holds. Update every caller.
  - `api.AgentVolumeStatus { Name string; Ready bool; Reason string }` (json `name`, `ready`, `reason,omitempty`) — define in `internal/api/types.go` in this task.
  - `ListAgents` returns `VolumeStatus []api.AgentVolumeStatus` on the agent record.
- Consumes: nothing.

- [ ] **Step 1: Failing tests** (PostgreSQL-backed, same harness as Vars store tests): `TestAgentVolumeCRUD` (upsert twice → one row with new spec; list sorted; get absent → nil; delete absent → nil); `TestSetAgentVolumeStatusReplaces` (set two, set nil → empty); `TestUpsertAgentOnClaimVolumesKnown` (stored `[a]`; claim with `[]`, known=false → still `[a]`; known=true → `[]`).
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Migration + implementation + verify.go sentinel `{version: 31, migration: "031_agent_volumes", table: "agent_volume_defs"}` and a column sentinel for `agents.volume_status` if verify.go supports both in one entry (otherwise two entries, as 030 does for a column).
- [ ] **Step 4:** `go test ./internal/store/...` → PASS.
- [ ] **Step 5:** Commit `feat(store): agent volume definitions and status`.

### Task 3: Controller resource API, authz, audit, CLI, AppSource

**Files:**
- Create: `internal/controller/api_agent_volumes.go`, `internal/controller/api_agent_volumes_test.go`
- Modify: `internal/controller/server.go` (routes), `internal/controller/authz_routes.go`, `internal/authz/catalogue.go`, `internal/controller/audit.go`, `internal/controller/appsource_apply.go`, `internal/controller/appsource_reconciler.go` (second pass), `internal/api/types.go`, `internal/cli/apply.go`, `internal/cli/export.go`, docs `docs/operator-manual/authorization.md`, `docs/operator-manual/audit.md`, `docs/user-guide/resources/app-source.md` (synced kinds list), `docs/reference/cli.md`; new user doc `docs/user-guide/resources/agent-volume.md` (resource reference) linked from the resources index.

**Interfaces:**
- Consumes: Task 1 `ParseAgentVolume`; Task 2 store methods.
- Produces:
  - Routes `POST /api/v1/agent-volumes` (body `api.ApplyAgentVolumeRequest{YAML string}`), `GET /api/v1/agent-volumes` (`[]api.AgentVolumeMeta{Name string; Spec dsl.AgentVolumeSpec; UpdatedAt time.Time}`), `DELETE /api/v1/agent-volumes/{name}`.
  - Permissions `authz.AgentVolumeRead` (in viewer) and `authz.AgentVolumeWrite`; audit actions `agentvolume.apply` / `agentvolume.delete` with resource-name extraction like Vars.
  - Apply/delete call `guardManagedResource` with kind `AgentVolume`.
  - CLI `unified-cli apply -f` accepts `kind: AgentVolume` (incl. dry-run) and prints `agent volume applied: <name>`; `export` writes them to an `agent-volumes/` dir.

- [ ] **Step 1: Failing tests:** `TestAgentVolumeApplyListDelete` (apply spec example → 200; list contains it; delete → gone; apply invalid → 400 with the field name); `TestAgentVolumeManagedGuard` (managed by an AppSource → 409); AppSource reconciler test syncing an `AgentVolume` file creates it and prune deletes it; existing `TestEveryRegisteredRouteIsClassified`, `TestEveryHumanWriteRouteIsAuditedOrExempt`, `TestAuditTablesMatchRouteTable` and `export_kind_completeness_test.go` pass.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement following `api_vars.go` and the Vars cases in each switch.
- [ ] **Step 4:** `go test ./internal/controller/... ./internal/cli/... ./internal/authz/...` → PASS.
- [ ] **Step 5:** Commit `feat(controller): AgentVolume resource API, authz, audit, CLI and AppSource sync`.

### Task 4: Delivery — heartbeat response, status, claim, visibility, schedulability

**Files:**
- Modify: `internal/api/types.go`, `internal/controller/api_agent.go` (`handleAgentHeartbeat`, claim upsert), `internal/controller/schedulability.go`, `internal/controller/api_agents*.go` (list), `internal/agent/client.go` (`Heartbeat` signature only), tests `internal/controller/api_agent_volumes_delivery_test.go`, `schedulability_test.go`.
- Docs: `docs/operator-manual/agents.md` (Registration and liveness: heartbeat now returns a body), `docs/troubleshooting/runs-and-scheduling.md:170` (new reason texts).

**Interfaces:**
- Consumes: Task 1 `AgentVolumeMatches`; Task 2 `ListAgentVolumes`, `SetAgentVolumeStatus`, `UpsertAgentOnClaim(..., volumesKnown)`.
- Produces:
  - `api.DesiredVolume { Name, Mode, Size, StorageClassName string }` (json `name`, `mode`, `size`, `storageClassName,omitempty`).
  - `api.HeartbeatResponse { Volumes []DesiredVolume; Revision string }` (json `volumes`, `revision`); `Volumes` always non-nil (empty array when none), sorted by name; `Revision` = hex SHA-256 of the canonical JSON of `Volumes`.
  - Heartbeat answers `200` + `HeartbeatResponse` (was 204) to every authenticated beat, with or without a request body; if `ListAgentVolumes` fails it answers `503` without a body so the agent keeps its last set (never an empty set on a store error).
  - `api.HeartbeatRequest.VolumeStatus []AgentVolumeStatus` (json `volumeStatus,omitempty`), persisted via `SetAgentVolumeStatus` on every beat (nil clears).
  - `api.ClaimRequest.VolumesKnown bool` (json `volumesKnown,omitempty`) forwarded to `UpsertAgentOnClaim`; `AgentRegisterRequest.VolumesKnown bool` likewise for register's full replace (register with known=false must keep stored volumes).
  - `api.AgentInfo.VolumeStatus []AgentVolumeStatus` (json `volumeStatus,omitempty`).
  - `func (c *Client) Heartbeat(ctx, agentID string, req *api.HeartbeatRequest) (*api.HeartbeatResponse, error)` — returns `(nil, nil)` on a 204 (old controller). Update callers in `internal/agent/heartbeat.go` and `internal/k8sagent` minimally (discard the response) so everything builds; Tasks 6–7 consume it.
  - Schedulability reasons: `no AgentVolume "X" is declared` (new) vs existing `no agent declares volume "X"` (keep, now meaning declared but not ready anywhere). Update the doc comment at `schedulability.go:30-34`.

- [ ] **Step 1: Failing tests:**
  - `TestHeartbeatReturnsDesiredVolumes`: definitions `a` (selector `[role:unity]`), `b` (no selector), `c` (`[role:ios]`); agent principal labels `[role:unity]` → response volumes `[a, b]` in order with mode/size; two beats with no change return the same revision; after changing `a`'s size the revision changes.
  - `TestHeartbeatBodylessStillReturnsSet`: a bodyless beat → 200 with the set.
  - `TestHeartbeatStoreErrorReturns503` (inject a failing `ListAgentVolumes` via the existing store-wrapper test pattern if one exists; otherwise drop this test and note it).
  - `TestHeartbeatPersistsVolumeStatus` and `TestAgentsListShowsVolumeStatus`: beat with `[{a, false, "claim name too long"}]` → `GET /api/v1/agents` shows it; next beat without it clears it.
  - `TestClaimWithoutVolumesKnownKeepsStoredVolumes` / `TestClaimWithVolumesKnownClearsVolumes`.
  - `TestSchedulabilityUndeclaredVolume`: job needs `x`, no definition → new reason; definition exists, no agent advertises it → old reason.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement. The desired set uses `agentPrincipalFromContext(r.Context()).AuthorizedLabels`, not the `agents` row.
- [ ] **Step 4:** `go build ./... && go test ./internal/controller/... ./internal/agent/... ./internal/k8sagent/... ./internal/api/...` → PASS.
- [ ] **Step 5:** Commit `feat(controller): deliver AgentVolume sets on heartbeat`.

### Task 5: Host agent — mutable volume manager

**Files:**
- Modify: `internal/agent/volumes.go`, `internal/agent/volumes_mount.go` (link ownership), `internal/agent/storage_sampler.go` (dynamic paths), `internal/agent/volume_gate.go` if it reads `vols`; tests `internal/agent/volumes_apply_test.go`.

**Interfaces:**
- Consumes: Task 4 `api.DesiredVolume`, `api.AgentVolumeStatus`.
- Produces:
  - `func NewVolumeManager(base string) (*VolumeManager, error)` — no configs; starts empty (non-nil manager always; keep nil-safety of methods for tests that pass nil).
  - `func (m *VolumeManager) Apply(desired []api.DesiredVolume) []api.AgentVolumeStatus` — diff against live state under `m.mu`; returns one status per desired name (ready, or not ready with reason) sorted by name. Not-ready reasons, verbatim: `"waiting for N run(s) to release before switching to <mode>"`, `"invalid: <error>"`, `"create directory: <error>"`. (A removed volume is not in the desired set, so it has no status line.)
  - Apply is idempotent and is called on **every** successful heartbeat response, so a deferred removal or mode switch completes at the first Apply after the last holder released (within one heartbeat). No completion-on-release callback is needed.
  - Semantics: add → validate (name, `checkOutOfGCReach`, overlap, EvalSymlinks re-check as today), `MkdirAll` default path (existing contents kept), then ready. Remove → stop offering at once (excluded from `Names()` and from assignment), keep the entry in a `retiring` state while any holder exists, forget it when the last holder releases. Mode change → not offered while held; switch and offer when free. Size change → applied immediately.
  - `Names()` returns only ready, offered volumes. `Busy()` must not nil-deref for a held name that is retiring (`volumes.go:788`).
  - `func (m *VolumeManager) Paths() []string` — current paths of all live entries (ready + retiring) for the sampler.
  - `ownsLinkTarget` / stale-link sweep recognise links into retiring volumes and into any path the manager has ever served this process (keep a set of known paths).
  - All reads of the volume map (`PathOf`, `ModeOf`, `Names`, `CheckUsage`, `ownsLinkTarget`) take `m.mu` (or an RWMutex) — run `go test -race`.
  - `storageSampler.run(ctx, paths func() []string)` — re-read paths every tick.

- [ ] **Step 1: Failing tests** (`go test -race`):
  - `TestApplyAddCreatesDirAndKeepsExisting`: pre-create `<base>/volumes/u/keep.txt`; Apply `[u exclusive 1Gi]` → ready, `keep.txt` still present, `Names()==[u]`.
  - `TestApplyRemoveWhileHeld`: reserve `u` for run r1; Apply `[]` → `Names()==[]`, a new `ReserveSet` for `u` fails with "declares none"; release r1, Apply `[]` again → entry gone; directory still exists.
  - `TestApplyModeChangeWaitsForHolders`: `u` exclusive held by r1; Apply `[u shared]` → status not ready with reason `waiting for 1 run(s) to release before switching to shared`, `Names()` excludes `u`; release, then Apply `[u shared]` again → `u` shared and offered; two runs can then hold it.
  - `TestApplyOneBadVolumeDoesNotBlockOthers`: Apply `[good, bad]` where `bad`'s directory cannot be created (make `<base>/volumes/bad` a regular file) → `good` ready, `bad` not ready with reason prefix `create directory:`.
  - `TestApplyIdempotent`: same set twice → same statuses, no directory changes.
  - `TestLinkCleanupAfterRemoval`: native run with a link into `u`; Apply `[]` while held; run cleanup removes the link (no "replaced by something else" path).
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement; update existing tests that call `NewVolumeManager(base, cfgs)` to `NewVolumeManager(base)` + `Apply`.
- [ ] **Step 4:** `go test -race ./internal/agent/...` → PASS.
- [ ] **Step 5:** Commit `feat(agent): apply volume sets at runtime`.

### Task 6: Host agent — wiring and config removal

**Files:**
- Modify: `internal/config/agent.go` (drop `Volumes` and `VolumeConfig` from the host config; config load errors when the file has a `volumes` key), `cmd/unified-cd-agent/main.go`, `internal/agent/agent.go` (register/claim payloads, sampler paths), `internal/agent/heartbeat.go`; tests `internal/agent/heartbeat_volumes_test.go`, config tests.
- Docs: `docs/reference/configuration.md` (remove `volumes:` from the host example ~320 and the `### Agent volumes` table ~437, say volumes are declared with `AgentVolume`), `docs/troubleshooting/agents-and-enrollment.md:182` (replace the startup config error entry with the new "volumes: is no longer supported" error and the 204 warning).

**Interfaces:**
- Consumes: Task 4 `Client.Heartbeat`, `HeartbeatResponse`, `ClaimRequest.VolumesKnown`, `AgentRegisterRequest.VolumesKnown`; Task 5 `Apply`, `Paths`.
- Produces:
  - `StartHeartbeat(ctx, client, agentID, interval, build func() *api.HeartbeatRequest, onResponse func(*api.HeartbeatResponse)) <-chan struct{}` — sends the first beat immediately; on error calls nothing (last set kept); on `(nil, nil)` (204) logs once `controller does not deliver AgentVolumes; upgrade the controller` and calls nothing.
  - The agent calls `Apply` on every successful response (Apply is idempotent; `Revision` is only used to log "volume set changed" once per change). After each Apply it stores the returned statuses for the next beat's `VolumeStatus` and sets `volumesKnown = true`.
  - Register sends `VolumesKnown: false` and no volumes (the first heartbeat follows immediately); claims send `Volumes: m.Names()`, `VolumesKnown: volumesKnown`.
  - Config: the error text, verbatim: `agent config: "volumes" is no longer supported; declare volumes on the controller with kind: AgentVolume (see docs/operator-manual/migrations/2026-11-18-controller-declared-volumes.md)`.

- [ ] **Step 1: Failing tests:** `TestFirstHeartbeatIsImmediate`; `TestAgentAppliesHeartbeatSet` (fake controller returns `[u]` → `Names()==[u]`, next beat carries status); `TestAgentKeepsLastSetOnHeartbeatError` (after `[u]`, server returns 500 → `Names()` still `[u]`); `TestAgentHeartbeat204Warns` (204 → no volumes, one warning logged); `TestClaimSendsVolumesKnown` (false before first set, true after); `TestConfigRejectsVolumesKey` (file with `volumes:` → the verbatim error).
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go build ./... && go test -race ./internal/agent/... ./internal/config/... ./cmd/...` → PASS.
- [ ] **Step 5:** Commit `feat(agent): receive AgentVolumes from the controller; drop volumes from host config`.

### Task 7: Kubernetes agent

**Files:**
- Modify: `internal/k8sagent/config.go` (drop `Volumes`; error on the key, same verbatim text as Task 6 with `k8s agent config:` prefix), `internal/k8sagent/volumes.go` (`EnsurePVCs` → per-volume ensure; `declaredVolumes()` from live state), `internal/k8sagent/agent.go` (heartbeat wiring, register, claim request), `internal/k8sagent/podpolicy.go` (PVC-name check against the live set), tests `internal/k8sagent/volumes_apply_test.go`.
- Docs: `docs/operator-manual/kubernetes-integration.md` (`## Named volumes` ~411, config example ~194, minFreeDisk ~614 still applies), Helm chart values/templates if they template `volumes` (grep `deploy/` / `charts/`), `docs/reference/configuration.md` k8s example ~589.

**Interfaces:**
- Consumes: Task 4 types and `Client.Heartbeat`; reuse `StartHeartbeat` from Task 6 if the k8s agent uses it, otherwise mirror its contract.
- Produces:
  - `type volumeState` held by `K8sAgent` behind a mutex; `func (a *K8sAgent) applyVolumes(ctx context.Context, desired []api.DesiredVolume) []api.AgentVolumeStatus`.
  - Add → validate name and `claimNameFor(agentName, name)` length ≤ 63 (reason `invalid: claim name "<claim>" exceeds 63 characters`), create PVC if missing (labels and access mode as today; existing PVC → today's mismatch warning), then ready. Create failure → reason `create PVC: <error>`, retried on the next Apply.
  - Remove → stop offering; the PVC is never deleted; log it as orphaned once no pod holds it (holding = today's `heldVolumes` from pod labels).
  - Mode change → not offered while any pod holds it; then switch (existing PVC access mode unchanged, warn).
  - `applyVolumes` is called on every successful heartbeat response, like the host agent, so deferred transitions complete at the first beat after the holding pods are gone.
  - `volumesKnown` flag and claim/register payloads as in Task 6.

- [ ] **Step 1: Failing tests** (fake clientset): `TestK8sApplyCreatesPVC`; `TestK8sApplyKeepsExistingPVC` (pre-existing PVC with data label untouched, no recreate); `TestK8sApplyRemoveWhileHeld` (pod holding the volume → not offered, PVC kept; pod gone → forgotten, PVC still present); `TestK8sApplyClaimNameTooLong` (one long name, one good → good ready); `TestK8sConfigRejectsVolumesKey`.
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go build ./... && go test -race ./internal/k8sagent/...` → PASS; `helm lint` if the chart changed.
- [ ] **Step 5:** Commit `feat(k8sagent): receive AgentVolumes from the controller; drop volumes from config`.

### Task 8: User docs, migration guide, decisions, TODO

**Files:**
- Modify: `docs/user-guide/writing-jobs/volumes.md` (where volumes come from; "Volumes belong to one agent" ~335 now says definitions live on the controller, occupancy on the agent; scheduling section ~290), `docs/operator-manual/agents.md` (`## Named volumes` ~1124, routing ~555), `docs/operator-manual/operations.md` (`## Named-Volume Hygiene` ~185), `docs/troubleshooting/steps-and-execution.md` (entries that mention editing the agent config or restarting to change volumes), `docs/getting-started/concepts.md`, `docs/superpowers/decisions/2026-10-20-agent-named-volumes-rulings.md` (append a note that ruling 55 is superseded for definitions by the 2026-11-18 spec), `TODO.md` if an item covers volume config, release notes/CHANGELOG if the repo keeps one (breaking change for agents).
- Create: `docs/operator-manual/migrations/2026-11-18-controller-declared-volumes.md` (upgrade order controller → agents; per file volume write an `AgentVolume` with same name/mode/size and a selector; ensure agents carry the labels; remove `volumes:`; restart; data is kept because the default path is unchanged; a volume that used a custom `path:` must be moved to `<workspaceDir>/volumes/<name>` first; what the 204 warning means).

- [ ] **Step 1:** `grep -rn -i "volumes:" docs/ examples/ templates/ deploy/ charts/ 2>/dev/null` and `grep -rn -i "agent config" docs/ | grep -i volum` — list every statement now false; fix each.
- [ ] **Step 2:** Write the migration guide and doc changes.
- [ ] **Step 3:** Verify links: every new relative link resolves (`grep -o '](\.\./[^)]*)'` spot check or the repo's link checker if CI has one).
- [ ] **Step 4:** Commit `docs: controller-declared AgentVolumes, migration guide`.
