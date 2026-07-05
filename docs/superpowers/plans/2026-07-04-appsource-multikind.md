# AppSource Multi-Kind GitOps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Extend AppSource GitOps sync from Job-only to all five resource kinds (Job, Schedule, WebhookReceiver, GitCredential, AppSource), with per-kind prune tracking and deterministic, resilient reconciliation.

**Architecture:** Replace the reconciler's Job-only loop with a kind-dispatching flow backed by two small testable functions (`applyResource`/`deleteResource`) and a new `dsl.ParseGitCredential`. Managed-resource tracking moves from `app_sources.managed_jobs text[]` to `managed_resources jsonb` (`[{kind,name}]`).

**Tech Stack:** Go 1.26+, PostgreSQL (pgx v5), golang-migrate, existing `internal/dsl` parsers and `internal/store` upsert/delete methods.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-04-appsource-multikind-design.md` — every task implements part of it.
- Managed kinds: `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, `AppSource`.
- Nested AppSource deletion is **non-cascading** (delete the row only; never delete that child's managed resources).
- Determinism: process synced files sorted by file path; duplicate `{kind,name}` within one sync → keep first (lexicographically earliest path), WARN-skip the rest.
- Resilience: per-file parse/unknown-kind errors → WARN + skip; `Upsert*` DB error or `FetchDir` error → abort whole sync; `Delete*` (prune) error → WARN + continue.
- Prune stays opt-in (`syncPolicy.prune`, default false).
- `secretRef` is a name reference only; secret values are never synced.
- Next migration number is `003` (base `main` has `001`, `002`).
- Tests use `store.NewTestPostgres(t)` (real Postgres, requires Docker) — matches the existing reconciler/store test pattern; it runs all migrations including `003`. Pure parser tests need no Docker.
- Commit after each task. Do not push. Worktree: `unified-cd-project/unified-cd-appsource-multikind` (branch `feature/appsource-multikind`).

## File Structure

- `internal/store/migrations/003_appsource_managed_resources.{up,down}.sql` — CREATE: schema change.
- `internal/store/store.go` — MODIFY: add `ResourceRef`; `AppSource.ManagedJobs []string` → `ManagedResources []ResourceRef`; change `UpdateAppSourceSyncState` interface signature.
- `internal/store/postgres.go` — MODIFY: 4 AppSource methods to use `managed_resources`.
- `internal/store/postgres_appsources_test.go` — MODIFY: round-trip + migration test.
- `internal/dsl/gitcredential_parse.go` — CREATE: `ParseGitCredential`.
- `internal/dsl/gitcredential_parse_test.go` — CREATE: parser tests.
- `internal/controller/appsource_apply.go` — CREATE: `applyResource`, `deleteResource`, `probeKind`.
- `internal/controller/appsource_apply_test.go` — CREATE: dispatch tests.
- `internal/controller/appsource_reconciler.go` — MODIFY: sync loop → multi-kind dispatch.
- `internal/controller/appsource_reconciler_test.go` — MODIFY: multi-kind + prune + duplicate + non-cascade tests.
- `docs/resources.md` — MODIFY: AppSource section.

---

### Task 1: Data layer — `ResourceRef`, migration 003, store methods

Move managed-resource tracking to `managed_resources jsonb`. Keep reconciler behavior Job-only for now (wired to multi-kind in Task 4) so all existing tests stay green.

**Files:**
- Create: `internal/store/migrations/003_appsource_managed_resources.up.sql`, `.down.sql`
- Modify: `internal/store/store.go`, `internal/store/postgres.go`, `internal/controller/appsource_reconciler.go`
- Test: `internal/store/postgres_appsources_test.go`

**Interfaces:**
- Produces: `store.ResourceRef{ Kind, Name string }` (json tags `kind`,`name`); `store.AppSource.ManagedResources []ResourceRef`; `Store.UpdateAppSourceSyncState(ctx, name, lastCommit string, syncedAt time.Time, managed []ResourceRef) error`.

- [ ] **Step 1: Write the migration up/down**

Create `internal/store/migrations/003_appsource_managed_resources.up.sql`:
```sql
ALTER TABLE app_sources ADD COLUMN managed_resources jsonb NOT NULL DEFAULT '[]'::jsonb;
UPDATE app_sources SET managed_resources = COALESCE(
  (SELECT jsonb_agg(jsonb_build_object('kind', 'Job', 'name', j)) FROM unnest(managed_jobs) AS j),
  '[]'::jsonb);
ALTER TABLE app_sources DROP COLUMN managed_jobs;
```

Create `internal/store/migrations/003_appsource_managed_resources.down.sql`:
```sql
ALTER TABLE app_sources ADD COLUMN managed_jobs text[] NOT NULL DEFAULT '{}'::text[];
UPDATE app_sources SET managed_jobs = COALESCE(
  (SELECT array_agg(elem->>'name') FROM jsonb_array_elements(managed_resources) AS elem
   WHERE elem->>'kind' = 'Job'),
  '{}'::text[]);
ALTER TABLE app_sources DROP COLUMN managed_resources;
```

- [ ] **Step 2: Add `ResourceRef` and update `AppSource` + interface in `store.go`**

In `internal/store/store.go`, replace the `AppSource` struct's `ManagedJobs []string` field and add the type above it:
```go
// ResourceRef identifies a resource managed by an AppSource.
type ResourceRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// AppSource holds a GitOps source definition.
type AppSource struct {
	Name             string
	Spec             []byte
	LastSyncedAt     *time.Time
	LastCommit       string
	ManagedResources []ResourceRef
	UpdatedAt        time.Time
}
```
Change the interface method signature (in the Store interface, `// AppSources` block):
```go
UpdateAppSourceSyncState(ctx context.Context, name, lastCommit string, syncedAt time.Time, managed []ResourceRef) error
```

- [ ] **Step 3: Rewrite the 4 AppSource methods in `postgres.go`**

In `internal/store/postgres.go`, replace `UpsertAppSource`, `GetAppSource`, `ListAppSources`, and `UpdateAppSourceSyncState`. Scan `managed_resources` jsonb into `[]byte` then unmarshal; marshal `[]ResourceRef` to `[]byte` on write (same pgx jsonb pattern as the `spec` column).

```go
func (p *Postgres) UpsertAppSource(ctx context.Context, name string, spec []byte) (*AppSource, error) {
	const q = `
		INSERT INTO app_sources(name, spec)
		VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE
		  SET spec = EXCLUDED.spec, last_commit = '', updated_at = NOW()
		RETURNING name, spec, last_synced_at, last_commit, managed_resources, updated_at`
	var a AppSource
	var mr []byte
	if err := p.pool.QueryRow(ctx, q, name, spec).
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt); err != nil {
		return nil, fmt.Errorf("upsert AppSource: %w", err)
	}
	if err := unmarshalManagedResources(mr, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (p *Postgres) GetAppSource(ctx context.Context, name string) (*AppSource, error) {
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_resources, updated_at FROM app_sources WHERE name = $1`
	var a AppSource
	var mr []byte
	if err := p.pool.QueryRow(ctx, q, name).
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt); err != nil {
		return nil, fmt.Errorf("get AppSource name=%s: %w", name, err)
	}
	if err := unmarshalManagedResources(mr, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (p *Postgres) ListAppSources(ctx context.Context) ([]AppSource, error) {
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_resources, updated_at FROM app_sources ORDER BY name`
	rows, err := p.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppSource
	for rows.Next() {
		var a AppSource
		var mr []byte
		if err := rows.Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt); err != nil {
			return nil, err
		}
		if err := unmarshalManagedResources(mr, &a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateAppSourceSyncState(ctx context.Context, name, lastCommit string, syncedAt time.Time, managed []ResourceRef) error {
	if managed == nil {
		managed = []ResourceRef{}
	}
	data, err := json.Marshal(managed)
	if err != nil {
		return fmt.Errorf("marshal managed resources: %w", err)
	}
	_, err = p.pool.Exec(ctx,
		`UPDATE app_sources SET last_commit = $1, last_synced_at = $2, managed_resources = $3, updated_at = NOW() WHERE name = $4`,
		lastCommit, syncedAt, data, name)
	return err
}

// unmarshalManagedResources decodes the managed_resources jsonb column into a.ManagedResources.
func unmarshalManagedResources(raw []byte, a *AppSource) error {
	if len(raw) == 0 {
		a.ManagedResources = nil
		return nil
	}
	if err := json.Unmarshal(raw, &a.ManagedResources); err != nil {
		return fmt.Errorf("unmarshal managed_resources for %q: %w", a.Name, err)
	}
	return nil
}
```
Confirm `encoding/json` is already imported in `postgres.go` (it is — used elsewhere).

- [ ] **Step 4: Keep the reconciler compiling (Job-only, new schema)**

In `internal/controller/appsource_reconciler.go`, `syncAppSource`: the prune loop and final persist reference `src.ManagedJobs` and pass `managedJobs []string`. Update them to the new type while keeping Job-only behavior:

Replace the prune loop header:
```go
	for _, prev := range src.ManagedJobs {
		if currentJobNames[prev] {
```
with:
```go
	for _, prev := range src.ManagedResources {
		if prev.Kind != "Job" {
			continue
		}
		if currentJobNames[prev.Name] {
```
(the loop body references `prev`; change `st.DeleteJob(ctx, prev)` → `st.DeleteJob(ctx, prev.Name)` and the log fields `"job", prev` → `"job", prev.Name`.)

Replace the final block:
```go
	managedJobs := make([]string, 0, len(currentJobNames))
	for name := range currentJobNames {
		managedJobs = append(managedJobs, name)
	}
	return st.UpdateAppSourceSyncState(ctx, src.Name, headSHA, time.Now(), managedJobs)
```
with:
```go
	managed := make([]store.ResourceRef, 0, len(currentJobNames))
	for name := range currentJobNames {
		managed = append(managed, store.ResourceRef{Kind: "Job", Name: name})
	}
	return st.UpdateAppSourceSyncState(ctx, src.Name, headSHA, time.Now(), managed)
```

- [ ] **Step 5: Write the store round-trip + migration test**

In `internal/store/postgres_appsources_test.go`, add:
```go
func TestAppSource_ManagedResourcesRoundTrip(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	if _, err := pg.UpsertAppSource(ctx, "src1", []byte(`{"repoURL":"https://x/y","targetRevision":"main","path":"jobs"}`)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	want := []ResourceRef{{Kind: "Job", Name: "build"}, {Kind: "Schedule", Name: "nightly"}}
	if err := pg.UpdateAppSourceSyncState(ctx, "src1", "sha1", time.Now(), want); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := pg.GetAppSource(ctx, "src1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reflect.DeepEqual(got.ManagedResources, want) {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got.ManagedResources, want)
	}
}
```
Ensure imports include `context`, `reflect`, `time` (add any missing).

- [ ] **Step 6: Run store tests**

Run: `cd internal/store && go test -run 'TestAppSource' ./...`
Expected: PASS (Docker running). Also run `go build ./...` from the repo root — Expected: clean (reconciler compiles on new types).

- [ ] **Step 7: Commit**
```bash
git add internal/store/ internal/controller/appsource_reconciler.go
git commit -m "feat(store): track AppSource managed resources by kind (managed_resources jsonb)"
```

---

### Task 2: `dsl.ParseGitCredential`

Add the missing parser so the reconciler can handle GitCredential documents symmetrically with the other kinds.

**Files:**
- Create: `internal/dsl/gitcredential_parse.go`, `internal/dsl/gitcredential_parse_test.go`

**Interfaces:**
- Produces: `func ParseGitCredential(r io.Reader) (*GitCredential, error)` — validates `metadata.name`, `spec.host`, `spec.type ∈ {token,sshKey}`, `spec.secretRef`.

- [ ] **Step 1: Write failing tests**

Create `internal/dsl/gitcredential_parse_test.go`:
```go
package dsl

import "strings"
import "testing"

func TestParseGitCredential_Valid(t *testing.T) {
	in := `apiVersion: unified-cd/v1
kind: GitCredential
metadata:
  name: gh
spec:
  host: github.com
  type: token
  secretRef: gh-token`
	gc, err := ParseGitCredential(strings.NewReader(in))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gc.Metadata.Name != "gh" || gc.Spec.Host != "github.com" || gc.Spec.Type != "token" || gc.Spec.SecretRef != "gh-token" {
		t.Fatalf("bad parse: %+v", gc)
	}
}

func TestParseGitCredential_Invalid(t *testing.T) {
	cases := map[string]string{
		"missing name":   "kind: GitCredential\nspec:\n  host: github.com\n  type: token\n  secretRef: s",
		"missing host":   "kind: GitCredential\nmetadata:\n  name: gh\nspec:\n  type: token\n  secretRef: s",
		"bad type":       "kind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: basic\n  secretRef: s",
		"missing secret": "kind: GitCredential\nmetadata:\n  name: gh\nspec:\n  host: github.com\n  type: token",
	}
	for name, in := range cases {
		if _, err := ParseGitCredential(strings.NewReader(in)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd internal/dsl && go test -run TestParseGitCredential ./...`
Expected: FAIL (`undefined: ParseGitCredential`).

- [ ] **Step 3: Implement the parser**

Create `internal/dsl/gitcredential_parse.go`:
```go
package dsl

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseGitCredential decodes and validates a GitCredential YAML document.
func ParseGitCredential(r io.Reader) (*GitCredential, error) {
	var gc GitCredential
	dec := yaml.NewDecoder(r)
	if err := dec.Decode(&gc); err != nil {
		return nil, fmt.Errorf("parse GitCredential: %w", err)
	}
	if gc.Metadata.Name == "" {
		return nil, fmt.Errorf("GitCredential: metadata.name is required")
	}
	if gc.Spec.Host == "" {
		return nil, fmt.Errorf("GitCredential %q: spec.host is required", gc.Metadata.Name)
	}
	if gc.Spec.Type != "token" && gc.Spec.Type != "sshKey" {
		return nil, fmt.Errorf("GitCredential %q: spec.type must be \"token\" or \"sshKey\", got %q", gc.Metadata.Name, gc.Spec.Type)
	}
	if gc.Spec.SecretRef == "" {
		return nil, fmt.Errorf("GitCredential %q: spec.secretRef is required", gc.Metadata.Name)
	}
	return &gc, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd internal/dsl && go test -run TestParseGitCredential ./...`
Expected: PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/dsl/gitcredential_parse.go internal/dsl/gitcredential_parse_test.go
git commit -m "feat(dsl): add ParseGitCredential"
```

---

### Task 3: `applyResource` / `deleteResource` dispatch

Kind dispatch extracted into standalone, testable functions.

**Files:**
- Create: `internal/controller/appsource_apply.go`, `internal/controller/appsource_apply_test.go`

**Interfaces:**
- Consumes: `store.Store` (Upsert*/Delete* methods), `dsl.Parse*`, `store.ResourceRef` (Task 1), `dsl.ParseGitCredential` (Task 2).
- Produces:
  - `func probeKind(doc []byte) string`
  - `func applyResource(ctx context.Context, st store.Store, kind string, doc []byte) (name string, err error)`
  - `func deleteResource(ctx context.Context, st store.Store, kind, name string) error`

- [ ] **Step 1: Write failing tests**

Create `internal/controller/appsource_apply_test.go`:
```go
package controller

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/store"
)

func TestApplyResource_EachKind(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	cases := []struct {
		kind, wantName, doc string
	}{
		{"Job", "j1", "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j1\nspec:\n  agentSelector: [kind:docker]\n  steps:\n    - name: s\n      run: echo hi"},
		{"Schedule", "sc1", "apiVersion: unified-cd/v1\nkind: Schedule\nmetadata:\n  name: sc1\nspec:\n  cron: \"* * * * *\"\n  job: j1"},
		{"WebhookReceiver", "wh1", "apiVersion: unified-cd/v1\nkind: WebhookReceiver\nmetadata:\n  name: wh1\nspec:\n  trigger:\n    job: j1\n  auth:\n    type: none"},
		{"GitCredential", "gc1", "apiVersion: unified-cd/v1\nkind: GitCredential\nmetadata:\n  name: gc1\nspec:\n  host: github.com\n  type: token\n  secretRef: s"},
		{"AppSource", "as1", "apiVersion: unified-cd/v1\nkind: AppSource\nmetadata:\n  name: as1\nspec:\n  repoURL: https://x/y\n  targetRevision: main\n  path: jobs"},
	}
	for _, c := range cases {
		got, err := applyResource(ctx, pg, c.kind, []byte(c.doc))
		if err != nil {
			t.Fatalf("%s: applyResource error: %v", c.kind, err)
		}
		if got != c.wantName {
			t.Errorf("%s: name = %q, want %q", c.kind, got, c.wantName)
		}
	}
}

func TestApplyResource_UnknownAndBad(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	if _, err := applyResource(ctx, pg, "Nope", []byte("kind: Nope")); err == nil {
		t.Error("unknown kind: expected error")
	}
	if _, err := applyResource(ctx, pg, "Job", []byte("kind: Job\nmetadata: {name: x}\nspec: {steps: []}")); err == nil {
		t.Error("invalid Job: expected error")
	}
}

func TestProbeKind(t *testing.T) {
	if k := probeKind([]byte("kind: Schedule\nmetadata: {name: x}")); k != "Schedule" {
		t.Errorf("probeKind = %q, want Schedule", k)
	}
	if k := probeKind([]byte("metadata: {name: x}")); k != "" {
		t.Errorf("probeKind (no kind) = %q, want empty", k)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd internal/controller && go test -run 'TestApplyResource|TestProbeKind' ./...`
Expected: FAIL (`undefined: applyResource`).

- [ ] **Step 3: Implement dispatch**

Create `internal/controller/appsource_apply.go`:
```go
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/eirueimi/unified-cd/internal/store"
)

// errStoreWrite wraps a failure from the store layer. The reconciler aborts the
// whole sync on this (infrastructure failure) but skips the single file on any
// other error (parse failure, unknown kind).
var errStoreWrite = errors.New("store write failed")

// probeKind reads only the top-level "kind" field of a YAML document.
func probeKind(doc []byte) string {
	var probe struct {
		Kind string `yaml:"kind"`
	}
	_ = yaml.Unmarshal(doc, &probe)
	return probe.Kind
}

// applyResource parses one synced document by kind and upserts it, returning metadata.name.
// Parse/unknown-kind failures return a bare error (skippable); store failures are
// wrapped with errStoreWrite (abort). Never panics.
func applyResource(ctx context.Context, st store.Store, kind string, doc []byte) (string, error) {
	switch kind {
	case "Job":
		job, err := dsl.Parse(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		specJSON, err := json.Marshal(job.Spec)
		if err != nil {
			return "", err
		}
		if _, err := st.UpsertJob(ctx, job.Metadata.Name, job.APIVersion, specJSON); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return job.Metadata.Name, nil
	case "Schedule":
		sc, err := dsl.ParseSchedule(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		if _, err := st.UpsertSchedule(ctx, sc.Metadata.Name, sc.Spec.Cron, sc.Spec.Job, sc.Spec.Params); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return sc.Metadata.Name, nil
	case "WebhookReceiver":
		wr, err := dsl.ParseWebhookReceiver(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		specJSON, err := json.Marshal(wr.Spec)
		if err != nil {
			return "", err
		}
		if _, err := st.UpsertWebhookReceiver(ctx, wr.Metadata.Name, specJSON); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return wr.Metadata.Name, nil
	case "GitCredential":
		gc, err := dsl.ParseGitCredential(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		if err := st.UpsertGitCredential(ctx, gc.Metadata.Name, gc.Spec.Host, gc.Spec.Type, gc.Spec.SecretRef); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return gc.Metadata.Name, nil
	case "AppSource":
		as, err := dsl.ParseAppSource(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		specJSON, err := json.Marshal(as.Spec)
		if err != nil {
			return "", err
		}
		if _, err := st.UpsertAppSource(ctx, as.Metadata.Name, specJSON); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return as.Metadata.Name, nil
	default:
		return "", fmt.Errorf("unsupported kind %q", kind)
	}
}

// deleteResource removes a previously-managed resource by kind and name.
// For kind "AppSource" this deletes only the app_sources row (non-cascading).
func deleteResource(ctx context.Context, st store.Store, kind, name string) error {
	switch kind {
	case "Job":
		return st.DeleteJob(ctx, name)
	case "Schedule":
		return st.DeleteSchedule(ctx, name)
	case "WebhookReceiver":
		return st.DeleteWebhookReceiver(ctx, name)
	case "GitCredential":
		return st.DeleteGitCredential(ctx, name)
	case "AppSource":
		return st.DeleteAppSource(ctx, name)
	default:
		return fmt.Errorf("unsupported kind %q", kind)
	}
}
```

- [ ] **Step 4: Run to verify pass**

Run: `cd internal/controller && go test -run 'TestApplyResource|TestProbeKind' ./...`
Expected: PASS.

- [ ] **Step 5: Commit**
```bash
git add internal/controller/appsource_apply.go internal/controller/appsource_apply_test.go
git commit -m "feat(controller): add kind-dispatching applyResource/deleteResource for AppSource"
```

---

### Task 4: Wire the reconciler to multi-kind sync

Replace the Job-only loop in `syncAppSource` with sorted, deterministic, multi-kind dispatch and per-kind prune.

**Files:**
- Modify: `internal/controller/appsource_reconciler.go`
- Test: `internal/controller/appsource_reconciler_test.go`

**Interfaces:**
- Consumes: `applyResource`, `deleteResource`, `probeKind` (Task 3); `store.ResourceRef`, `UpdateAppSourceSyncState([]ResourceRef)` (Task 1).

- [ ] **Step 1: Write failing tests**

In `internal/controller/appsource_reconciler_test.go`, add (adapt the fetcher/store setup to the existing helpers in that file — the existing tests show how to seed an AppSource and drive `reconcileAppSources`):
```go
func TestReconciler_AppliesAllKinds(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	// seed an AppSource (mirror existing tests' seeding of spec + fetcher files)
	files := map[string][]byte{
		"a-job.yaml":      []byte("kind: Job\nmetadata:\n  name: j1\nspec:\n  agentSelector: [kind:docker]\n  steps:\n    - name: s\n      run: echo hi"),
		"b-schedule.yaml": []byte("kind: Schedule\nmetadata:\n  name: sc1\nspec:\n  cron: \"* * * * *\"\n  job: j1"),
		"c-webhook.yaml":  []byte("kind: WebhookReceiver\nmetadata:\n  name: wh1\nspec:\n  trigger:\n    job: j1\n  auth:\n    type: none"),
	}
	seedAppSourceForTest(t, pg, "multi", files) // helper: see Step 3 note
	reconcileAppSources(ctx, pg, newMockFetcher(files, "sha1"), nil)

	if _, err := pg.GetJob(ctx, "j1"); err != nil {
		t.Errorf("job not applied: %v", err)
	}
	if _, err := pg.GetSchedule(ctx, "sc1"); err != nil {
		t.Errorf("schedule not applied: %v", err)
	}
	as, _ := pg.GetAppSource(ctx, "multi")
	if len(as.ManagedResources) != 3 {
		t.Errorf("managed = %+v, want 3 entries", as.ManagedResources)
	}
}

func TestReconciler_PruneNonCascadeAppSource(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	// First sync: parent manages a child AppSource that itself manages a Job.
	child := []byte("kind: AppSource\nmetadata:\n  name: child\nspec:\n  repoURL: https://x/y\n  targetRevision: main\n  path: jobs")
	seedAppSourceForTest(t, pg, "parent-prune", map[string][]byte{"child.yaml": child}) // prune: true in spec
	reconcileAppSources(ctx, pg, newMockFetcher(map[string][]byte{"child.yaml": child}, "sha1"), nil)
	// Give the child a managed Job directly, to prove non-cascade.
	_ = pg.UpdateAppSourceSyncState(ctx, "child", "x", time.Now(), []store.ResourceRef{{Kind: "Job", Name: "orphan"}})
	if _, err := pg.UpsertJob(ctx, "orphan", "unified-cd/v1", []byte(`{"steps":[]}`)); err != nil {
		t.Fatal(err)
	}
	// Second sync: child removed from Git → parent prunes the child AppSource.
	reconcileAppSources(ctx, pg, newMockFetcher(map[string][]byte{}, "sha2"), nil)
	if _, err := pg.GetAppSource(ctx, "child"); err == nil {
		t.Error("child AppSource should be pruned")
	}
	if _, err := pg.GetJob(ctx, "orphan"); err != nil {
		t.Error("non-cascade violated: child's Job must NOT be deleted")
	}
}
```
Note: `seedAppSourceForTest`, `newMockFetcher` should reuse or lightly wrap whatever the existing tests already use (the file already has `mockAppSourceFetcher`). If a seeding helper does not exist, extract one from the existing `TestReconciler_AppliesJobsFromGit` body. For the prune test, the seeded parent's spec must set `syncPolicy.prune: true`.

- [ ] **Step 2: Run to verify failure**

Run: `cd internal/controller && go test -run 'TestReconciler_AppliesAllKinds|TestReconciler_PruneNonCascade' ./...`
Expected: FAIL (multi-kind not wired; schedule/webhook not applied).

- [ ] **Step 3: Rewrite the sync body**

In `internal/controller/appsource_reconciler.go`, add `"errors"` and `"sort"` to imports. Replace the block from `currentJobNames := map[string]bool{}` through the final `return st.UpdateAppSourceSyncState(...)` with:
```go
	// Deterministic order: sort file paths so duplicate {kind,name} resolution is stable.
	paths := make([]string, 0, len(files))
	for fp := range files {
		paths = append(paths, fp)
	}
	sort.Strings(paths)

	current := make([]store.ResourceRef, 0, len(paths))
	seen := map[store.ResourceRef]bool{}
	for _, fp := range paths {
		kind := probeKind(files[fp])
		name, err := applyResource(ctx, st, kind, files[fp])
		if err != nil {
			// Store-write failures abort the whole sync; parse/unknown-kind skip one file.
			if errors.Is(err, errStoreWrite) {
				return fmt.Errorf("apply %s (%s): %w", kind, fp, err)
			}
			slog.Warn("appsource reconciler: skipping file", "name", src.Name, "file", fp, "kind", kind, "error", err)
			continue
		}
		ref := store.ResourceRef{Kind: kind, Name: name}
		if seen[ref] {
			slog.Warn("appsource reconciler: duplicate resource, keeping first", "name", src.Name, "kind", kind, "resource", name, "file", fp)
			continue
		}
		seen[ref] = true
		current = append(current, ref)
	}

	// Prune resources managed previously but absent now.
	for _, prev := range src.ManagedResources {
		if seen[prev] {
			continue
		}
		if spec.SyncPolicy.Prune {
			if err := deleteResource(ctx, st, prev.Kind, prev.Name); err != nil {
				slog.Warn("appsource reconciler: failed to delete resource", "appsource", src.Name, "kind", prev.Kind, "resource", prev.Name, "error", err)
			} else {
				slog.Info("appsource reconciler: deleted resource (prune)", "appsource", src.Name, "kind", prev.Kind, "resource", prev.Name)
			}
		} else {
			slog.Warn("appsource reconciler: resource removed from Git is still present (set syncPolicy.prune: true to delete it)", "appsource", src.Name, "kind", prev.Kind, "resource", prev.Name)
		}
	}

	return st.UpdateAppSourceSyncState(ctx, src.Name, headSHA, time.Now(), current)
```

Error classification uses the `errStoreWrite` sentinel from `applyResource` (Task 3): `errors.Is(err, errStoreWrite)` → abort; any other error → skip one file. No string matching needed.

Remove the now-unused Job-only block added in Task 1 Step 4 (the `currentJobNames` loop, the `src.ManagedResources` Job-filter prune loop, and the old `managed` build) — this rewrite replaces all of it.

- [ ] **Step 4: Run to verify pass**

Run: `cd internal/controller && go test -run 'TestReconciler' ./...`
Expected: PASS (existing Job tests + new multi-kind + non-cascade prune).

- [ ] **Step 5: Full build + vet**

Run: `go build ./... && go vet ./internal/controller/... ./internal/store/... ./internal/dsl/...`
Expected: clean.

- [ ] **Step 6: Commit**
```bash
git add internal/controller/appsource_reconciler.go internal/controller/appsource_reconciler_test.go
git commit -m "feat(controller): AppSource syncs all kinds with deterministic order and per-kind prune"
```

---

### Task 5: Documentation

**Files:**
- Modify: `docs/resources.md` (AppSource section)

- [ ] **Step 1: Update the AppSource docs**

In `docs/resources.md`, in the AppSource section, replace any "only Jobs are applied" wording and add:
- Managed kinds: "AppSource syncs `Job`, `Schedule`, `WebhookReceiver`, `GitCredential`, and `AppSource` documents found (recursively) under `spec.path`. Files of other kinds, or files that fail to parse, are skipped with a per-file warning; the rest of the sync continues."
- Determinism: "Files are processed in sorted path order. If two files declare the same kind and name, the first (lexicographically earliest path) wins and the rest are skipped with a warning."
- Non-cascade: "When `syncPolicy.prune: true`, resources removed from Git are deleted. Pruning a nested `AppSource` removes only that AppSource; the resources it managed are left in place (non-cascading, matching Argo CD's default)."
- Overlap: "Do not manage the same resource from two AppSources — the last sync wins."
- Secrets: "`secretRef` fields (on `GitCredential`/`WebhookReceiver`) reference a `StoredSecret` by name. Secret values are never stored in Git; create them with `unified-cd secret set` before syncing."

- [ ] **Step 2: Verify links/format**

Run: `grep -n 'AppSource' docs/resources.md | head` and read the section to confirm consistent formatting.

- [ ] **Step 3: Commit**
```bash
git add docs/resources.md
git commit -m "docs(resources): document AppSource multi-kind sync, non-cascade prune, secret handling"
```

---

## Self-Review Notes

- **Spec coverage:** kinds (Tasks 3-4), non-cascade delete (Task 3 `deleteResource` + Task 4 prune + Task 4 test), secretRef name-only (no code path syncs values; Task 5 docs), determinism/collision (Task 4), prune opt-in extended (Task 4), schema `managed_resources` + migration (Task 1), `ParseGitCredential` (Task 2), testing (each task), docs (Task 5). All spec sections mapped.
- **Placeholder scan:** every code step contains complete code; the only prose direction is Task 4 Step 1's "reuse existing seeding helper," which is explicit about extracting from `TestReconciler_AppliesJobsFromGit`.
- **Type consistency:** `store.ResourceRef{Kind,Name}`, `AppSource.ManagedResources`, `UpdateAppSourceSyncState(...,[]ResourceRef)`, `applyResource(ctx,st,kind,doc)(string,error)`, `deleteResource(ctx,st,kind,name)error`, `probeKind([]byte)string` used consistently across Tasks 1, 3, 4.
- **Error classification:** parse/unknown-kind vs store-write failures are distinguished with the `errStoreWrite` sentinel (`errors.Is`), not string matching — store failures abort the sync, parse failures skip one file.
