# Jobs hierarchical tree (directory-only) — design

**Date:** 2026-07-05
**Status:** Draft (awaiting review)

## Goal

Show jobs in the Web UI as a collapsible **directory tree table**, where the
hierarchy comes straight from the AppSource repository's directory structure.
A job synced from `jobs/team-a/build.yaml` appears under a `team-a` folder as
`build`; a job at the repo root, or one applied directly with `unified-cli
apply`, appears at the top level with no folder.

## Core decision: identity stays a single qualified name

The single most important constraint driving this design: **`jobs.name`
remains the one and only unique identifier for a job, unchanged.** We do NOT
introduce a composite `(path, name)` key. Removing/relaxing the uniqueness of
`name` would be a system-wide identity refactor — `name` is the de-facto
primary key used by run association (`runs.job_name`, `POST /runs {jobName}`,
`GET /runs?jobName=`), schedules (`UpsertSchedule(..., jobName, ...)`),
webhook receivers, AppSource `managed_resources` `(kind, name)`, every job API
URL (`/api/v1/jobs/{name}`), the CLI, and every frontend link. None of that
changes.

Instead, hierarchy is expressed as a **qualified name**: the directory path and
the short name joined with `/` into a single string that is stored in the
existing unique `name` column.

- `jobs/team-a/build.yaml` (short name `build`) → qualified name `team-a/build`
- `jobs/team-b/edge/test.yaml` (short name `test`) → qualified name `team-b/edge/test`
- `jobs/hello.yaml` (short name `hello-docker`) → qualified name `hello-docker` (root)

`team-a/build` and `team-b/build` are distinct strings, so the same short name
can live in different directories. Two files that resolve to the *same*
qualified name collide exactly as two identically-named jobs collide today
(existing reconcile/upsert semantics are unchanged).

### Explicitly out of scope (non-goals)

- No composite key; no relaxing of the `name` unique constraint.
- No support for two jobs sharing an identical qualified name.
- **No AppSource grouping anywhere** — no AppSource nodes, badges, or
  provenance in the UI or the API. The tree is folders only. This is
  deliberate: keep the AppSource name out of sight so it never leaks into how
  a job is referenced/triggered.
- No per-folder operations (e.g. "run every job in this folder").
- No drag-and-drop reorganization.
- No new `path` database column (path is derived — see below).

## Authoring model

`dsl.Metadata` gains an `annotations` map. A reserved key `path` carries the
directory:

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
  annotations:
    path: team-a        # optional; AppSource fills this from the directory
spec: { ... }
```

```go
type Metadata struct {
    Name        string            `yaml:"name"`
    Labels      map[string]string `yaml:"labels,omitempty"`
    Annotations map[string]string `yaml:"annotations,omitempty"`
}
```

### Qualified-name derivation (single source of truth)

One shared helper computes the qualified name at apply time:

```
effectivePath = trimSlashes(annotations["path"])   // "" if absent
shortName     = trimSlashes(metadata.name)
qualifiedName = effectivePath == "" ? shortName : effectivePath + "/" + shortName
```

- Applied to **both** the direct-apply path (`handleApplyJob`) and the
  AppSource reconcile path, so the rule lives in exactly one place.
- The qualified name is what gets written to `jobs.name`.
- `metadata.name` (short) must not itself be empty. Leading/trailing slashes on
  `path` and `name` are trimmed; internal slashes in `path` are allowed
  (nested directories). A `metadata.name` containing `/` is tolerated and
  treated as additional path segments (last segment = leaf).

### AppSource reconcile fills `path` from the directory

`FetchDir` already returns files keyed by full repo path (e.g.
`jobs/team-a/build.yaml`). During reconcile, for each Job resource:

1. Strip the AppSource `spec.path` prefix → relative path (`team-a/build.yaml`).
2. Take the directory portion → `team-a` (empty for a file directly under
   `spec.path`).
3. **Set** `metadata.annotations["path"]` to that directory before computing
   the qualified name and upserting — the directory is authoritative for
   AppSource-managed jobs, overwriting any `path` written in the file so the
   tree always mirrors the repository layout.

`managed_resources` continues to track `(kind, name)` — `name` is now the
qualified name, so pruning and re-sync compare on the same value with no other
change.

## Data / storage

**No schema migration.** The qualified name lives in the existing `jobs.name`
column (still `UNIQUE`). `path` and the display leaf are *derived* from the
qualified name by splitting on the **last** `/`. Splitting on the last
separator makes the leaf slash-free by construction, so this holds even if a
`metadata.name` contained slashes:

- `path` = everything before the last `/` (`team-b/edge` for
  `team-b/edge/test`; `""` for `hello-docker`)
- `leaf` = everything after the last `/` (`test`; `hello-docker`)

Deriving in the API handler keeps storage untouched and guarantees `path` and
`name` can never drift apart.

## API

`GET /api/v1/jobs` — each element gains two derived, display-only fields:

```go
type Job struct {
    ID         string     `json:"id"`
    Name       string     `json:"name"`        // qualified name, e.g. "team-a/build"
    Path       string     `json:"path"`        // derived directory, "" = root
    Leaf       string     `json:"leaf"`        // derived short name for display
    APIVersion string     `json:"apiVersion"`
    Spec       []byte     `json:"spec"`
    Inputs     []InputDef `json:"inputs,omitempty"`
    UpdatedAt  time.Time  `json:"updatedAt"`
}
```

The tree is assembled **client-side** from the flat list; the API stays a flat
array (no nested tree endpoint).

### Slash-in-name routing

`name` can now contain `/`, which affects only the three **path-based** job
endpoints:

- `GET /api/v1/jobs/{name}`
- `GET /api/v1/jobs/{name}/yaml`
- `DELETE /api/v1/jobs/{name}`

These are changed to a catch-all segment that captures the full qualified name
(including slashes), extracting it via the router's wildcard and stripping the
optional `/yaml` suffix in the handler. Arbitrary nesting depth is supported.

**Unaffected** (name travels in the body or query string, where `/` encodes
transparently): `POST /api/v1/runs` (`jobName` in body), `GET
/api/v1/runs?jobName=...`, schedules, webhooks. This keeps the routing change
small and contained.

## Frontend

`web/src/routes/JobList.svelte`:

- Build a directory tree from the flat `GET /api/v1/jobs` response by splitting
  each job's `path` on `/`. Folders are intermediate nodes; jobs are leaves.
  Root-level jobs (empty `path`) render at the top level with no folder and no
  "Ungrouped" node.
- Collapsible folders (chevron per folder). Collapse state is local component
  state.
- Search/filter matches on the qualified name; folders containing a match
  auto-expand.
- Preserve existing behaviors: running-status badge (the active-runs map is
  keyed by qualified name), the `Runs →` link (to `#/jobs/<qualified-name>`),
  and row-click expansion showing recent runs.

SPA routing (`svelte-spa-router`) for `#/jobs/{name}`, `#/jobs/{name}/run`,
`#/jobs/{name}/yaml` must accept slashes in `{name}` (wildcard segment).
`JobDetail`, `JobRun`, `JobYaml` read `params.name` as the full qualified name;
their fetches already URL-encode the name for query/body use.

## CLI

**No structural change.** The qualified name is passed as the single positional
argument, exactly as a flat name is today:

```
unified-cli run trigger team-a/build --param BRANCH=main
unified-cli run list --job team-a/build
```

`unified-cli jobs list` prints qualified names, which users copy verbatim into
`run trigger`. `run show` prints `Job: team-a/build`. No AppSource name appears
anywhere in the CLI. (`internal/cli/run.go` sends `jobName` in the request
body, so slashes need no special handling.)

## Backward compatibility

Fully compatible **for jobs directly under `spec.path`** (i.e. an empty
relative directory). Existing flat jobs have names with no `/` → derived
`path` is `""`, `leaf` equals the name → they render unchanged at the tree
root under the same qualified name as before. Their runs, schedules,
webhooks, and other name references are untouched. No data migration for
this case.

**This does NOT extend to jobs that already live in AppSource
subdirectories.** A file previously synced from, say, `jobs/team-a/build.yaml`
was stored under the flat name `build` (the directory was ignored pre-feature).
After this change the same file is stored under the qualified name
`team-a/build` — a different `jobs.name` value.

> **UPGRADE NOTE:** On the first reconcile after upgrading, every AppSource
> job that lives in a subdirectory is re-keyed: it is applied under its new
> qualified name (`team-a/build`) and the old flat-named row (`build`) is no
> longer written by that AppSource. If the AppSource has `prune: true`, the
> old flat-named job is deleted on that same sync. If `prune: false`, the old
> flat-named job is left behind as an orphan (no AppSource claims it anymore)
> and must be cleaned up manually. Either way, expect a one-time
> prune/re-create for every nested job, and re-point any Schedules or
> WebhookReceivers that reference the old flat name (e.g. `job: build`) to the
> new qualified name (`job: team-a/build`) *before* or immediately after the
> upgrade, since they are not renamed automatically. Run history rows keyed to
> the old flat name remain in place but are no longer associated with the
> job's new identity going forward.

## Testing

- **Unit — qualified-name helper:** empty path, single-segment path, nested
  path, leading/trailing slash trimming, name-with-slash, empty name rejected.
- **Unit — path/leaf derivation** from a qualified name (root, one level, deep
  nesting).
- **Reconcile:** file at `spec.path` root → empty path; one level deep →
  single-segment path; deep nesting → multi-segment path; in-file `path`
  annotation is overwritten by the directory; prune matches on qualified name.
- **API:** `/jobs` returns correct `path`/`leaf`; `GET`/`DELETE`/`yaml`
  succeed for a slash-containing qualified name via the catch-all route.
- **Frontend:** tree assembly from a flat list with mixed root and nested
  jobs; folder collapse/expand; search auto-expands ancestors; active-run
  badge and `Runs →` link resolve by qualified name.

## Affected files (indicative)

- `internal/dsl/types.go` — `Annotations` on `Metadata`; qualified-name helper.
- `internal/controller/appsource_reconciler.go` — derive `path` from directory.
- `internal/controller/api_jobs.go` / `internal/controller/server.go` — derived
  `path`/`leaf` in responses; catch-all routing for path-based job endpoints.
- `internal/api/types.go` — `Path`/`Leaf` fields on `Job`.
- `web/src/routes/JobList.svelte` — tree rendering.
- `web/src/routes/JobDetail.svelte`, `JobRun.svelte`, `JobYaml.svelte`, router
  config — accept slashes in the name segment.
