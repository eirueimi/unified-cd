# Design: `downloadArtifact.runId` + call-step `ChildRunID` template data

Date: 2026-07-25
Status: Approved

## Problem

A `call:` step returns the child run's output parameters to the parent, but
there is no way for a parent step to download an artifact the child run
uploaded. Artifacts are stored under a run-scoped key
(`artifacts/{runID}/{name}.tar.gz`) and the `downloadArtifact` step always
downloads from the *current* run's ID — the step schema has no field to name
another run, and template data gives the parent no way to learn the child
run's ID in the first place. The docs ("Upload and download files between
jobs within the same or across runs") overstate what the step can do:
cross-run fetch exists only for humans via the API/CLI.

## Decision summary

1. Add an optional, template-expandable `runId:` field to `downloadArtifact`.
   Empty means "current run" — fully backward compatible.
2. Expose the child run ID of a completed `call:` step to templates as
   `{{ .Steps.<step>.ChildRunID }}` (matrix call steps aggregate per
   combination key, same as outputs).
3. No controller/authorization change. `GET /api/v1/runs/{runID}/artifacts/*`
   is already `agentOrServerAuth` with no per-run guard, so any agent can
   already read any run's artifacts; this feature only surfaces that at the
   DSL level. Tightening (e.g. lineage-scoped reads) was considered and
   deliberately deferred to a separate change.

## DSL

```yaml
steps:
  - name: build-app
    call:
      job: build
      with: { tag: "{{ .Params.tag }}" }

  - name: fetch-child-binary
    downloadArtifact:
      name: app-binary
      runId: "{{ .Steps.build-app.ChildRunID }}"   # optional; default = current run
      destDir: artifacts
```

- `DownloadArtifactStep` gains a `RunID` field with yaml tag `runId,omitempty`.
- `dsl.StepData` gains `ChildRunID any`: a plain string for non-matrix call
  steps, a `map[string]string` keyed by combination key (e.g. `linux/amd64`)
  for matrix call steps — mirroring how `Outputs` aggregates.

## Runtime behavior (shared orchestrator)

- On call-step success the orchestrator already holds the child run ID
  (`ExecuteCallStep` returns it); it now stores it in the step's `StepData`
  alongside the child outputs. It is set only on success: on failure
  subsequent steps do not run (and `continueOnError` callers get no
  `ChildRunID`, keeping the v1 surface small).
- `executeDownloadArtifact` expands `runId` with a **restricted** template
  context: `Params`, `Steps`, `Matrix`, `Foreach` only. `Secrets` and
  `Stdout` are excluded because the expanded value is embedded in a URL path
  and appears in logs (same precedent as `call:` param expansion).
- The expanded value must match `^[A-Za-z0-9_-]{1,64}$`. This is validated
  agent-side before any request because the value is spliced into the
  download URL path (host backend) and the sidecar `--run` argument (k8s
  backend); the pattern rules out path traversal (`..`, `/`) and other
  URL-structure characters.
- Expansion failure, validation failure, and download failure (including
  404) all fail the step through the existing `downloadArtifact` failure
  path.

## Backends

Both backends are already parameterized by run ID, so the orchestrator simply
passes the resolved target run ID instead of unconditionally passing the
current run's ID:

- Host: `Client.DownloadArtifact(ctx, runID, name, destDir)` →
  `GET /api/v1/runs/{runID}/artifacts/{name}`.
- Kubernetes: sidecar exec `unified-sidecar artifact download --run <runID>
  --name <name> --dest <dir>`.

No controller change.

## Documentation

- `docs/jobs.md` Artifacts section: document `runId`, replace the misleading
  "across runs" sentence with an accurate description (cross-run download
  requires `runId:`; humans can also use the API/CLI).
- `docs/jobs.md` `call:` section: add a child-artifact fetch example using
  `ChildRunID`.
- `docs/field-reference.md`: add the new field.

## Testing

- `internal/dsl`: parse test for `runId`; template test for
  `{{ .Steps.x.ChildRunID }}` (string) and matrix aggregation
  (`{{ index .Steps.x.ChildRunID "linux/amd64" }}`).
- `internal/agent` (host orchestrator, fake controller):
  - call step success populates `ChildRunID`; a later `downloadArtifact`
    with `runId:` referencing it downloads from the child run's URL.
  - `runId` template expansion failure and pattern-validation failure fail
    the step (no HTTP request made).
  - omitted `runId` still downloads from the current run (regression).
- `internal/k8sagent`: parity test asserting the sidecar argv `--run` value
  is the override when `runId` is set.

## Out of scope (recorded for later)

- Child failure detail propagation (parent only sees
  `finished with status <S>`).
- Parent-cancel → child-cancel propagation (needs agent-side cancel authz).
- Lineage-scoped authorization for agent artifact reads.
- `type: artifact` outputs remain declarative-only.
