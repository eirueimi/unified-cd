# Disk-headroom gating — Implementation plan

Design: `docs/superpowers/specs/2026-10-23-disk-gating-design.md`. Section/decision numbers (D1..D9, Q1..Q9) refer to it.

Process (repo `AGENTS.md`): cut a worktree first (`git worktree add ../unified-cd-disk-gating -b disk-gating`),
commit only there. Everything written in English. Delegate each task to a fresh implementer; review each
task before the next. Tasks 1-4 (host) and 6-7 (Kubernetes) are independent after Task 1.

Dependency order: 1 -> (2 -> 3 -> 4) and (5 -> 6, 7) -> 8 -> 9.
Tasks 2-4 share `internal/agent/agent.go`; Tasks 5-7 share `internal/k8sagent/`. Tasks 6/7 touch
`podbuilder.go` **not at all** (see design section 5).

Resolve open questions Q1-Q3 with the user before Task 2 starts (they change defaults and startup
behaviour). Q4-Q9 have recommended answers the tasks below assume.

Standard verification for every task (run at least the package tests; run all before the final task):

```
go build ./...
go vet ./...
go test ./internal/<pkg>/... -count=1
go test ./... -count=1          # before each commit that crosses packages, and in Task 9
python -m mkdocs build --strict # any task that edits docs/ or mkdocs.yml
```

---

## Task 1 — `config.DiskThreshold` type (pure, no behaviour change)

**Files**
- Create `internal/config/diskthreshold.go`, `internal/config/diskthreshold_test.go`

**Tests first** (table-driven)
- Parse: `"1%"`, `"0.5%"`, `"1.5 %"`, `"10GiB"`, `"500MB"`, `"10Gi"`, `"5G"`, `"5368709120"`, `"0"`, `"off"`,
  `"disabled"`, `""` (unset), whitespace padding.
- Reject: `"-1"`, `"0%"`, `"100%"`, `"101%"`, `"abc"`, `"1.2.3%"`, `"10XB"`, overflow (`"99999999999999999999"`).
- `Resolve(total)`: `1%` of `1<<40` is `(1<<40)/100` rounded down; absolute ignores total; `Off` -> 0.
- `Below(free, total)`: exactly-at-threshold is **not** below (matches `belowMinFreeDisk`, `free < min`);
  `Off` never below; `total == 0` with a percent is never below (cannot judge; fail open).
- `UnmarshalYAML`: int scalar `5368709120`, string `"5Gi"`, string `"1%"`, `0`, absent field leaves `Set == false`,
  `minFreeDisk: 0` sets `Set == true, Off == true`; a mapping or list node errors.
- `String()` round-trips (used in logs and `--help` defaults).
- Strict-mode: `LoadAgent` rejects unknown fields; the new type must not weaken that.

**Implementation notes**
- Struct `DiskThreshold{Percent float64; Bytes uint64; Set, Off bool}`; zero value = unset.
- Suffix handling: strip a trailing `B` after `Ki|Mi|Gi|Ti` (`GiB` -> `Gi`) and after `K|M|G|T` (`GB` -> `G`, SI),
  then `resource.ParseQuantity` (already imported in `internal/agent/volumes.go:116`); bare digits = bytes.
- `Default()` returns `DiskThreshold{Percent: 1, Set: false}`; callers use `OrDefault()`.

**Verify** `go test ./internal/config/ -count=1`

**Docs:** none yet (Task 8).

---

## Task 2 — Host agent config/flag/env/yaml plumbing and default

**Files**
- Modify `internal/config/agent.go` (`AgentConfig.MinFreeDisk` -> `DiskThreshold`; env parse at ~225; merge at ~311 uses `Set`)
- Modify `cmd/unified-cd-agent/main.go` (flag at 115 becomes `flag.String`; assignment at 242; log effective value at startup)
- Modify `internal/agent/agent.go` (`Agent.MinFreeDisk` type at 104-107)
- Tests: `internal/config/agent_volumes_test.go` neighbours -> new `internal/config/agent_minfree_test.go`;
  `cmd/unified-cd-agent` flag test if one exists (`internal/config/version_flag_test.go` shows the pattern; check `flagscan.go`)

**Tests first**
- Priority order flag > file > env > default, for each form (`5368709120`, `"1%"`, `"10GiB"`).
- Legacy: existing byte value from each of flag, env, yaml gives `Bytes == value`.
- Explicit `minFreeDisk: 0` in the file beats the default (`Off`); explicit `UNIFIED_AGENT_MIN_FREE_DISK=0` too.
- Unset everywhere -> `1%`.
- Malformed env value -> `AgentEffective` returns an error / startup fails with a message naming the variable (Q2).
- `stale_refs_test.go` (`internal/config`) must still pass; update it if it pins the flag help text.

**Implementation notes**
- `AgentEffective` currently cannot return an error; either add an error return (check callers) or validate in
  `main.go` after loading. Prefer the smallest change; record the choice in the PR.
- Startup INFO: `disk gate: minFreeDisk=1% (default)` / `minFreeDisk=5368709120 bytes` / `disabled`.
- Update `--min-free-disk` help string (host only; accepts bytes, `10GiB`, or `1%`; `0` disables; default `1%`).

**Verify** `go test ./internal/config/ ./cmd/... -count=1`, `go build ./...`

**Docs:** deferred to Task 8, but note every identifier you rename so Task 8 can grep.

---

## Task 3 — Host claim gate: percent workspace gate + named-volume gate

**Files**
- Modify `internal/agent/agent.go` (`runLoop` gate block at ~546-565; `storageReading` at ~202-225)
- Modify `internal/agent/diskfree.go` (replace `belowMinFreeDisk(free, min uint64)` use with `DiskThreshold.Below`; keep or remove the old helper, updating `diskfree_test.go`)
- Create `internal/agent/volume_gate.go` (+ `_test.go`): `gatedVolumes(sampler, threshold) []string`, per-volume transition logging, "not above threshold" skip (D3)
- Modify `internal/agent/volumes.go` only if a read-only accessor is needed (`Names()`, `PathOf()` already exist)
- Modify `internal/api/types.go` (`VolumeStats.ClaimBlocked`, doc comments on `BusyVolumes` and `MinFreeDiskBytes`)

**Tests first** (extend `internal/agent/diskfree_claimgate_test.go` patterns: injected `diskStatsFn`)
- Workspace: 1% threshold, free 0.5% -> skips claim, `blockedSlots` == 1, WARN contains threshold bytes and percent;
  free 2% -> claims. Absolute threshold behaves exactly as before. `Off` never gates. `statfs` error and zero
  total still claim (existing tests unchanged).
- Volume gate (sampler seeded with fake readings and an injected `now`): volume `v` below threshold ->
  `ClaimRequest.BusyVolumes` contains `v`; the claim still happens (workspace fine); a volume at/above the
  threshold, never measured, or stale -> absent from the list. `Busy()` entries are preserved and sorted
  deterministically (volumes, then paths, then gated; or de-duplicated union — pick one and test it).
- Absolute threshold >= volume total -> volume not gated, single WARN per volume per process (second loop iteration logs nothing).
- Transition logging: entering and leaving the gated state each log once, not every 2s.
- Detached slot loop: no volumes, so only the workspace gate applies (assert `ClaimDetached` request unchanged).
- Heartbeat: `storageReading` sets `Volumes[name].ClaimBlocked` for gated volumes; `MinFreeDiskBytes` = resolved
  workspace bytes for a percent (e.g. 1% of the faked total), `0` only when `Off`, and **nil** when percent is
  configured but the workspace sample is missing (design D8). Update `heartbeat_storage_test.go` accordingly.
- **No inline statfs on a volume path**: a test whose `diskStatsFn` blocks forever for a volume path must not
  stall `runLoop`'s claim (regression guard for rulings #8).
- Goroutine safety: run the package with `-race`.

**Implementation notes**
- The workspace check stays inline as today; only volumes read the sampler (D5). The sampler instance is created in
  `Run` (~line 410); pass it to `runLoop` or hang it off `Agent` (an unexported field set in `Run`).
- Compute `gatedVolumes` once per loop iteration, after the workspace check and before `Client.Claim`.

**Verify** `go test ./internal/agent/... -race -count=1`, then `go test ./... -count=1`

---

## Task 4 — Metrics for the new series and resolved threshold

**Files**
- Modify `internal/metrics/collector.go` (new `volumeClaimBlockedDesc`; `Describe`; `emitAgentStorage`; reword `minFreeDiskDesc` help)
- Modify `internal/metrics/collector_test.go`
- Check `internal/store/postgres.go:1686` comment and `internal/store/agent_storage_test.go` (JSON round-trip of `ClaimBlocked`; no migration needed)
- Check `internal/controller/api_agent_storage_test.go` for the heartbeat path

**Tests first**
- `unifiedcd_agent_volume_claim_blocked{agent,agent_name,volume}` is 1/0 per volume that has a stamped reading; absent
  when the volume is dropped (`MeasuredAt` zero rule, rulings #10); never emitted for an agent that sent no volumes.
- `min_free_disk_bytes` unchanged for byte values, `0` for disabled, omitted when nil.
- Store: `SetAgentStorage` / `ListAgentStorage` round-trip `ClaimBlocked: true` and treat a missing key as false.
- Old-agent payload (no `claimBlocked`) decodes to false.

**Verify** `go test ./internal/metrics/ ./internal/store/ ./internal/controller/ -count=1` (store tests start PostgreSQL via Docker; if Docker is unavailable say so, do not skip silently)

---

## Task 5 — Kubernetes agent config: `minFreeDisk` and `workspaceSizeLimit`

**Files**
- Modify `internal/k8sagent/config.go` (`Config.MinFreeDisk config.DiskThreshold` yaml `minFreeDisk`; `Config.WorkspaceSizeLimit string` yaml `workspaceSizeLimit`; `AgentPodTemplate.WorkspaceSizeLimit string` yaml `workspaceSizeLimit`; env `UNIFIED_K8S_MIN_FREE_DISK`, `UNIFIED_K8S_WORKSPACE_SIZE_LIMIT` in `Validate`, next to the other `UNIFIED_K8S_*` at ~311-326)
- Modify `internal/k8sagent/config_test.go`

**Tests first**
- Unset `minFreeDisk` -> effective `1%`; explicit `0` -> off; bytes / `"10Gi"` / `"1%"` parse; malformed -> `Validate` error naming the field.
- `workspaceSizeLimit` must parse with `resource.ParseQuantity`, must be > 0; `""` = unset; malformed -> `Validate` error; per-template value validated the same way.
- Config remains strict about unknown keys.
- Startup INFO when `workspaceSizeLimit` is unset (assert via a log capture helper, or keep the log in `cmd/k8s-agent/main.go` and test the predicate).

**Verify** `go test ./internal/k8sagent/ -run 'Config' -count=1`

---

## Task 6 — Kubernetes (b): volume headroom claim gate

**Files**
- Modify `internal/k8sagent/volumes.go` (`claimRequest` at ~499-521; helper `gatedVolumes`; reading age check)
- Modify `internal/k8sagent/agent.go` only for the probe-run warning at the pre-run measurement (~551): after `measureVolumes`, call `warnRun` for each volume still below threshold
- Create `internal/k8sagent/volume_gate.go` (+ test) holding `volumeGateMaxAge = 10 * time.Minute` and the pure decision `volumeGated(stats api.VolumeStats, th config.DiskThreshold, now time.Time) bool`
- Modify `volumeReadings` (volumes.go ~830) only to expose a `get(name)`-style snapshot (the existing `snapshot()` already works; prefer using it)
- Extend `internal/k8sagent/volumes_storage_test.go`, `volumes_agent_test.go`

**Tests first**
- `volumeGated`: below threshold and fresh -> true; at threshold -> false; older than 10 min -> false (lockout escape, D6);
  never measured -> false; threshold >= total -> false; `Off` -> false.
- `claimRequest`: a declared volume with a low fresh reading appears in `BusyVolumes` for both `exclusive` and `shared`
  modes; a held-and-low volume appears once; volumes with no declared config are ignored; an error reading pods still
  returns the gated names (gating is local; only the pod-derived busy list degrades).
- Transition logs: once on entering, once on leaving.
- ClaimBlocked set on the heartbeat `Volumes` entries via `storageReading` (`volumes.go` ~855).
- Probe-run warning: a pre-run reading under threshold emits one `warnRun` line naming volume, free, percent, threshold; a healthy
  reading emits none.
- Replica independence: two `volumeReadings` hold different values; each agent gates on its own (document-only test).

**Verify** `go test ./internal/k8sagent/... -race -count=1`

---

## Task 7 — Kubernetes (a): workspace size limit, ephemeral-storage request, readable eviction

**Files**
- Create `internal/k8sagent/workspace_limit.go` (+ `_test.go`): `applyWorkspaceLimit(pod *corev1.Pod, limit resource.Quantity)`, `resolveWorkspaceSizeLimit(cfg Config, tmplName string) (resource.Quantity, bool)`
- Modify `internal/k8sagent/volumes.go` `buildAndCreate` (line ~723): one call after `BuildPod`. **Do not edit `podbuilder.go`** (concurrent changes to `resolveWorkspaceConfig`; design section 5)
- Modify `internal/k8sagent/scopepod.go` (`createScopePod`, scratch volume at ~29): same helper
- Modify `internal/k8sagent/backend.go` (the four `ExecStep` call sites at ~236, 258, 282, 1238) via one helper `explainPodLoss(ctx, podName, err)`; add `PodManager.GetPod` if missing (`podmanager.go` has `ListPods`; add a thin `GetPod`)
- Modify `internal/k8sagent/fakepm_test.go` if the fake needs `GetPod`

**Tests first**
- `applyWorkspaceLimit`: sets `emptyDir.sizeLimit` on `workspace`; adds `requests["ephemeral-storage"]` to container `job` only;
  **no** container `limits` added; unchanged if any container already has an `ephemeral-storage` request or limit; unchanged
  `sizeLimit` if already set; pod without a `workspace` volume or `job` container is left as is (no panic); other emptyDirs
  (`/.ucd` tools volume, sidecar volumes) untouched.
- `resolveWorkspaceSizeLimit`: template value beats global; global beats unset; unset -> not applied; pooled/reuse pods get it too.
- `buildAndCreate` (fake clientset): created pod carries the limit; without config the created pod is byte-identical to today's
  (golden comparison of the `workspace` volume and the `job` container resources).
- `explainPodLoss`: a pod fixture with `Status.Phase=Failed, Reason=Evicted, Message='Usage of EmptyDir volume "workspace" exceeds the limit "20Gi".'`
  produces an error containing the kubelet message **first**, then the hint naming `workspaceSizeLimit`; with no limit configured
  the hint is omitted but the kubelet message is kept; a pod that still exists and is Running leaves the error untouched; a failing
  or slow `GetPod` (5s bound) returns the original error; `NotFound` returns the original error.
- Integration (if the repo's kind/envtest harness exists — `podmanager_integration_test.go`, `testutil_k8s_test.go` — add one case; otherwise
  note the gap in the PR): an oversized write in a real pod is evicted and the run's failure reason is the wrapped message.
- `PodTemplateNeedsKubernetes` is **not** changed: add a test asserting a job without `ephemeral-storage` still does not require Kubernetes after the agent injects one (the injection is agent-side only).

**Verify** `go test ./internal/k8sagent/... ./internal/dsl/... -race -count=1`

---

## Task 8 — Documentation, examples, migration note

Write after Tasks 1-7 are green (final names and behaviour known). Grep every changed identifier first:
`grep -rn -i "minFreeDisk\|min-free-disk\|MIN_FREE_DISK\|claim_blocked_by_disk\|min_free_disk_bytes\|ephemeral-storage" docs examples README.md manifests --include=* | grep -v superpowers/`
(`docs/superpowers/**` is historical: do not edit specs/plans/decisions, except add a short decision file below if rulings were made.)

**Files**
- `docs/reference/configuration.md`: host flag table (~160) new type/default/help; env table (~192) now "default `1%`, `0` disables, host **and** volume gating" (drop "no preflight" claim); yaml example (~237) with `"1%"` and `10GiB` forms; the long `minFreeDisk` paragraph (~291-305) rewritten (percent, default on, volumes, fail-open rules, startup error on malformed value); k8s section: `minFreeDisk`, `workspaceSizeLimit` rows in the field table (~494 region) and `podTemplates.<name>.workspaceSizeLimit`; env table rows for `UNIFIED_K8S_MIN_FREE_DISK`, `UNIFIED_K8S_WORKSPACE_SIZE_LIMIT`; fix the sentence "`minFreeDisk` ... are host agent only".
- `docs/operator-manual/operations.md`: the preflight-lever bullet (~115-125, "`0` (default) disables" is now false); "Watching agent disk headroom" (~455-650) — metric table (`volume_claim_blocked`, new meaning of `min_free_disk_bytes`), the "`minFreeDisk` defaults to `0`" bullet (~571-576), the Kubernetes-omits bullet (~577-581, a Kubernetes agent now gates volumes and omits only the workspace series), the PromQL alert examples (~644-649 assume `0 = disabled`); a "choose a threshold from your workload" paragraph (floor vs margin, Unity example `60GiB`).
- `docs/operator-manual/kubernetes-integration.md`: resource-requests bullet (~58-70) note that the agent injects an `ephemeral-storage` request only when `workspaceSizeLimit` is set and never overrides the job's; workspace section (~361) add the limit; volumes section (~390-430) volume claim gate, "never measured", 10-minute re-probe, restart as reset, per-replica caches; new "Disk limits and headroom" subsection with `kubectl` check for evictions.
- `docs/operator-manual/agents.md` (~687 "A job's workspace is ephemeral"): the workspace gate and volume gate in two sentences, link to operations.
- `docs/user-guide/writing-jobs/volumes.md`: job-author view — "if the agent's volume is below its free-space threshold, runs that mount it stay Queued; other jobs are unaffected"; replace any text implying the advisory `size` is the only guard.
- `docs/troubleshooting/agents-and-enrollment.md` (~195-215): update the existing entry — default threshold, percent log attrs, how to tell workspace gate from volume gate; keep the exact log string `free disk space below minimum, skipping claim` (grep-able; if attrs change, show the new example line).
- `docs/troubleshooting/runs-and-scheduling.md`: new entry "Run stays Queued: the volume it mounts is below `minFreeDisk`" with the exact log text and the `volume_claim_blocked` series, how to free space / lower the threshold / restart a Kubernetes agent to clear its cache.
- `docs/troubleshooting/steps-and-execution.md` or `artifacts-and-storage.md` (whichever hosts k8s pod failures; `artifacts-and-storage.md` already has the CreateContainerConfigError entry): new entry "Kubernetes run fails: pod was evicted: Usage of EmptyDir volume "workspace" exceeds the limit" with the exact message and the three remedies.
- `docs/troubleshooting/index.md` (~68): add the two new entry links.
- `docs/operator-manual/migrations/2026-10-23-disk-gating-defaults.md` (new): before->after table (default 0 -> 1%; byte values unchanged; `0` still disables; env typo now fatal; volumes gated; k8s volume gate; k8s limit opt-in with the unset default; `min_free_disk_bytes` semantics; new series), "what to do before upgrading" (check `df` of every agent host; set an explicit value; `minFreeDisk: 0` to opt out), exact startup error string, rollback note. Add it to `mkdocs.yml` nav if migrations are listed there.
- `examples/config/agent.yaml` (~38-42): the comment "The default, 0, DISABLES the check" is now false; rewrite with `"1%"`/`10GiB` examples. Also check `examples/config/*` for a k8s agent config and any `manifests/` ConfigMap that lists agent config keys (grep above).
- `README.md`: only if it mentions the disk check (grep; currently no hit).
- `TODO.md`: no entry exists for this work (`grep -n -i "disk\|min-free" TODO.md` -> none relevant); add nothing, state that in the PR.
- Optional `docs/superpowers/decisions/2026-10-23-disk-gating-rulings.md`: record rulings taken during implementation, in the format of the 2026-10-21 file.
- `site/` is the committed mkdocs output in this checkout: do **not** hand-edit; check whether it is tracked before deciding (`git ls-files site | head`).

**Verify**
```
python -m mkdocs build --strict
grep -rn -i "defaults to .0.\|0 (default) disables\|host agent only" docs/reference docs/operator-manual   # no stale hits about minFreeDisk
go test ./internal/dsl/ -run 'Templates|Examples' -count=1                                              # examples still parse
```

---

## Task 9 — Whole-branch verification and review

- `go build ./... && go vet ./... && go test ./... -count=1` and `go test -race ./internal/agent/... ./internal/k8sagent/...`
- `python -m mkdocs build --strict`
- Cross-build the agent for windows and linux (the `statfs` shim has build-tag splits, rulings #3): `GOOS=windows go build ./cmd/unified-cd-agent && GOOS=linux go build ./cmd/unified-cd-agent && GOOS=darwin go build ./cmd/unified-cd-agent`
- Manual host check: run the agent with `--min-free-disk 99%` against a scratch `workspaceDir`; confirm WARN log, `claim_blocked_by_disk 1`, then `--min-free-disk 1%`; with a declared volume and `99%`, confirm a volume job stays Queued while a no-volume job runs.
- Manual k8s check (kind): set `workspaceSizeLimit: 50Mi`, run a job that writes 200Mi, confirm Evicted pod and the wrapped run failure; confirm `kubectl describe pod` shows the `ephemeral-storage` request.
- Docs sweep: `grep -rn` for every identifier renamed or reworded in Tasks 1-8, zero stale hits outside `docs/superpowers/` history.
- Final report names every doc updated (user requirement: completion reports list updated documents).
- Whole-branch review by a stronger-model reviewer (architecture-level: lockout/escape, fail-open paths, default-on blast radius).
