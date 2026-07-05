# AppSource Multi-Kind GitOps — Design

**Status:** Approved (brainstorming 2026-07-04)
**Branch / worktree:** `feature/appsource-multikind` at `unified-cd-project/unified-cd-appsource-multikind` (base: `main` @ 8be360d)

## Goal

Extend AppSource GitOps sync from **Job-only** to **all five resource kinds**: `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, and `AppSource` itself. Today the reconciler parses every synced file as a Job and silently skips everything else with a warning; this makes GitOps unable to manage schedules, webhooks, or credentials declaratively.

## Non-goals

- **UI hierarchical/directory display** of resources — deferred to a separate spec.
- **Secret values in Git** — `secretRef` fields reference `StoredSecret` names only; the secret values are set out-of-band via `secret set`. AppSource never syncs secret values.
- **Cross-AppSource ownership arbitration** — if two AppSources manage a resource of the same name, the later sync wins (documented, not enforced).
- **Templating / generators** (ArgoCD ApplicationSet-style) — out of scope.

## Background (verified against code)

- Reconciler: `internal/controller/appsource_reconciler.go` — `FetchDir` returns `map[filePath][]byte`; the loop calls `dsl.Parse` (Job parser) → `st.UpsertJob`, tracks `src.ManagedJobs`, prunes via `st.DeleteJob`.
- File enumeration is recursive: `FetchDir` runs `git ls-tree -r --name-only FETCH_HEAD <path>` and keeps `.yaml`/`.yml` files at any depth (`internal/gittemplate/fetch.go`).
- Parsers exist for every kind: `dsl.Parse` (Job), `dsl.ParseSchedule`, `dsl.ParseWebhookReceiver`, `dsl.ParseAppSource`. **GitCredential has no parser** — `internal/cli/apply.go` unmarshals it inline.
- Store upsert/delete exist for every kind: `UpsertJob`/`DeleteJob`, `UpsertSchedule`/`DeleteSchedule`, `UpsertWebhookReceiver`/`DeleteWebhookReceiver`, `UpsertGitCredential`/`DeleteGitCredential`, `UpsertAppSource`/`DeleteAppSource`.
- Tracking: `app_sources.managed_jobs text[]` (migration `001_init`); `store.AppSource.ManagedJobs []string`.
- Current migration high-water mark on `main`: `002` → next is **`003`**.

## Decisions (from brainstorming)

1. **Kinds managed:** Job, Schedule, WebhookReceiver, GitCredential, AppSource (all five).
2. **Nested AppSource:** allowed (ArgoCD "App of Apps" precedent). **Deletion is non-cascading** — pruning an AppSource deletes only its `app_sources` row; the resources that child AppSource managed are left in place (matches ArgoCD's finalizer-less default).
3. **secretRef:** name reference only; values pre-registered out-of-band.
4. **Determinism & collisions:** process files **sorted by path**; on a duplicate `{kind, name}` within one sync, keep the first (lexicographically earliest file) and WARN-skip the rest.
5. **Cross-AppSource overlap:** not enforced; later sync wins; documented.
6. **Prune:** opt-in via `syncPolicy.prune` (unchanged default `false`), extended to all kinds.

## Architecture & Data Flow

Replace the reconciler's file loop with a kind-dispatching flow. The per-kind dispatch is extracted into two small, independently testable functions (not buried in the loop).

```
FetchDir(repoURL, ref, path) → map[filePath]content
        │  sort file paths (deterministic)
        ▼
for each file (sorted):
    kind := probeKind(content)                 // yaml.Unmarshal into struct{ Kind string }
    name, err := applyResource(ctx, st, kind, content)
        Job             → dsl.Parse            → st.UpsertJob(name, apiVersion, specJSON)
        Schedule        → dsl.ParseSchedule    → st.UpsertSchedule(name, cron, jobName, params)
        WebhookReceiver → dsl.ParseWebhookReceiver → st.UpsertWebhookReceiver(name, specJSON)
        GitCredential   → dsl.ParseGitCredential (NEW) → st.UpsertGitCredential(name, host, type, secretRef)
        AppSource       → dsl.ParseAppSource   → st.UpsertAppSource(name, specJSON)
        unknown / parse error → return error
    if err != nil:  WARN + continue            // per-file resilience preserved
    key := {kind, name}
    if key already in currentManaged:  WARN "duplicate", skip
    else: add key to currentManaged

prune (if syncPolicy.prune):
    for each key in prevManaged (DB managed_resources) not in currentManaged:
        deleteResource(ctx, st, key.kind, key.name)
            Job/Schedule/WebhookReceiver/GitCredential → st.Delete<Kind>(name)
            AppSource → st.DeleteAppSource(name)        // non-cascade: row only
        on delete error: WARN + continue
else (prune disabled):
    for each stale key: WARN "removed from Git but still present (set syncPolicy.prune: true)"

persist currentManaged to app_sources.managed_resources
```

### New functions

In `internal/controller/appsource_reconciler.go` (or a sibling file `appsource_apply.go` for testability):

```go
// applyResource parses one synced document by kind and upserts it.
// Returns the resource's metadata.name. Returns an error on unknown kind or parse failure.
func applyResource(ctx context.Context, st Store, kind string, doc []byte) (name string, err error)

// deleteResource removes a previously-managed resource by kind and name.
// For kind "AppSource" this deletes only the app_sources row (non-cascading).
func deleteResource(ctx context.Context, st Store, kind, name string) error
```

In `internal/dsl/gitcredential_parse.go` (NEW, mirroring `schedule_parse.go`):

```go
// ParseGitCredential decodes a GitCredential YAML document.
func ParseGitCredential(r io.Reader) (*GitCredential, error)
```
Validates required fields (`metadata.name`, `spec.host`, `spec.type` ∈ {token, sshKey}, `spec.secretRef`).

## Data Model & Schema

Replace `managed_jobs text[]` with `managed_resources jsonb` holding `[{kind, name}, …]`.

### Migration `003_appsource_managed_resources`

`up.sql`:
```sql
ALTER TABLE app_sources ADD COLUMN managed_resources jsonb NOT NULL DEFAULT '[]'::jsonb;

UPDATE app_sources
SET managed_resources = COALESCE(
  (SELECT jsonb_agg(jsonb_build_object('kind', 'Job', 'name', j)) FROM unnest(managed_jobs) AS j),
  '[]'::jsonb
);

ALTER TABLE app_sources DROP COLUMN managed_jobs;
```

`down.sql` (information-reducing downgrade — keeps only kind=Job entries):
```sql
ALTER TABLE app_sources ADD COLUMN managed_jobs text[] NOT NULL DEFAULT '{}'::text[];

UPDATE app_sources
SET managed_jobs = COALESCE(
  (SELECT array_agg(elem->>'name')
   FROM jsonb_array_elements(managed_resources) AS elem
   WHERE elem->>'kind' = 'Job'),
  '{}'::text[]);

ALTER TABLE app_sources DROP COLUMN managed_resources;
```

golang-migrate is configured with `migpostgres.Config{}` (MultiStatementEnabled=false, `internal/store/postgres.go:54`), so each migration file is sent to Postgres in one `Exec` and runs inside a single implicit transaction — the three statements in `up.sql` (and `down.sql`) either all apply or all roll back. Neither migration adds a validated CHECK against existing rows, avoiding the TODO #017-class rollback failure. The `down` intentionally drops non-Job tracking — acceptable because it reverts a feature, not user data.

### Types & store changes

- New `store.ResourceRef struct { Kind string; Name string }` (JSON tags `kind`, `name`).
- `store.AppSource.ManagedJobs []string` → `ManagedResources []ResourceRef`.
- `internal/store/postgres.go`: every `managed_jobs` reference (`UpsertAppSource` RETURNING, `GetAppSource`, `ListAppSources` SELECT, and the post-sync `UPDATE app_sources SET managed_jobs=$3`) switches to `managed_resources` with jsonb marshal/unmarshal of `[]ResourceRef`.
- The `spec` column (AppSource definition) is unchanged; existing AppSources need no re-apply.

## Error Handling (resilience preserved)

| Event | Behavior |
|---|---|
| Missing/unknown kind, parse failure | WARN + skip that file; sync continues |
| Duplicate `{kind,name}` within a sync | Keep first (sorted), WARN-skip rest |
| `Upsert*` DB error | Abort whole sync, return error (retried next interval) |
| `Delete*` (prune) error | WARN + continue other prunes |
| `FetchDir` error (git unreachable) | Abort whole sync, return error |

Principle: single-file authoring mistakes never break the whole sync; infrastructure errors (DB write, Git fetch) abort to avoid partial application.

## Testing Strategy

**Unit — `applyResource`/`deleteResource`** (fake `Store`): each kind routes to the correct `Upsert*`/`Delete*` and returns the right name; unknown/missing kind and parse failures return errors without panicking.

**Unit — `dsl.ParseGitCredential`** (mirrors `schedule_parse_test.go`): valid YAML → correct `Host`/`Type`/`SecretRef`; missing required fields or invalid `type` → error.

**Unit — reconciler** (fake fetcher, multi-kind directory):
- 5 kinds mixed → each store method called once; `managed_resources` records all `{kind,name}`.
- Deterministic sort; duplicate `{kind,name}` → first wins, rest skipped.
- One unparseable file → skipped, others applied.
- `Upsert*` DB error → sync aborts.
- Prune enabled: each removed kind deleted via its `Delete*`.
- Prune of kind=AppSource → only `DeleteAppSource` called, no cascade into that child's resources.
- Prune disabled: nothing deleted, WARN only.

**Store (PostgreSQL) — `postgres_appsources_test.go`:** `managed_resources` round-trip (`[]ResourceRef` in == out); migration `003` converts an existing `managed_jobs` row to `managed_resources` with kind=Job.

**e2e (light):** in-memory/test fetcher with one Job + one Schedule + one WebhookReceiver → apply → all three registered.

## Documentation

- `docs/resources.md` AppSource section: list all five managed kinds; document non-cascading AppSource deletion; note cross-AppSource overlap is last-writer-wins; note `secretRef` is a name reference (values set via `secret set`). Remove the "Job only" wording.
