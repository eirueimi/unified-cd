# Agent storage metrics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Publish each agent's disk headroom through the controller's `/metrics`, so an operator can alert before a job fails for want of space.

**Architecture:** Every figure is a `statfs` — never a tree walk. A host agent measures its workspace filesystem and each declared volume's filesystem on every heartbeat; a Kubernetes agent measures a volume from inside the claim pod that already mounts it, through a new `ucd-sh` subcommand. Readings travel on the existing heartbeat, are persisted, and are exported by a scrape-time collector reading the database — not by in-process gauges, because the controller runs several replicas behind a load balancer.

**Tech Stack:** Go, PostgreSQL (pgx), Prometheus client_golang, Kubernetes client-go, `mvdan.cc/sh` (the `ucd-sh` shim).

**Spec:** `docs/superpowers/specs/2026-10-21-agent-storage-metrics-design.md` — currently held alongside this plan outside the repository; both land on the implementation branch together.

**Branch:** this work depends on named volumes (PR #176). Cut the implementation branch from `worktree-agent-named-volumes` if that PR has not merged, or from `main` if it has.

## Global Constraints

- Everything written in this repository is in English: code, comments, commit messages, docs, error strings. No Japanese.
- No personally identifying information in any file. Placeholders are `/path/to/unified-cd` and `your-username`.
- `docs/reference/field-reference.md` and `schemas/unified-cd.schema.json` are generated. Never hand-edit; run `go generate ./...` and commit the diff, reverting any `internal/shim/embedded` churn.
- Metric names carry the existing `unifiedcd_` prefix.
- A Kubernetes agent **omits** a series it cannot answer. It never sends zero: a zero reads as "fine".
- `minFreeDisk` is published as-is, and **0 means the check is disabled** — which is the default.
- The volume list in a heartbeat is a **snapshot**: the controller replaces what it holds for that agent.
- Readings are persisted and exported at scrape time. In-process gauges are wrong here and the spec says why.
- A feature change is not done until `docs/`, `examples/` and `templates/` agree with the code (`AGENTS.md`).

## Review Focus

Failure modes the spec implies that no task's happy path exercises. Each has a test pinned to the task that owns the code.

1. **An agent's row is deleted or reaped while Prometheus keeps scraping.** Its series must disappear, not freeze at its last healthy value — the spec's central anti-goal. — Task 6.
2. **A Kubernetes agent restarts.** Its in-memory volume readings are gone, so those series must be absent until a run next mounts each volume, never zero and never the pre-restart number. — Task 5.
3. **`statfs` fails** — the path is gone, the exec is refused, the mount is unreadable. Nothing must be published for that target, and no timestamp either. — Tasks 1, 2, 5.
4. **`minFreeDisk` is unset.** It publishes 0, and 0 must be distinguishable from "no space left" by anyone reading the metric or the docs. — Tasks 4 and 7.
5. **Two Kubernetes replicas share a PVC.** Two series exist for one physical volume with different ages; an operator alerting on the raw series gets one alert per replica, and the documented query must aggregate. — Tasks 5, 6 and 7.

---

### Task 1: `diskStats` returns free and total

**Files:**
- Modify: `internal/agent/diskfree_unix.go`, `internal/agent/diskfree_windows.go`
- Modify: `internal/agent/agent.go` (the `freeBytesFn` field and its one call site, ~line 431)
- Test: `internal/agent/diskfree_test.go`

**Interfaces:**
- Produces: `func diskStats(path string) (free, total uint64, err error)` in both platform files. `freeBytes` either grows into it or delegates; `Agent.freeBytesFn` becomes `diskStatsFn func(string) (uint64, uint64, error)`.

- [ ] **Step 1: Write the failing tests**

```go
func TestDiskStats_ReportsBothAndTotalExceedsFree(t *testing.T)
    // on t.TempDir(): total > 0, free > 0, free <= total
func TestDiskStats_MissingPathIsAnError(t *testing.T)
    // a path that does not exist returns an error, not (0, 0, nil)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/agent/ -run TestDiskStats -v`
Expected: FAIL, undefined `diskStats`.

- [ ] **Step 3: Implement `diskStats` on both platforms**

Unix: total is `uint64(stat.Blocks) * uint64(stat.Bsize)`; free stays `Bavail * Bsize` (available to an unprivileged writer, which is the number that matters). Windows: `GetDiskFreeSpaceEx` already returns `totalBytes` and the current code discards it.

- [ ] **Step 4: Run to verify they pass, on this platform**

Run: `go test ./internal/agent/ -run TestDiskStats -v`
Expected: PASS. Note in the report that CI covers the other platform.

- [ ] **Step 5: Commit**

---

### Task 2: `ucd-sh statfs`

**Files:**
- Modify: `cmd/ucd-sh/main.go` (the `switch args[0]` at ~line 29, and `usage`)
- Create: `internal/shim/statfs.go`, `internal/shim/statfs_unix.go`, `internal/shim/statfs_windows.go`
- Test: `cmd/ucd-sh/main_test.go`, `internal/shim/statfs_test.go`

**Interfaces:**
- Produces: `ucd-sh statfs <path>` printing one line of machine-readable output — `free=<bytes> total=<bytes>` — to stdout, exit 0. On error: a message to stderr and a non-zero exit, printing nothing to stdout.

**Why this exists:** a Kubernetes agent cannot assume `df` is in the job image. `/.ucd/ucd-sh` is already mounted in every container, so the measurement needs nothing from the image. This mirrors the existing `pause` and `--install` subcommands.

- [ ] **Step 1: Write the failing tests**

```go
func TestUcdSh_Statfs_PrintsFreeAndTotal(t *testing.T)
    // run(["statfs", t.TempDir()]) exits 0 and stdout parses to two positive
    // numbers with free <= total
func TestUcdSh_Statfs_MissingPathExitsNonZeroAndPrintsNothing(t *testing.T)
    // stdout is empty; a caller must not be able to parse a zero out of it
func TestUcdSh_Statfs_WrongArgCountIsUsage(t *testing.T)
    // exit 2, matching the other subcommands
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/ucd-sh/ ./internal/shim/ -run Statfs -v`
Expected: FAIL.

- [ ] **Step 3: Implement the subcommand and the platform helpers**

Keep the parsing contract boring and greppable; Task 5 parses it. Do not reuse `internal/agent`'s helper — the shim must not import the agent.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./cmd/ucd-sh/ ./internal/shim/ -v`
Expected: PASS.

- [ ] **Step 5: Regenerate the embedded shim and commit**

The shim binary is embedded; run `go generate ./...` and commit whatever `internal/shim/embedded` legitimately changes here — this is the one task where that churn is real rather than noise.

---

### Task 3: Persist the readings — migration 023 and the store

**Files:**
- Modify: `internal/api/types.go` (the storage types)
- Create: `internal/store/migrations/023_agent_storage.up.sql`, `023_agent_storage.down.sql`
- Modify: `internal/store/postgres.go`, `internal/store/verify.go` (schema sentinel)
- Test: `internal/store/agent_storage_test.go`

**Interfaces:**
- Produces:
  - `api.AgentStorage{ Workspace *api.FilesystemStats; Volumes map[string]api.VolumeStats; MinFreeDiskBytes uint64; ClaimBlockedByDisk bool }`, with `FilesystemStats{FreeBytes, TotalBytes uint64}` and `VolumeStats{FilesystemStats; MeasuredAt time.Time}`. The pointer and the map are what let a Kubernetes agent omit what it cannot answer. These types are defined **here**, not in Task 4, because the store persists them and Task 4 consumes them.
  - `SetAgentStorage(ctx, agentID string, s api.AgentStorage) error` — replaces the agent's whole reading, snapshot semantics.
  - `ListAgentStorage(ctx, staleAfter time.Duration) (map[string]api.AgentStorage, error)` — live agents only, for the collector.

- [ ] **Step 1: Write the failing tests**

```go
func TestSetAgentStorage_ReplacesTheWholeReading(t *testing.T)
    // writing {a, b} then {a} leaves only a — a dropped volume is gone
func TestListAgentStorage_OmitsAgentsPastStaleAfter(t *testing.T)
    // an agent whose last_seen_at is older than staleAfter is absent from the map
func TestListAgentStorage_OmitsADeletedAgent(t *testing.T)
    // Review Focus 1, at the store layer
func TestSetAgentStorage_UnknownAgentIsANoOp(t *testing.T)
    // a write for an agent with no row changes nothing and returns no error.
    // handleAgentHeartbeat calls TouchAgent first, which re-creates a missing
    // row, so the only way here is a deregister racing in between — benign,
    // and not worth failing a heartbeat over.
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/store/ -run AgentStorage -v`
Expected: FAIL (requires Docker; `NewTestPostgres` starts its own PostgreSQL).

- [ ] **Step 3: Write migration 023 and the two store methods**

022 is the latest on the named-volumes branch. Additive only. `verify.go` carries a schema sentinel per migration and `TestSchemaSentinelsCoverAllMigrations` fails without one — add it in this task.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/store/ -count=1`
Expected: PASS, existing store tests included.

- [ ] **Step 5: Commit**

---

### Task 4: The host agent measures and reports

**Files:**
- Modify: `internal/api/types.go` (`HeartbeatRequest` only — the storage types land in Task 3)
- Modify: `internal/agent/heartbeat.go`, `internal/agent/agent.go`
- Modify: `internal/controller/api_agent.go` (`handleAgentHeartbeat`, ~line 99)
- Test: `internal/agent/heartbeat_test.go`, `internal/controller/api_agent_test.go`

**Interfaces:**
- Consumes: `diskStats` (Task 1); `api.AgentStorage` and `SetAgentStorage` (Task 3).
- Produces: `HeartbeatRequest.Storage *api.AgentStorage`.

- [ ] **Step 1: Write the failing tests**

```go
func TestHeartbeat_HostReportsWorkspaceAndEachVolume(t *testing.T)
    // two declared volumes: both appear, with free <= total
func TestHeartbeat_ReportsTheConfiguredThreshold(t *testing.T)
    // minFreeDisk 5GiB is reported as 5GiB
func TestHeartbeat_UnsetThresholdReportsZero(t *testing.T)
    // Review Focus 4: 0, not omitted — the disabled case is itself a signal
func TestHeartbeat_ClaimBlockedIsReportedByTheAgent(t *testing.T)
    // free below the threshold: ClaimBlockedByDisk true, and false once above
func TestHeartbeat_AVolumeWhoseStatfsFailsIsOmitted(t *testing.T)
    // Review Focus 3: absent from the map, no zero entry, and the other
    // volume is still reported
func TestHandleAgentHeartbeat_PersistsTheStorage(t *testing.T)
    // the controller side: the reading reaches SetAgentStorage
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/agent/ ./internal/controller/ -run 'Heartbeat' -v`
Expected: FAIL.

- [ ] **Step 3: Implement the measurement, the wire field and the handler**

The agent reports `ClaimBlockedByDisk` rather than the controller inferring it: the agent knows it at the moment it declines, and a controller comparing a threshold against a reading taken at a different time would be guessing. `handleAgentHeartbeat` already calls `TouchAgent`; persist the storage beside it, and do not fail the heartbeat if only the storage write fails — liveness matters more than a metric.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/agent/ ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

---

### Task 5: The Kubernetes agent measures a volume from inside the pod

**Files:**
- Modify: `internal/k8sagent/volumes.go`, `internal/k8sagent/agent.go`
- Test: `internal/k8sagent/volumes_storage_test.go`

**Interfaces:**
- Consumes: `ucd-sh statfs` (Task 2), `api.AgentStorage` (Task 4), the existing `a.exec.ExecStep(ctx, podName, container, script, ...)`.
- Produces: an in-memory, per-volume cache of the latest `api.VolumeStats`, sent on each heartbeat as `Storage.Volumes`. `Storage.Workspace` is nil, `MinFreeDiskBytes` is 0 and `ClaimBlockedByDisk` is false, because a Kubernetes agent answers none of those.

- [ ] **Step 1: Write the failing tests**

```go
func TestK8sStorage_MeasuresEachMountedVolume(t *testing.T)
    // a run mounting two volumes caches two readings with MeasuredAt set
func TestK8sStorage_OmitsWorkspaceAndThreshold(t *testing.T)
    // Storage.Workspace is nil and ClaimBlockedByDisk is false — the agent
    // reports nothing it cannot answer
func TestK8sStorage_AFailedExecCachesNothing(t *testing.T)
    // Review Focus 3: no entry, no zero, no timestamp
func TestK8sStorage_AFreshAgentReportsNoVolumes(t *testing.T)
    // Review Focus 2: before any run mounts anything, Volumes is empty —
    // this is what a restart looks like
func TestK8sStorage_ReadingsSurviveUntilTheNextMeasurement(t *testing.T)
    // the cache is not cleared between heartbeats
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/k8sagent/ -run K8sStorage -v`
Expected: FAIL.

- [ ] **Step 3: Measure at mount time and cache**

Run the measurement where the run already has the pod and the mounts — reuse the exec path `clearReusedWorkspace` uses. Parse Task 2's output strictly: anything unparseable is a failure that caches nothing, not a zero.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/k8sagent/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

---

### Task 6: The scrape-time collector

**Files:**
- Modify: `internal/metrics/collector.go` (the `Counts` interface, `dbCollector`, `Describe`, `Collect`)
- Modify: `cmd/controller/main.go` (whatever passes `Counts` to `RegisterDBCollector`)
- Test: `internal/metrics/collector_test.go`

**Interfaces:**
- Consumes: `ListAgentStorage(ctx, staleAfter)` (Task 3) — add it to the `Counts` interface the collector already takes, or a sibling interface if that reads better.
- Produces the series in the spec's §4.5, labelled `agent`, and `agent_name` plus `volume` on the per-volume ones.

- [ ] **Step 1: Write the failing tests**

```go
func TestCollector_ExportsWorkspaceVolumeAndThreshold(t *testing.T)
    // a fake Counts with one agent yields the expected series and values
func TestCollector_ADeletedAgentHasNoSeries(t *testing.T)
    // Review Focus 1: the store returns nothing for it, so nothing is exported
    // — this is the test that proves the design's central promise
func TestCollector_OmitsSeriesTheAgentDidNotReport(t *testing.T)
    // a Kubernetes-shaped reading exports volumes but no workspace,
    // no min_free_disk and no claim_blocked
func TestCollector_TwoReplicasSharingAVolumeExportTwoSeries(t *testing.T)
    // Review Focus 5: same agent_name and volume, different agent, both present
func TestCollector_AStoreErrorIncrementsCollectorErrors(t *testing.T)
    // follow what the existing collector does on CountRunsByStatus failure
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/metrics/ -run Collector -v`
Expected: FAIL.

- [ ] **Step 3: Extend the collector**

Follow `dbCollector`'s existing shape exactly: `Describe` lists the new descs, `Collect` reads under `collectTimeout` and counts errors rather than failing the scrape.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/metrics/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

---

### Task 7: Documentation

**Files:**
- Modify: `docs/operator-manual/operations.md`, `docs/reference/configuration.md`, `docs/troubleshooting/` (the agent-side page), `docs/user-guide/writing-jobs/volumes.md`, and wherever `/metrics` is documented.

- [ ] **Step 1: Write the metrics reference**

Every series with its labels, and three things a reader must not have to discover: which series a Kubernetes agent omits and why; that a Kubernetes volume figure refreshes only when a run mounts that volume, with `unifiedcd_agent_volume_measured_timestamp_seconds` to judge it by; and that `min_free_disk_bytes` of 0 means the check is **disabled**, not that there is no space.

- [ ] **Step 2: Write the alerting guidance**

In `operations.md`, including the aggregation query — an operator alerting on the raw per-volume series gets one alert per Kubernetes replica:

```
min by (agent_name, volume) (unifiedcd_agent_volume_filesystem_free_bytes)
```

Say plainly that `minFreeDisk` defaults to 0, that 0 disables the claim check entirely, and that setting it is what turns the metric into protection rather than only warning.

- [ ] **Step 3: The remaining pages**

`configuration.md`: `minFreeDisk` is published as a metric. Troubleshooting: an entry for "an agent is not claiming" pointing at `unifiedcd_agent_claim_blocked_by_disk`. `volumes.md`: a pointer from the advisory `size` field to the metric that shows the real number.

- [ ] **Step 4: Verify**

Run: `python -m mkdocs build --strict`
Expected: clean. Then `go test ./... -race -count=1`.

- [ ] **Step 5: Commit**
