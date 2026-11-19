# Volumes in the Web UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show which agent holds which AgentVolumes, their state and disk headroom (and Kubernetes used bytes) in the Web UI, phone-friendly, and drop the host agent's release-time usage walk.

**Architecture:**
- Kubernetes agents derive `usedBytes = total - free` from their existing per-PVC statfs reading.
- The controller returns the stored `agents.storage` reading on the agents API and exports a used-bytes gauge.
- The Svelte UI joins `/api/v1/agents` with `/api/v1/agent-volumes` in the browser: a Volumes card on the agent detail page and a new `#/volumes` list page.
- The host agent's synchronous `CheckUsage` walk is deleted.

**Tech Stack:** Go (controller, agents, pgx store, Prometheus client), Svelte 4 + svelte-spa-router + vitest/@testing-library/svelte.

**Spec:** `docs/superpowers/specs/2026-11-19-volume-usage-web-design.md`

## Global Constraints

- All code, comments, docs and commit messages are in English. No PII; use `/path/to/unified-cd`-style placeholders.
- Absent stays absent end to end:
  - A missing `usedBytes` or `storage` is omitted (`omitempty`, pointer), never zero.
  - The UI renders "—" for it, never "0".
- No new disk work on any agent. The host agent never reports `usedBytes`.
- No DB migration: `usedBytes` lives inside the existing `agents.storage` JSON column.
- No new HTTP endpoint and no new permission. The agents API stays `agent.read`; the declarations API stays `agentvolume.read`.
- Kubernetes agents are recognised by `AgentInfo.OS` ending in `/k8s` (set in `internal/k8sagent/agent.go`: `runtime.GOOS + "/k8s"`).
- The online rule is the existing one: `Date.now() - lastSeenAt < 90000` ms.
- Phone layout: `@media (max-width: 640px)`.
  - `thead` is hidden and each `tr` is a card.
  - Each `td` shows `attr(data-label)` as its label.
  - The first cell is the card title.
- Byte formatting: binary units `B, KiB, MiB, GiB, TiB, PiB` with one decimal (`1.5 GiB`); bytes below 1024 print as an integer (`512 B`).
- Commit with `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`. Every commit message ends with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV
  ```

## Review Focus

1. **Kubernetes reading with `free > total`.** A corrupt or racing statfs reading can report free above total; `usedBytes` must then be absent, not an underflowed huge `uint64` (Task 1 test).
2. **Agent with `volumeStatus` but `storage` NULL** (an old or just-restarted agent). Its rows render with state and "—" for every reading, and nothing crashes (Task 3 test).
3. **Declaration `size` in decimal or suffix forms** (`200Gi`, `1.5Ti`, `500G`, `1000000`). The over-size comparison must parse them; an unparseable size shows no marker, not a false one (Task 3 test).
4. **Volume declared but delivered to no agent.** The list row shows `0 / 0` and "—", and expanding shows an empty-state line (Task 4 test).
5. **A viewer with `agentvolume.read` but without `agent.read`**, and the reverse:
   - The list page must not call `/api/v1/agents` and must hide the per-agent columns.
   - The detail page must show "—" for mode and size without calling `/api/v1/agent-volumes`.
   - Tests: Task 3 and Task 4.

---

### Task 1: Controller and Kubernetes agent report storage and used bytes

**Files:**
- Modify: `internal/api/types.go` (`VolumeStats`, `AgentInfo`)
- Modify: `internal/store/postgres.go:1837-1886` (`ListAgents`, `GetAgent`)
- Modify: `internal/k8sagent/volumes.go:728` (`storageReading`)
- Modify: `internal/metrics/collector.go` (new desc, Describe, Collect loop at ~137)
- Test: `internal/store/agent_storage_test.go`, `internal/k8sagent/volumes_agent_test.go`, the existing metrics collector test file in `internal/metrics/`, `internal/controller/api_agent_storage_test.go`
- Docs: `docs/operator-manual/operations.md` (metrics list), `docs/operator-manual/kubernetes-integration.md` (where volume metrics are described)

**Interfaces:**
- Produces:
  - `api.VolumeStats.UsedBytes *uint64` with JSON tag `usedBytes,omitempty`.
  - `api.AgentInfo.Storage *api.AgentStorage` with JSON tag `storage,omitempty`.
  - The metric `unifiedcd_agent_volume_used_bytes{agent, agent_name, volume}`, help text: `"Bytes used on a named volume's own filesystem (total minus free), as last measured by the agent. Reported only by Kubernetes agents, whose volumes are PVCs."`

- [ ] **Step 1: Write failing tests**
  - Store: `TestListAgents_ReturnsStorage` and `TestGetAgent_ReturnsStorage`.
    - Upsert an agent and `SetAgentStorage` with `Volumes{"cache": {FreeBytes:10, TotalBytes:100, UsedBytes:ptr(90), MeasuredAt:t}}`.
    - Assert that `ListAgents` and `GetAgent` return `Storage` equal to it.
  - Store: `TestGetAgent_NullStorageStaysNil`. An agent that never reported has `Storage == nil`.
  - Store: in the existing `TestSetAgentStorage_RoundTripsEveryField`, add `UsedBytes` to the round trip.
  - k8s: `TestStorageReading_DerivesUsedBytes`.
    - `volumeStats.set("c", {FreeBytes: 30, TotalBytes: 100}, now)`, then `storageReading()`.
    - Assert `*Volumes["c"].UsedBytes == 70`.
  - k8s: `TestStorageReading_FreeAboveTotalOmitsUsed`. `{Free: 200, Total: 100}` gives `UsedBytes == nil`.
  - Metrics:
    - `used_bytes` is emitted with labels `(agent, agent_name, volume)` when `UsedBytes` is set.
    - It is absent when `UsedBytes` is nil.
  - Controller: `GET /api/v1/agents/{id}` and `GET /api/v1/agents` JSON include `storage.volumes.<name>.usedBytes` after a heartbeat that carried it. Extend `api_agent_storage_test.go`.
- [ ] **Step 2: Run and see them fail**

  ```bash
  go test ./internal/store/ -run 'Storage|ListAgents|GetAgent'
  go test ./internal/k8sagent/ -run StorageReading
  go test ./internal/metrics/
  go test ./internal/controller/ -run Storage
  ```

  Expected: compile errors or FAIL.
- [ ] **Step 3: Implement**
  - Add the two fields.
  - Add `storage` to both SELECTs and decode it when non-empty. On a decode error, return `list agents: decode storage for agent %q: %w` or `get agent: decode storage: %w`, matching the `volume_status` handling.
  - In `storageReading()`, set `UsedBytes` only when `TotalBytes >= FreeBytes`.
  - Emit the gauge inside the existing `MeasuredAt`-guarded loop.
- [ ] **Step 4: Run the Step 2 commands.** Expected: PASS. Then run `go vet ./...`.
- [ ] **Step 5: Update the docs.**
  - Add the new metric next to the other `unifiedcd_agent_volume_*` entries in `docs/operator-manual/operations.md`.
  - Mention it where `kubernetes-integration.md` lists volume metrics.
  - Check with `grep -rn "volume_filesystem_free_bytes" docs/`, which lists every place to touch.
- [ ] **Step 6: Commit** with the message `feat: report agent storage on the agents API and Kubernetes volume used bytes`.

### Task 2: Remove the host agent's release-time usage walk

**Files:**
- Modify: `internal/agent/volumes_mount.go:643-651` (release closure)
- Modify: `internal/agent/volumes.go:95-97,143,833-910` (`walk` field, `CheckUsage`, `measureUsage`)
- Modify: `internal/agent/volumes_test.go:~195-245,~439` (tests of the removed functions)
- Docs:
  - `docs/user-guide/resources/agent-volume.md`
  - `docs/user-guide/writing-jobs/volumes.md`
  - any other hit of `grep -rniE "advisory|exceeds its advisory|size is not enforced" docs/ README.md examples/`
  - the `Size` doc comment in `internal/dsl/agentvolume_types.go`, then `go generate ./...`

**Interfaces:**
- Consumes: none.
- Produces: the release closure only calls `release()`.

- [ ] **Step 1: Delete** the measuring loop in the release closure, so that it returns `func() { release() }` or simply `release`.
- [ ] **Step 2: Delete `CheckUsage`, `measureUsage` and the `walk` field** with its initialiser, if nothing else references it (`grep -rn "\.walk\b\|walk:" internal/agent`). Also delete their tests and imports that become unused.
- [ ] **Step 3: Add `TestReleaseDoesNotWalkVolume`** in `internal/agent/volumes_test.go`, or wherever the release closure is already tested.
  - Make a volume directory unreadable for its children, or build a large tree.
  - Assert that the release closure returns and that `TakeWarnings` or the logs carry no usage warning.
  - If no hook makes that observable, assert instead that `volumes_mount.go` no longer references any usage measurement, by making the test compile only without `CheckUsage`. Pick the observable form if one exists.
- [ ] **Step 4: Run tests and build.** Expected: PASS.

  ```bash
  go test ./internal/agent/ -run Volume
  go build ./...
  ```
- [ ] **Step 5: Update the docs** to say that `size` sets the PVC request on Kubernetes agents and has no effect on host agents, and that no usage warning is logged.
  - Update the `Size` field comment in `internal/dsl/agentvolume_types.go` the same way.
  - Run `go generate ./...` and commit the generated `docs/reference/field-reference.md` / `schemas/unified-cd.schema.json` diff.
  - The `grep` in Files must return zero stale hits outside `docs/superpowers/`.
- [ ] **Step 6: Commit** with the message `refactor(agent): drop the release-time volume usage walk`.

### Task 3: Agent detail Volumes card and shared UI helpers

**Files:**
- Create: `web/src/lib/volumes.js`
- Create: `web/src/lib/volumes.test.js`
- Modify: `web/src/routes/AgentDetail.svelte` (the card goes after Labels)
- Modify: `web/src/app.css` (the phone-card table class, shared by Task 4)
- Test: `web/src/routes/AgentDetail.test.js` (create)

**Interfaces:**
- Produces, from `web/src/lib/volumes.js`:
  - `fmtBytes(n: number|null|undefined): string`. It returns `'—'` for null or undefined.
  - `parseQuantity(q: string): number|null`. It handles the Kubernetes suffixes `Ki Mi Gi Ti Pi Ei` (×1024ⁿ), `k M G T P E` (×1000ⁿ), plain or decimal numbers, and returns `null` when unparseable.
  - `isK8sAgent(agent): boolean`, true when `agent.os` ends with `'/k8s'`.
  - `isOnline(lastSeenAt): boolean`, using the 90 s rule.
  - `volumeRows(agent, declsByName): Array<{name, mode, size, ready, reason, usedBytes, freeBytes, totalBytes, measuredAt, claimBlocked, overSize}>`.
    - There is one row per `agent.volumeStatus` entry.
    - Each row joins `agent.storage?.volumes?.[name]` and `declsByName[name]?.spec`.
    - Missing values are `null`.
    - `overSize` is `usedBytes != null && parseQuantity(size) != null && usedBytes > parseQuantity(size)`.
  - CSS class `table.cards` in `app.css`. On phones it applies the card layout from Global Constraints. Cells carry `data-label`.

- [ ] **Step 1: Write failing unit tests** in `volumes.test.js`:
  - `fmtBytes(512) === '512 B'`
  - `fmtBytes(1536) === '1.5 KiB'`
  - `fmtBytes(200 * 2**30) === '200.0 GiB'`
  - `fmtBytes(null) === '—'`
  - `parseQuantity('200Gi') === 200 * 2**30`
  - `parseQuantity('1.5Ti') === 1.5 * 2**40`
  - `parseQuantity('500G') === 5e11`
  - `parseQuantity('1000000') === 1e6`
  - `parseQuantity('abc') === null`
  - `volumeRows` with `storage: null` returns rows whose reading fields are all `null`.
  - `volumeRows` with used `250 GiB` and size `'200Gi'` sets `overSize === true`.
  - With size `'bogus'`, `overSize === false`.
  - `isK8sAgent({os: 'linux/k8s'})` is true; `isK8sAgent({os: 'linux'})` is false.
- [ ] **Step 2: Write failing component tests** in `AgentDetail.test.js`, mocking `fetch` per URL in the style of `AppSourceList.test.js`:
  - With `agent.read` and `agentvolume.read`:
    - The card titled "Volumes" lists each `volumeStatus` name.
    - It shows "Ready" and "Not ready" with the reason text.
    - It shows the mode and size from the declaration.
    - Free and total appear as `41.0 GiB / 1.0 TiB`.
  - A host agent's used cell shows "—". A k8s agent's (os `linux/k8s`) shows the formatted used value.
  - The over-size row shows "⚠".
  - With only `agent.read`, `/api/v1/agent-volumes` is never fetched, and mode and size show "—".
  - An agent with no `volumeStatus` renders no Volumes card.
  - `claimBlocked: true` gives the free cell the failure-colour class.
- [ ] **Step 3: Run and see them fail.** Expected: FAIL.

  ```bash
  cd web
  npx vitest run src/lib/volumes.test.js src/routes/AgentDetail.test.js
  ```
- [ ] **Step 4: Implement** the helpers, the CSS and the card.
  - Use `allows($grants, 'agentvolume.read')` from the same module `App.svelte` uses to gate the declarations fetch.
  - A failure of that fetch leaves mode and size as "—"; it is not a page error.
  - Columns: Volume, Mode, State, Used, Size, Disk free, Measured. Measured uses `fmtRelative`.
- [ ] **Step 5: Run the Step 3 command, then the full suite.** Expected: PASS.

  ```bash
  cd web
  npx vitest run
  npm run build
  ```
- [ ] **Step 6: Commit** with the message `feat(web): show an agent's volumes on its detail page`.

### Task 4: AgentVolumes list page

**Files:**
- Create: `web/src/routes/AgentVolumeList.svelte`
- Create: `web/src/routes/AgentVolumeList.test.js`
- Modify: `web/src/lib/volumes.js` (add `aggregateVolume`) and `web/src/lib/volumes.test.js`
- Modify: `web/src/App.svelte` (route `"/volumes": AgentVolumeList`, and the nav link `Volumes` after Agents, gated on `allows($grants, 'agentvolume.read')`)
- Docs: the Web UI page under `docs/` if one exists (`grep -rln "Web UI" docs/user-guide docs/operator-manual`), and `docs/user-guide/resources/agent-volume.md` (a short "Seeing volumes in the Web UI" paragraph covering both pages and that used size is Kubernetes-only)

**Interfaces:**
- Consumes: `fmtBytes`, `isK8sAgent`, `isOnline` and `table.cards` from Task 3.
- Produces: `aggregateVolume(name, agents): {asked: number, ready: number, measured: number, totalUsed: number|null, largest: {agentId, usedBytes}|null, perAgent: Array<{agentId, online, ready, reason, usedBytes, freeBytes, totalBytes, measuredAt, claimBlocked}>}`.
  - `asked` counts agents whose `volumeStatus` lists `name`, by individual agent ID.
  - `ready` counts those with `ready: true`.
  - `totalUsed` and `largest` come from readings that have `usedBytes`.
  - For k8s agents, readings are first de-duplicated by `(agent.storage.agentName, name)`, keeping the latest `measuredAt`.
  - `measured` is the number of asked agents with a used reading.
  - `totalUsed` is `null` when `measured === 0`.
  - `perAgent` lists every asked agent, replicas included.

- [ ] **Step 1: Write failing unit tests for `aggregateVolume`:**
  - **Two k8s replicas share a PVC.** Both have `os: 'linux/k8s'` and `storage.agentName: 'builders'`. Their used values are 10 GiB measured at t1 and 12 GiB measured at t2 > t1. Expect `totalUsed === 12 GiB`, `asked === 2` and `perAgent.length === 2`.
  - **Two host agents with the same hostname** do not collapse (both `usedBytes` absent): `measured === 0` and `totalUsed === null`.
  - **Mixed: one k8s agent with used, one host agent.** Expect `measured === 1` and `asked === 2`.
  - **No agent lists the volume:** `asked === 0` and `totalUsed === null`.
- [ ] **Step 2: Write failing component tests:**
  - With both permissions:
    - A row shows name, mode, `agentSelector` joined by ", ", reclaimPolicy (default "Retain" when empty), "1 / 2", and total used.
    - With `measured < asked`, the total also shows `(1 of 2 measured)`.
    - The largest holder links to `#/agents/<id>`.
    - Clicking the row reveals the per-agent breakdown.
    - An offline agent (lastSeenAt 10 min ago) shows "Offline".
  - With only `agentvolume.read`:
    - `/api/v1/agents` is never fetched.
    - The Agents, Total used and Largest headers are absent.
    - Rows do not expand.
  - A declaration with no agents shows `0 / 0` and "—"; expanding shows "No agent has been asked to provision this volume."
  - In `App.test.js` (extend the existing file), the `Volumes` nav link appears only with `agentvolume.read`.
- [ ] **Step 3: Run and see them fail.** Expected: FAIL.

  ```bash
  cd web
  npx vitest run src/lib/volumes.test.js src/routes/AgentVolumeList.test.js src/App.test.js
  ```
- [ ] **Step 4: Implement** `aggregateVolume`, the page (using `table.cards` with `data-label` on every cell), the route and the nav link.
- [ ] **Step 5: Run the full suite and the build.** Expected: PASS.

  ```bash
  cd web
  npx vitest run
  npm run build
  ```
  If `web/dist` is committed or embedded (check `git ls-files web/dist | head`), follow the repo's existing practice for rebuilt assets.
- [ ] **Step 6: Update the docs** listed in Files.
- [ ] **Step 7: Commit** with the message `feat(web): add an AgentVolumes page`.
