# Volumes in the Web UI — design

Status: approved in conversation (2026-11-19)

## Why

Operators run build machines with purpose-specific AgentVolumes (Unity
library cache, iOS derived data, Android Gradle cache, compile-check cache).
When a disk fills up they need to answer, without logging into the machine:
"which agent holds which volumes, is each one ready, and how full is the disk
under it?" They also want to watch disks fill up through Prometheus alerts
and Grafana trend graphs.

Today none of this is visible in the Web UI:

- `GET /api/v1/agents` carries `volumeStatus` (name, ready, reason), but the
  UI does not render it.
- The filesystem free/total readings exist only as Prometheus gauges.

### Per-volume usage on host agents: rejected

Measuring each volume's used size on host agents was considered and rejected.

- It means walking every file of the volume. Git working trees and Unity
  caches hold huge numbers of files, so the walk loads the agent's disk.
- Jenkins' Disk Usage plugin is the known precedent for that cost.
- Caching schemes were considered (directory mtimes, persisted results,
  paced walks). They reduce the cost but do not remove it, and they miss
  in-place growth.
- For the same reason, the existing synchronous walk a host agent does when
  it releases a volume that has `size:` is removed. Today that walk only
  logs a warning when the volume exceeds `size`, and it delays the release
  of the reservation.

## Goals

1. **Agent detail page: a Volumes panel.**
   - One row per volume the agent was asked to provision.
   - Columns:
     - name
     - mode
     - state (Ready, or Not ready with the reason)
     - used bytes
     - declared `size`
     - filesystem free / total
     - when it was measured
   - Used is known only on Kubernetes agents; elsewhere it shows "—".
   - Used above the declared size shows a warning marker.
2. **AgentVolumes list page** (a new nav entry, "Volumes").
   - One row per declaration.
   - Columns:
     - name
     - mode
     - agentSelector
     - reclaimPolicy
     - agents ready / agents asked
     - total used (Kubernetes readings only)
     - largest holder
   - Clicking a row expands a per-agent breakdown:
     - agent
     - state
     - used
     - filesystem free / total
     - measured
3. **Both pages work on a phone.**
   - Below 640px width each table row renders as a card with "label: value"
     lines.
   - The first cell is the card title, as in the approved mockup.
4. **Kubernetes used bytes are derived and reported** from the in-pod statfs
   reading that already exists. They go into the agents API and into
   Prometheus.
5. **The host agent's release-time usage walk is removed** (`CheckUsage`,
   `measureUsage` and their tests). `size` then has no effect on host agents.
   It still sets the PVC request on Kubernetes agents.

## Non-goals

- No per-volume used size on host agents.
- No history or charts in the Web UI; trends belong to Prometheus/Grafana.
- No delete or clean-up button for volumes in the UI.
- No mobile work on other pages (agent list, jobs, runs).

## Anti-goals

- Nothing added may run new disk work on any agent.
- A missing reading must never render as zero. Absent stays absent end to
  end, which is the existing `AgentStorage` rule.

## Decided design

### Kubernetes agent: used bytes

`measureVolumes` already runs `ucd-sh statfs` inside the run pod and records
free and total bytes per PVC. Each PVC is its own filesystem, so the agent
sets `usedBytes = total - free` on the same reading, with the same
`MeasuredAt`. Nothing new runs.

### Host agent

- **Removed:**
  - The `CheckUsage` call in the release closure
    (`internal/agent/volumes_mount.go`).
  - `VolumeManager.CheckUsage` and `measureUsage`.
  - The `walk` injection field, if nothing else uses it.
  - Their tests.
- **Unchanged:** the filesystem free/total sampling (`storage_sampler.go`).
- Its volumes never report `usedBytes`.

### Wire, storage, API

- `api.VolumeStats` gains `usedBytes` (`*uint64`, `omitempty`).
  - Host agents and old agents omit it.
  - It travels inside the existing heartbeat `storage` and is persisted in
    the existing `agents.storage` JSON column. No migration.
- `api.AgentInfo` gains `storage` (`*api.AgentStorage`, `omitempty`).
  - `AgentInfo` is the item of `GET /api/v1/agents` and of
    `GET /api/v1/agents/{id}`.
  - `ListAgents` and `GetAgent` must read the `agents.storage` column. Today
    they do not.
  - No stale filter is applied. The permission is unchanged (`agent.read`).
- No new endpoint.
  - The AgentVolumes page reads `GET /api/v1/agent-volumes` (needs
    `agentvolume.read`) and `GET /api/v1/agents` (needs `agent.read`), and
    joins them in the browser.
  - A viewer without `agent.read` sees the declaration columns only. The
    per-agent columns and the expand are hidden.
- In the list page counts:
  - **Agents asked** are agents whose `volumeStatus` lists the volume, i.e.
    the controller delivered it to them.
  - **Ready** are the agents among them with `ready: true`. Agent IDs are
    counted individually.

### Kubernetes replicas share PVCs

Replicas of one Kubernetes agent each enrol under their own agent ID but
share PVCs named `<agentName>-<volume>`; this is why `AgentStorage.AgentName`
exists.

- Total used and largest holder de-duplicate readings by
  `(storage.agentName, volume)` for Kubernetes agents only.
  - Use whatever signal the codebase already has to tell a Kubernetes agent
    apart; the builder picks it.
  - When replicas disagree, the reading with the latest `measuredAt` wins.
- Host agents never collapse.
- The expanded breakdown lists every replica.

### Offline agents

- The UI marks agents offline by `lastSeenAt`, using the existing 90 s rule.
  Offline agents get an "Offline" badge and dimmed rows.
- Their readings still count in totals, because the data is still on their
  disk.
- Metrics keep the existing stale filter.

### Aggregates

- Total used sums the de-duplicated Kubernetes readings.
- When some asked agents have no used reading (host agents, or Kubernetes
  agents that have not measured yet), the cell adds "(k of n measured)".
- With no readings at all, it shows "—".

### Prometheus

- New gauge, emitted only when the agent reported the value:
  `unifiedcd_agent_volume_used_bytes{agent, agent_name, volume}`.
- Its time is the existing `unifiedcd_agent_volume_measured_timestamp_seconds`.

### Web UI

- Svelte, following existing pages and the `app.css` tokens.
- **Agent detail** (`web/src/routes/AgentDetail.svelte`):
  - A "Volumes" card after Labels.
  - Rows come from `volumeStatus` only. `AgentInfo.Volumes` is the legacy
    config list from pre-v0.16 agents and is ignored.
  - `storage.volumes` entries are joined onto those rows by name.
  - Mode and size come from the matching AgentVolume declaration when the
    viewer has `agentvolume.read`; otherwise both show "—".
- **New route** `#/volumes`, in a new file
  `web/src/routes/AgentVolumeList.svelte`:
  - The nav link "Volumes" appears when
    `allows($grants, 'agentvolume.read')`, following the existing nav
    pattern in `App.svelte`.
  - Agent names link to the agent detail page.
- **Formatting and markers:**
  - Bytes use binary units (KiB, MiB, GiB, TiB) with one decimal.
  - Relative times use the existing `fmtRelative`.
  - The over-size marker shows when `usedBytes > size` and the declaration
    has a size.
  - Disk free shows in the failure colour when `claimBlocked` is true.
- **Phone layout** (`max-width: 640px` media query):
  - Hide `thead`.
  - Show each `tr` as a card.
  - Give each `td` its label from a `data-label` attribute.
  - Make the first cell the card title.

## Left to the builder

- **Test layout.** At least:
  - Kubernetes used = total − free.
  - The release path no longer walks the volume.
  - Absent `usedBytes` stays absent through the store, the API and the
    metrics.
  - `storage` appears in the agent list and get responses.
  - The metric is emitted.
  - UI tests for both pages, including the case without `agent.read`, the
    de-duplication of Kubernetes replicas, and "(k of n measured)".
- **Documentation.** Update every page that describes volume usage, `size`,
  agent storage metrics or the agents API. At least:
  - `docs/user-guide/resources/agent-volume.md`: `size` is now Kubernetes-only
    and has no effect on host agents; the usage warning is gone.
  - `docs/user-guide/writing-jobs/volumes.md`
  - `docs/operator-manual/operations.md` (metrics list)
  - `docs/operator-manual/kubernetes-integration.md`
  - `docs/reference/configuration.md`, if it mentions `size`
  - The Web UI section, if one exists
- **Breaking change.** Removing the host size warning is a behaviour change
  but not a breaking one. Note it in the PR body; it needs no migration
  guide.
