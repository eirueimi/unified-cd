# Service Sidecar Log Visibility

**Status**: Draft for review
**Date**: 2026-07-13

## Problem

A podTemplate service sidecar (mysql, redis, a tool container, …) runs its own
long-lived process, but unified-cd captures only **step** output — the stdout/stderr
of scripts the agent execs into a container, keyed by step index. A sidecar's own
entrypoint output (mysqld startup, a crash on boot, a config error) is never
captured on either backend:

- **host**: sidecars are `docker/podman run -d` containers; the `ContainerRuntime`
  interface has no log-reading method, and the agent never reads their streams.
- **k8s**: sidecars are pod containers; the codebase uses only `pods/exec`, never
  `pods/log` (`GetLogs`).

To debug a sidecar that fails to start you must reach the underlying runtime
(`docker logs` / `kubectl logs -c`), which the run view cannot show — and on host
the containers are anonymous (no `--name`), so they are hard to even find. After
teardown the logs are gone entirely.

Goal: stream each **user-defined podTemplate sidecar's** own stdout/stderr into the
same run log store as step logs, always-on, on both backends, and surface it in the
run detail UI as a per-sidecar filterable entry with the container's live/terminal
status (including exit code).

## Goals / non-goals

**Goals**
- Capture user podTemplate sidecar container stdout/stderr for the run's lifetime,
  stored in the existing `logs` table (survives pod/container teardown).
- Both backends (host + k8s), at parity.
- Surface per-sidecar in the run detail UI: a "Sidecars" group, click-to-filter,
  status dot + `running` / `exited N`.
- Fix the existing inconsistency where the injected artifact sidecar's exec output
  ships on step index 0.

**Non-goals**
- Capturing internal containers' own process output: the injected `unified-artifact`
  sidecar's *idle* process, the pause container, the shim init container, and the
  primary `job` container (which already has step logs). Only **user-declared**
  podTemplate sidecars are streamed. (The artifact sidecar's *exec* output is
  re-attributed — see Fix below — but its idle process is not streamed.)
- Per-sidecar log volume caps: sidecar logs are treated exactly like step logs,
  which are uncapped and handled by the existing batch-ship / archive / windowed-read
  pipeline. No new limiting.
- Making a non-zero sidecar exit fail the run: a sidecar is a user-owned service,
  independent of step success. Its exit is reported for visibility only.
- Any DB schema change: the `logs.step_index` column is a bare `integer NOT NULL`
  with no FK/CHECK, so sentinel indices are storable today with no migration.

## Design

### Sentinel step_index scheme

`logs.step_index` already carries non-step sentinels: real steps use the flat range
`[0, N)` (steps + `finally`, assigned by the controller at claim-build), and `-1`
is "System" (run-level messages, e.g. claim-construction failures, rendered as
"System" in the UI). Sidecar logs get their own dedicated, high, positive range so
they collide with neither and read as obviously synthetic:

| step_index | source |
|---|---|
| `0 … N-1` | real steps (steps + finally) |
| `-1` | System (existing) |
| `artifactLogIndex = 90000` | injected artifact sidecar's exec output (moved off step 0) |
| `sidecarLogIndexBase + k = 100000 + k` | k-th user podTemplate sidecar (declared order, primary `job` excluded), k = 0,1,… |

High-positive (not negative) is deliberate: `-1` already means System, `steps=-100`
in a URL query is awkward, and any latent `step_index >= -1` assumption stays safe.
`100000` is far above any real step count; `90000…99999` is reserved headroom for
future internal sources.

**Determinism without a handshake.** The index is a pure function of the sidecar's
ordinal in `podTemplate.spec.containers` (declared order, skipping the primary
`job`). Both the agent (which ships logs) and the controller (which renders the UI)
read the same podTemplate and compute the same `100000 + k`, so no mapping needs to
be exchanged. Indices are per-run (logs are keyed by `run_id`), so cross-run
stability is irrelevant; within a run both sides see the same container list.
Duplicate container names are NOT a hard error: the host `claimContainerDefs`
warns and drops the duplicate (keeping the first definition) rather than
rejecting the podTemplate, and this is a malformed-input path, not something
either backend validates against. What keeps name↔ordinal unambiguous despite
that is the shared-helper enumeration described below — both sides walk the
same un-deduplicated `dsl.SidecarContainerNames(pt)` list, so a duplicate name
still yields the same two ordinals on both sides (agent and controller agree,
even though only one container actually runs).

A single shared helper computes the mapping so agent and controller cannot drift:

```
// dsl (or a shared package): the sidecar log index for the k-th non-"job"
// container in a podTemplate, and the reserved artifact-sidecar index.
SidecarLogIndex(ordinal int) int   // returns 100000 + ordinal
ArtifactLogIndex() int             // returns 90000
```

### Agent: log tap (both backends)

For each user podTemplate sidecar, tap stdout+stderr and stream line-buffered into a
`LogPusher(runID, sidecarLogIndex, stream)` — the same shipping path step logs use.
The bulk-ingest endpoint trusts each line's body `stepIndex`, so no new endpoint is
needed. All streaming is **best-effort**: a stream that errors or a container that
vanishes logs a warning and never fails the run; teardown cancels every stream.

**Host** (`internal/agent/claim_pod.go`, `internal/runtime`):
- Add `Logs(ctx context.Context, h ContainerHandle, follow bool) (io.ReadCloser, error)`
  to the `ContainerRuntime` interface, implemented by shelling out to
  `<bin> logs -f <id>` (docker/podman/nerdctl) and by the Apple driver. The fake
  runtime implements it for unit tests.
- In `claimPodManager.Start`, right after a non-primary container is created and
  stored in `m.open`, compute its `sidecarLogIndex` from its ordinal and spawn a
  goroutine: `Logs(ctx, h, follow=true)` → split into lines → two `LogPusher`s
  (stdout/stderr). Track the goroutines/streams on the manager.
- In `closeAllLocked`, cancel every sidecar log stream (close the `ReadCloser`,
  cancel the context) before removing containers, and record each sidecar's exit
  code (`docker wait <id>` or `inspect '{{.State.ExitCode}}'`) for the status report.

**K8s** (`internal/k8sagent`):
- Add a pod-log streamer using
  `client.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: name, Follow: true}).Stream(ctx)`.
- Start one goroutine per user sidecar once the claim pod is Running (the same point
  the agent begins running steps), each feeding a `LogPusher` at the sidecar's index.
  k8s merges a container's stdout+stderr into one stream; ship it as `stdout`.
- Cancel streams at pod teardown. Read each sidecar's exit code from
  `pod.Status.ContainerStatuses[].State.Terminated.ExitCode`.

### Artifact-sidecar step-0 fix

The injected artifact/cache sidecar's `unified-sidecar cache/artifact …` exec output
currently ships on `stepIndex 0` (documented as a stopgap in
`internal/k8sagent/backend.go`), polluting the first real step's log stream. Re-point
those `LogPusher`s to `ArtifactLogIndex()` (`90000`) so the artifact sidecar has its
own identity, consistent with the user-sidecar scheme. The controller renders it as a
sidecar entry (name: `artifact`) so this output is discoverable rather than buried in
step 0. Only the artifact sidecar's exec output is re-attributed this way; its idle
process is not separately streamed (per non-goals).

### Status / exit code reporting

Each sidecar has a status the UI shows as a dot + label: `running` while the run is
live, `exited N` once the container terminates. The agent already owns the container
lifecycle, so it reports a lightweight per-sidecar status update
`{name, ordinal, phase, exitCode}` to the controller (a small agent→controller call,
reusing the existing agent client; exact endpoint pinned in the plan). Exit code:
host via `docker wait`/`inspect`, k8s via `ContainerStatuses[].State.Terminated`.
A non-zero exit is shown with a danger-tinted dot but does **not** fail the run.

### Controller: sidecars as pseudo-steps

The controller exposes each user sidecar (and the artifact sidecar) as a pseudo-step
in the run's steps API response, shaped like a `StepReport` but with
`kind: "sidecar"`, the sidecar `name`, its sentinel `index`, and its reported
`status`/`exitCode`. It derives name+index from the run's stored podTemplate (same
`SidecarLogIndex` helper); it merges in status from the agent's reports. No new
storage table is required for names (derived), only the status is persisted/updated.

### UI: RunDetail "Sidecars" group

`web/src/routes/RunDetail.svelte`:
- Render a distinct "Sidecars" group in the step sidebar from the pseudo-step
  entries (accent-bordered, separate from "Steps"). Each row: status dot
  (running/exited), sidecar name, and an `exited N` / `sidecar` chip. Clicking sets
  `logView.steps = [sidecarIndex]`, filtering the existing windowed log viewer to
  that sidecar — reusing search, SSE, archive, and `/logs/range` unchanged.
- Extend `stepName(idx)` to resolve sidecar sentinel indices to the sidecar name, so
  the "All Steps" view prefixes sidecar lines with `mysql │ …` (today it already
  falls back to `"step " + idx` for unknown indices — this makes it a real name).
- The artifact sidecar entry: shown in the Sidecars group like the rest (name
  `artifact`), so its exec output is discoverable rather than hidden in step 0.

### Data flow

```
sidecar container stdout/stderr
  → agent tap (host: `logs -f` ReadCloser / k8s: GetLogs Stream)
  → line split → LogPusher(runID, 100000+k, stream)
  → POST /agents/{id}/runs/{run}/steps/{i}/logs/bulk  (body stepIndex = 100000+k)
  → store.AppendLog → logs table + pg_notify
  → read: /runs/{id}/logs/range?steps=100000+k  (+ SSE, search, archive)
  → UI: Sidecars group row → filter by that index

agent → controller: sidecar status {name, ordinal, phase, exitCode}
  → controller merges into steps API pseudo-step entries → UI status dot
```

## Error handling

- A sidecar log stream that fails to open or drops mid-run: log a warning, do not
  fail the run, do not retry aggressively (a crashed sidecar's stream simply ends;
  its buffered output is already stored).
- Teardown must cancel every sidecar stream goroutine and close every `ReadCloser`
  to avoid leaked goroutines / hung `docker logs -f` subprocesses. This mirrors the
  pause-container cleanup discipline in `claimPodManager.closeAllLocked`.
- Status reporting failures are non-fatal (the UI just shows the last known status,
  defaulting to `running`).

## Testing

- **Unit**: `SidecarLogIndex`/`ArtifactLogIndex` determinism and non-collision with
  `[0,N)`/`-1`; controller pseudo-step synthesis (names, indices, merged status);
  UI `stepName` rendering of sentinel indices; fake-runtime `Logs` wiring.
- **Host docker-gated integration** (`//go:build integration`, `runtime.Detect`
  skip, mirroring the existing `TestClaimPod_Integration_*` harness): a podTemplate
  sidecar whose entrypoint prints known lines to stdout → assert those lines land in
  the run log store at `100000+k`; a sidecar that exits non-zero → assert the
  reported `exited N` status and that the run still succeeds.
- **K8s integration** (kind, as the existing k8s CI job): a sidecar container's own
  output is streamed to the store via `GetLogs`; exit code surfaced from
  `ContainerStatuses`.
- **Corpus/examples**: none required (no new example run: scripts); if an example is
  added to demonstrate the feature, update the `internal/shim` corpus trip-wire.

## Compatibility

No schema migration (sentinel indices reuse the existing `logs.step_index`). No wire
break: the bulk-ingest endpoint already trusts the body `stepIndex`. Older UIs
render sidecar lines via the existing `"step " + idx` fallback (degraded but not
broken) until the Sidecars-group change ships. The artifact-sidecar re-point from
step 0 to `90000` is a behavior change to internal log attribution only — documented,
no user action.
