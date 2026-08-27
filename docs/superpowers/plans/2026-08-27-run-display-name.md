# Run Display Name Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a declarative `spec.displayName` field to the Job DSL, interpolated with the run's own params at run-creation time and stored/shown as a human-readable label for the run — the equivalent of Jenkins' `currentBuild.displayName`, replacing the unreadable "first 8 chars of a UUID" in the WebUI run list.

**Architecture:** `Spec.DisplayName` (a Go-template string) is expanded exactly once, at run-creation time, using the same `dsl.ExpandTemplate`/`dsl.TemplateData{Params: ...}` path that `agentSelector` already uses. The expanded value is sanitized (`sanitizeAgentText`, reused, NUL/invalid-UTF-8 fixup) and length-capped in `internal/controller` — the codebase's stated "ingestion boundary" for untrusted agent/webhook-derived text — then passed into `store.CreateRun` as a new parameter and persisted in a new nullable `runs.display_name` column. It rides back out through `api.Run.DisplayName` (`omitempty`) to the WebUI, which falls back to today's truncated-UUID rendering whenever it's absent — which is every run that exists before this change ships, and every run whose job doesn't declare `displayName:`.

**Tech Stack:** Go (backend, `internal/dsl`, `internal/store` w/ Postgres + golang-migrate-style embedded SQL migrations, `internal/controller`), Svelte + Vitest (frontend, `web/`), MkDocs (`docs/`), a hand-rolled AST-based JSON-Schema generator (`cmd/schemagen`) + doc generator (`cmd/docgen`).

**Spec:** No separate spec doc exists for this feature; the task brief (this plan's originating prompt) is the spec. Key excerpts are folded into each task below so this plan is self-contained.

## Global Constraints

- `Spec.DisplayName` MUST carry **both** `yaml:"displayName,omitempty"` and `json:"displayName,omitempty"` tags — the store persists `Spec` as JSON and re-reads it (see `Detached`/`Vars` in `internal/dsl/types.go`), and `omitempty` is what keeps the field optional in the generated JSON Schema (schemagen derives `required` from its absence — see PR #153, `internal/dsl/types.go:24-28`).
- Interpolation MUST reuse `dsl.ExpandTemplate`/`dsl.TemplateData` (`internal/dsl/template.go`) — do not write a second template-expansion path.
- A malformed template (parse/execute error, e.g. bad `{{` syntax or an unknown function) MUST fail run creation with an error, mirroring `dsl.ExpandAgentSelector`'s existing behavior at every one of its call sites.
- A reference to an **undeclared** param (e.g. `{{ .Params.typo }}`) MUST expand to `""`, not fail — this is `ExpandTemplate`'s existing `missingkey=zero` behavior, and is the behavior PR #162 deliberately made `if:` consistent with. No new code is needed for this; it falls out of reusing `ExpandTemplate` unmodified.
- The interpolated display name MUST be sanitized for NUL bytes / invalid UTF-8 by reusing `sanitizeAgentText` (`internal/controller/agent_text_sanitize.go`) — do not write a second sanitizer. This happens in `internal/controller`, not `internal/store` (see that function's doc comment: *"This is the controller's ingestion boundary, not the store"*).
- The interpolated display name MUST be capped at `maxDisplayNameLength = 200` runes server-side (not only in CSS), truncated (not rejected) with a trailing `…` when it exceeds the cap. Truncate, don't reject: unlike `ValidateName` (`internal/dsl/name.go`), which rejects author-declared identifiers, a display name is cosmetic and derived from potentially-attacker-influenced webhook params — failing the whole run over a cosmetic overflow is a worse failure mode than a truncated label.
- Existing runs (no `display_name` stored) MUST render in the WebUI exactly as they do today: `{r.id.slice(0, 8)}…` in the run list, nothing extra on the run detail page.
- Migration number is **019** (018 is `018_vars`, confirmed latest). The migration's `down` must be a working, complete reversal.
- Every new/changed Go doc comment on `Spec.DisplayName` must avoid a **blank line** inside a doc-comment block (breaks `docgen`'s markdown table — see `internal/dsl/types.go:66-72`'s warning on `Vars`); use a block comment for maintainer context + a single-line trailing comment for the published field reference, exactly like `Vars` does.
- Regenerate `schemas/unified-cd.schema.json` and `docs/reference/field-reference.md` via `go generate ./internal/dsl/` after the Go struct changes, and commit both — `TestSchemaIsUpToDate` (`cmd/schemagen/main_test.go`) fails CI otherwise.
- Verification commands (all must pass, run from repo root unless noted):
  ```
  go build ./...
  go vet ./...
  go test ./internal/dsl/ ./internal/store/ ./internal/controller/ ./cmd/schemagen/ -count=1
  python -m mkdocs build --strict
  ```
  plus, from `web/`: `npm test` (= `vitest run`).
- `internal/store` and `internal/controller` test suites each take minutes — wait for them, don't cancel early.
- Searchability/filtering of display names is explicitly **out of scope** for this feature (state this in the PR) — the list/search endpoints are not modified beyond returning the new field.

---

## Task 1: DSL — add `Spec.DisplayName`, regenerate schema + field reference

**Files:**
- Modify: `internal/dsl/types.go` (the `Spec` struct, currently lines 23-74)
- Modify (generated, via `go generate`): `schemas/unified-cd.schema.json`, `docs/reference/field-reference.md`
- Test: a new or existing `internal/dsl/*_test.go` file that unmarshals a Job YAML manifest into `dsl.Spec` (search first — see Step 1)

**Interfaces:**
- Produces: `dsl.Spec.DisplayName string` (yaml/json tag `displayName`, `omitempty` on both) — every later task reads this field off an unmarshaled `dsl.Spec`.

- [ ] **Step 1: Find the right test file and write a failing test**

  Run `grep -rl "yaml.Unmarshal" internal/dsl/*_test.go` (or search for a test that builds a `dsl.Spec`/`dsl.Job` from a YAML string, e.g. a test named like `TestParseJob`, `TestSpecUnmarshal`, `TestJobYAML`). If one exists, add a new test case there following its exact pattern. If none fits, create `internal/dsl/display_name_test.go`:

  ```go
  package dsl

  import (
  	"testing"

  	"github.com/stretchr/testify/require"
  	"gopkg.in/yaml.v3" // match whatever YAML import the rest of internal/dsl already uses — check an existing test file's import block first
  )

  func TestSpecDisplayNameUnmarshalsFromYAML(t *testing.T) {
  	yamlSrc := `
  displayName: "deploy {{ .Params.env }} @ {{ .Params.ref }}"
  steps:
    - name: build
      run: echo hi
  `
  	var spec Spec
  	require.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &spec))
  	require.Equal(t, `deploy {{ .Params.env }} @ {{ .Params.ref }}`, spec.DisplayName)
  }

  func TestSpecDisplayNameOmittedByDefault(t *testing.T) {
  	yamlSrc := `
  steps:
    - name: build
      run: echo hi
  `
  	var spec Spec
  	require.NoError(t, yaml.Unmarshal([]byte(yamlSrc), &spec))
  	require.Empty(t, spec.DisplayName)
  }
  ```

  Before finalizing, `Read` an existing test file in `internal/dsl/` to confirm the exact YAML library import path and whether `Spec` is unmarshaled directly or via a wrapping `Job`/`Manifest` type — adjust the test to match real usage.

- [ ] **Step 2: Run the test and confirm it fails for the right reason**

  Run: `go test ./internal/dsl/ -run TestSpecDisplayName -v`
  Expected: **compile failure** — `spec.DisplayName undefined (type Spec has no field or method DisplayName)`. This compile-time red is the correct TDD signal in Go for "field doesn't exist yet."

- [ ] **Step 3: Add the field to `Spec`**

  In `internal/dsl/types.go`, insert immediately after the `Description` field (current line 58) and before the `Vars` field (current line 59), inside the `Spec` struct:

  ```go
  	// DisplayName is optional: a run whose job has no declared displayName
  	// falls back to the WebUI's existing behavior of showing the run's
  	// truncated ID -- today's rendering for every run that already exists,
  	// so leaving this unset changes nothing.
  	//
  	// When set, it is interpolated with the run's own resolved params at
  	// run-creation time via the same ExpandTemplate path as agentSelector
  	// (see ExpandAgentSelector): missingkey=zero means an undeclared
  	// {{ .Params.X }} expands to "" rather than failing -- matching PR
  	// #162's if: behavior, not a third one -- while a malformed template
  	// (bad syntax, unknown function) fails run creation with 400, exactly
  	// like agentSelector's own template errors do.
  	//
  	// The interpolated result is then NUL/invalid-UTF-8-sanitized
  	// (sanitizeAgentText) and length-capped (maxDisplayNameLength) in
  	// internal/controller before it is stored, because params can be
  	// mapped from webhook payloads and are therefore untrusted text -- see
  	// internal/controller/run_display_name.go. That happens at the
  	// controller's ingestion boundary, not here and not in the store.
  	//
  	// BOTH tags are required: the store persists Spec as JSON and reads it
  	// back, so a yaml-only tag round-trips to nothing. See Detached/Vars.
  	DisplayName string `yaml:"displayName,omitempty" json:"displayName,omitempty"` // A human-readable label for this job's runs, e.g. "deploy {{ .Params.env }} @ {{ .Params.ref }}" -- interpolated with the run's params at creation, shown in the WebUI in place of the truncated run ID.
  ```

- [ ] **Step 4: Run the test again and confirm it passes**

  Run: `go test ./internal/dsl/ -run TestSpecDisplayName -v`
  Expected: PASS (both `TestSpecDisplayNameUnmarshalsFromYAML` and `TestSpecDisplayNameOmittedByDefault`).

- [ ] **Step 5: Regenerate the schema and field reference**

  Run: `go generate ./internal/dsl/`
  This runs both `cmd/schemagen` (writes `schemas/unified-cd.schema.json`) and `cmd/docgen` (writes `docs/reference/field-reference.md`). Confirm via `git diff schemas/unified-cd.schema.json docs/reference/field-reference.md` that a `displayName` entry now appears under the `Spec` definition (JSON schema) and in the `### Spec` table (field reference, alphabetically before `finally`), with the description text `"A human-readable label for this job's runs, e.g. \"deploy {{ .Params.env }} @ {{ .Params.ref }}\" -- interpolated with the run's params at creation, shown in the WebUI in place of the truncated run ID."`.

- [ ] **Step 6: Run the schemagen/DSL test suites**

  Run: `go test ./internal/dsl/ ./cmd/schemagen/ -count=1 -v`
  Expected: PASS, including `TestSchemaIsUpToDate` and `TestExamplesValidateAgainstSchema` (the latter confirms no existing `examples/jobs/*.yaml` manifest breaks against the new schema — it shouldn't, since the field is optional and `additionalProperties: false` was already true before this change for every OTHER unknown key, so nothing regresses).

- [ ] **Step 7: Commit**

  ```bash
  git add internal/dsl/types.go internal/dsl/display_name_test.go schemas/unified-cd.schema.json docs/reference/field-reference.md
  git commit -m "feat(dsl): add optional spec.displayName field to the Job schema"
  ```
  (Adjust the test file path in `git add` if Step 1 added to an existing file instead.)

---

## Task 2: Store — migration 019 (`runs.display_name`) + sentinel

**Files:**
- Create: `internal/store/migrations/019_run_display_name.up.sql`
- Create: `internal/store/migrations/019_run_display_name.down.sql`
- Modify: `internal/store/verify.go` (append to `schemaSentinels`, currently ending at line 46 with the `018_vars` entry)
- Test: `internal/store/verify_test.go` (existing `TestSchemaSentinelsCoverAllMigrations` is the failing-test trigger; optionally extend `TestVerifySchemaCleanAndDrifted`-style coverage)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: a nullable `runs.display_name text` column that Task 3's store-layer code will SELECT/INSERT/Scan.

- [ ] **Step 1: Write the migration files (this is the "red" step — it makes the sentinel-coverage test fail)**

  `internal/store/migrations/019_run_display_name.up.sql`:
  ```sql
  -- display_name stores the run's interpolated, sanitized, length-capped
  -- display name (see internal/controller/run_display_name.go). NULL --
  -- not empty string -- means "no displayName: declared on the job", which
  -- is every run created before this migration and every run whose job
  -- doesn't opt in; the WebUI falls back to the truncated run ID in that
  -- case, so leaving this column NULL must not change any existing run's
  -- rendering.
  ALTER TABLE runs ADD COLUMN display_name text;
  ```

  `internal/store/migrations/019_run_display_name.down.sql`:
  ```sql
  ALTER TABLE runs DROP COLUMN IF EXISTS display_name;
  ```

- [ ] **Step 2: Run the sentinel-coverage test and confirm it fails**

  Run: `go test ./internal/store/ -run TestSchemaSentinelsCoverAllMigrations -v`
  Expected: FAIL — `require.Equal` reports `ups` (19) != `len(schemaSentinels)` (18), with the message `"every migrations/*.up.sql needs a sentinel entry in internal/store/verify.go"`.

- [ ] **Step 3: Add the sentinel entry**

  In `internal/store/verify.go`, append immediately after the existing `018_vars` entry (current line 46):

  ```go
  	{19, "019_run_display_name", "runs", "display_name", ""},
  ```

  Confirm the full literal now reads (lines 28-47 today, one longer):
  ```go
  var schemaSentinels = []sentinel{
  	{1, "001_init", "runs", "", ""},
  	{2, "002_add_role", "pats", "role", ""},
  	{3, "003_appsource_managed_resources", "app_sources", "managed_resources", ""},
  	{4, "004_audit_logs", "audit_logs", "", ""},
  	{5, "005_matrix_variant", "step_reports", "variant", ""},
  	{6, "006_appsource_sync_status", "app_sources", "sync_status", ""},
  	{7, "007_step_call_link", "step_reports", "child_run_id", ""},
  	{8, "008_run_indexes", "runs", "", "runs_job_name_created_idx"},
  	{9, "009_agent_capabilities", "agents", "capabilities", ""},
  	{10, "010_sidecar_status", "sidecar_status", "", ""},
  	{11, "011_runs_terminal_updated_idx", "runs", "", "runs_terminal_updated_idx"},
  	{12, "012_run_log_archives_trimmed_at", "run_log_archives", "line_count", ""},
  	{13, "013_agent_identity_auth", "agent_credentials", "token_hash", ""},
  	{14, "014_agent_enrollment_policies", "agent_enrollment_policies", "", ""},
  	{15, "015_secrets_v2", "sessions", "refresh_token_dek", ""},
  	{16, "016_drop_secret_scope", "secrets", "", "secrets_name_key"},
  	{17, "017_run_detached", "runs", "detached", ""},
  	{18, "018_vars", "vars", "", ""},
  	{19, "019_run_display_name", "runs", "display_name", ""},
  }
  ```

- [ ] **Step 4: Run the sentinel-coverage test again and confirm it passes**

  Run: `go test ./internal/store/ -run TestSchemaSentinelsCoverAllMigrations -v`
  Expected: PASS.

- [ ] **Step 5: Run the full drift/migration test suite (this stands up a real Postgres via testcontainers, so it's the "minutes" suite — let it finish)**

  Run: `go test ./internal/store/ -run TestVerifySchema -v -count=1`
  Expected: PASS — `TestVerifySchemaCleanAndDrifted` (and any other `TestVerifySchema*`) confirms a freshly-migrated database verifies clean with all 19 sentinels, including the new `runs.display_name` column probe.

  Optionally add a dedicated drift-detection case for the new column, modeled exactly on the existing `TestVerifySchemaCleanAndDrifted` (`internal/store/verify_test.go`, drops `step_reports.child_run_id` and asserts the error mentions `007_step_call_link`): add a second `t.Run` or new test that drops `runs.display_name` on a migrated clone and asserts `verifySchema` reports drift mentioning `019_run_display_name` and `runs.display_name`. This is optional polish, not required for the task to be complete.

- [ ] **Step 6: Confirm migrations apply and roll back cleanly end-to-end**

  The store test suite already exercises the full up-migration path via `NewTestPostgres(t)` (used throughout `internal/store`), which is sufficient evidence the `up` file is valid SQL. To specifically prove `down` works, run (from repo root, requires Docker for the ephemeral Postgres testcontainers already used by `internal/store` tests — if no separate down-migration test exists, this step is exploratory/manual, not a new committed test):
  ```
  go test ./internal/store/ -run TestSchemaSentinelsCoverAllMigrations -v
  ```
  and inspect (via `go test ./internal/store/... -v` broader run in Step 5 above) that no test fails due to an inconsistent down migration. `golang-migrate`'s `iofs` loader (used at `internal/store/postgres.go:119`) validates the `up`/`down` filename pairing at load time — if `019_run_display_name.down.sql` were missing or malformed, migration loading itself would already fail every store test, which Step 5's full run would surface.

- [ ] **Step 7: Commit**

  ```bash
  git add internal/store/migrations/019_run_display_name.up.sql internal/store/migrations/019_run_display_name.down.sql internal/store/verify.go
  git commit -m "feat(store): add runs.display_name column (migration 019)"
  ```

---

## Task 3: Store — `api.Run.DisplayName` + `CreateRun` signature + read-path queries

**Files:**
- Modify: `internal/api/types.go` (`Run` struct, currently lines 51-61)
- Modify: `internal/store/store.go` (the `Store` interface's `CreateRun` method signature)
- Modify: `internal/store/postgres.go` — `CreateRun` (lines 219-264), `GetRun` (344-367), `ListRunsByJob` (379-406), `ListActiveRuns` (408-435), `ListRunsByAgent` (1477-1504)
- Modify: `internal/metrics/store.go` (`InstrumentedStore.CreateRun`, ~line 30 — pass-through wrapper)
- Modify: `internal/controller/api_runs.go`, `internal/controller/api_webhooks.go`, `internal/controller/scheduler.go` — every call to `store.CreateRun(...)` (grep for `\.CreateRun(` to find them all) gets a new trailing argument; **pass the literal empty string `""` at every call site in this task** — Task 4 replaces each `""` with the real interpolated value. This keeps `go build ./...` green at the end of this task without pulling controller-side interpolation logic in early.
- Modify: `internal/controller/scheduler_schedule_test.go` (`mockScheduleFireStore.CreateRun` — update its signature to match)
- Test: `internal/store/postgres_test.go` (or wherever `CreateRun`/`GetRun` are already tested — search first) and `internal/api` if any serialization test exists for `Run`.

**Interfaces:**
- Consumes: `internal/store/migrations/019_run_display_name.up.sql` (Task 2) — the `runs.display_name` column must exist for these queries to compile against a live schema in tests.
- Produces: `store.CreateRun(ctx, jobName, params, spec, agentSelector, requiredCaps, triggeredBy, displayName string) (*api.Run, error)` — Task 4 calls this with a real value. `api.Run.DisplayName string` (json tag `displayName,omitempty`) — Task 5 (frontend) and Task 6 (docs) reference this field name.

- [ ] **Step 1: Find the existing `CreateRun`/`GetRun` test(s) and write a failing test**

  Run `grep -rl "CreateRun(ctx" internal/store/*_test.go` to find existing coverage. Add a new test (in that file, or a new `internal/store/run_display_name_test.go` if none fits) exercising the store layer directly, bypassing the controller entirely:

  ```go
  func TestCreateRunPersistsAndReturnsDisplayName(t *testing.T) {
  	pg := NewTestPostgres(t) // match whatever helper existing CreateRun tests use to get a *Postgres
  	ctx := context.Background()

  	_, err := pg.CreateJob(ctx, "test-job", []byte(`{"steps":[]}`)) // match the real CreateJob signature used by neighboring tests; a run's job_name FK likely requires the job to exist first -- check an existing CreateRun test for the exact job-creation prerequisite
  	require.NoError(t, err)

  	run, err := pg.CreateRun(ctx, "test-job", map[string]string{"env": "prod"}, []byte(`{"steps":[]}`), nil, nil, "api", "deploy prod")
  	require.NoError(t, err)
  	require.Equal(t, "deploy prod", run.DisplayName)

  	fetched, err := pg.GetRun(ctx, run.ID)
  	require.NoError(t, err)
  	require.Equal(t, "deploy prod", fetched.DisplayName)
  }

  func TestCreateRunWithEmptyDisplayNameStoresNoneAndListRunsOmitsIt(t *testing.T) {
  	pg := NewTestPostgres(t)
  	ctx := context.Background()

  	_, err := pg.CreateJob(ctx, "test-job2", []byte(`{"steps":[]}`))
  	require.NoError(t, err)

  	run, err := pg.CreateRun(ctx, "test-job2", nil, []byte(`{"steps":[]}`), nil, nil, "api", "")
  	require.NoError(t, err)
  	require.Empty(t, run.DisplayName) // existing-run rendering must be unaffected

  	runs, err := pg.ListRunsByJob(ctx, "test-job2", 10)
  	require.NoError(t, err)
  	require.Len(t, runs, 1)
  	require.Empty(t, runs[0].DisplayName)
  }
  ```

  Before finalizing: `Read` `internal/store/postgres.go` around `CreateRun` and an existing `_test.go` that calls it, to get the exact `NewTestPostgres`/`CreateJob` helper names and signatures right — the snippets above show intent, not guaranteed-exact existing helper names.

- [ ] **Step 2: Run the test and confirm it fails to compile**

  Run: `go test ./internal/store/ -run TestCreateRunPersistsAndReturnsDisplayName -v`
  Expected: compile error — `not enough arguments in call to pg.CreateRun` (or `run.DisplayName undefined`, depending on which change lands first in your read of the diff) — confirming neither the new `CreateRun` parameter nor `api.Run.DisplayName` exist yet.

- [ ] **Step 3: Add `DisplayName` to `api.Run`**

  In `internal/api/types.go`, add to the `Run` struct (current lines 51-61), after `ClaimedBy`:

  ```go
  	// DisplayName is the run's interpolated, sanitized, length-capped
  	// display name (see internal/controller/run_display_name.go). Empty
  	// for every run whose job has no spec.displayName -- which is every
  	// run created before this field existed -- so the WebUI falls back to
  	// showing the truncated run ID in that case, unchanged from today.
  	DisplayName string `json:"displayName,omitempty"`
  ```

- [ ] **Step 4: Extend `CreateRun`'s signature across the interface, Postgres impl, and metrics wrapper**

  `internal/store/store.go` — update the `Store` interface's `CreateRun` method to:
  ```go
  CreateRun(ctx context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, requiredCaps []string, triggeredBy string, displayName string) (*api.Run, error)
  ```

  `internal/store/postgres.go` — update `CreateRun` (lines 219-264):
  ```go
  func (p *Postgres) CreateRun(ctx context.Context, jobName string, params map[string]string, spec []byte, agentSelector []string, requiredCaps []string, triggeredBy string, displayName string) (*api.Run, error) {
  	if params == nil {
  		params = map[string]string{}
  	}
  	if agentSelector == nil {
  		agentSelector = []string{}
  	}
  	if requiredCaps == nil {
  		requiredCaps = []string{}
  	}
  	if triggeredBy == "" {
  		triggeredBy = "api"
  	}
  	paramsJSON, err := json.Marshal(params)
  	if err != nil {
  		return nil, err
  	}
  	// Derive the detached flag from the run's spec so ClaimNextRun can filter on
  	// a persisted column without re-parsing the spec on the hot claim path.
  	var detached bool
  	if len(spec) > 0 {
  		var s dsl.Spec
  		if json.Unmarshal(spec, &s) == nil {
  			detached = s.Detached
  		}
  	}
  	const q = `
  		INSERT INTO runs(job_name, params, spec, agent_selector, required_caps, triggered_by, detached, display_name)
  		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''))
  		RETURNING id, job_name, status, params, created_at, updated_at, triggered_by, display_name;
  	`
  	var r api.Run
  	var paramsOut []byte
  	var status string
  	var displayNameOut *string
  	err = p.pool.QueryRow(ctx, q, jobName, paramsJSON, spec, agentSelector, requiredCaps, triggeredBy, detached, displayName).
  		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &displayNameOut)
  	if err != nil {
  		return nil, fmt.Errorf("create run: %w", err)
  	}
  	r.Status = api.RunStatus(status)
  	_ = json.Unmarshal(paramsOut, &r.Params)
  	if r.Params == nil {
  		r.Params = map[string]string{}
  	}
  	if displayNameOut != nil {
  		r.DisplayName = *displayNameOut
  	}
  	return &r, nil
  }
  ```
  (`NULLIF($8, '')` turns an empty-string `displayName` argument into a real SQL `NULL`, matching the "NULL, not empty string, means no displayName:" convention from Task 2's migration comment.)

  `internal/metrics/store.go` — update the `InstrumentedStore.CreateRun` pass-through wrapper's signature to match (add the `displayName string` parameter to both its signature and its inner call to the wrapped store's `CreateRun`).

- [ ] **Step 5: Update `GetRun`, `ListRunsByJob`, `ListActiveRuns`, `ListRunsByAgent` to select/scan `display_name`**

  For each of the four functions, add `display_name` to the SELECT column list (immediately after `triggered_by`) and scan it into a local `*string`, then set `r.DisplayName` from it if non-nil. Example for `GetRun` (current lines 344-367):

  ```go
  func (p *Postgres) GetRun(ctx context.Context, id string) (*api.Run, error) {
  	const q = `SELECT id, job_name, status, params, created_at, updated_at, triggered_by, claimed_by, display_name FROM runs WHERE id = $1`
  	var r api.Run
  	var paramsOut []byte
  	var status string
  	var claimedBy *string
  	var displayName *string
  	err := p.pool.QueryRow(ctx, q, id).
  		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &claimedBy, &displayName)
  	// ... (rest of existing body unchanged) ...
  	if displayName != nil {
  		r.DisplayName = *displayName
  	}
  	// ... existing return ...
  }
  ```

  Apply the same shape (add `display_name` to SELECT, add a `var displayName *string`, add it to `Scan(...)`, set `r.DisplayName` after) to `ListRunsByJob` (lines 379-406) and `ListActiveRuns` (408-435) inside their `for rows.Next()` loops, and to `ListRunsByAgent` (1477-1504). Read each function's exact current body first (`Read internal/store/postgres.go` at the given line ranges) before editing, since exact variable names/error-handling may differ slightly from the `GetRun` example above.

  Deliberately **do not** modify `ListRunsNeedingArchival` (currently ~lines 1760-1791) — it already omits `triggered_by` from its SELECT, meaning it's an internal archival-housekeeping shape, not a UI-facing `api.Run` projection, and adding `display_name` there is unneeded scope. Leave a one-line comment there if convenient noting the deliberate omission.

- [ ] **Step 6: Update every `store.CreateRun(...)` call site to pass a placeholder**

  Run `grep -rn "\.CreateRun(" internal/` to enumerate every call site (expected: `internal/controller/api_runs.go` — at least 2 call sites, `internal/controller/api_webhooks.go`, `internal/controller/scheduler.go`, plus the test mock `internal/controller/scheduler_schedule_test.go`'s `mockScheduleFireStore.CreateRun`). At every production call site, append a trailing `""` argument (Task 4 will replace each with a real expanded value — leave a `// TODO(Task 4): interpolate spec.DisplayName` comment at each so they're easy to find). Update `mockScheduleFireStore.CreateRun`'s method signature in the test file to accept and (for realism) store the new parameter, mirroring how it already threads the other parameters through.

- [ ] **Step 7: Run the new store test and confirm it passes**

  Run: `go test ./internal/store/ -run TestCreateRunPersistsAndReturnsDisplayName -v` and `-run TestCreateRunWithEmptyDisplayNameStoresNoneAndListRunsOmitsIt -v`
  Expected: PASS.

- [ ] **Step 8: `go build ./...` and full store suite**

  Run: `go build ./...`
  Expected: succeeds (confirms every `CreateRun` call site — controller, metrics wrapper, test mocks — compiles with the new parameter).

  Run: `go test ./internal/store/ -count=1`
  Expected: PASS (this is the multi-minute suite — wait for it).

- [ ] **Step 9: Commit**

  ```bash
  git add internal/api/types.go internal/store/store.go internal/store/postgres.go internal/metrics/store.go internal/controller/api_runs.go internal/controller/api_webhooks.go internal/controller/scheduler.go internal/controller/scheduler_schedule_test.go internal/store/*_test.go
  git commit -m "feat(store): thread display_name through CreateRun and run-read queries"
  ```

---

## Task 4: Controller — interpolate, sanitize, and truncate the display name at run creation

**Files:**
- Create: `internal/controller/run_display_name.go`
- Test: `internal/controller/run_display_name_test.go`
- Modify: `internal/controller/api_runs.go` — `createRunFromJob` (lines 44-88) and `handleReplayRun` (~line 100-160)
- Modify: `internal/controller/api_webhooks.go` — webhook-triggered run creation (~line 261)
- Modify: `internal/controller/scheduler.go` — cron-fired run creation (~line 193)

**Interfaces:**
- Consumes: `dsl.Spec.DisplayName` (Task 1), `dsl.ExpandTemplate`/`dsl.TemplateData` (existing, `internal/dsl/template.go`), `sanitizeAgentText` (existing, `internal/controller/agent_text_sanitize.go`), `store.CreateRun(..., displayName string)` (Task 3).
- Produces: `expandRunDisplayName(tmpl string, params map[string]string) (string, error)` in package `controller` — the single function every run-creation call site uses in place of the `""` placeholder from Task 3.

- [ ] **Step 1: Write the failing tests first**

  Create `internal/controller/run_display_name_test.go`:

  ```go
  package controller

  import (
  	"strings"
  	"testing"

  	"github.com/stretchr/testify/require"
  )

  func TestExpandRunDisplayName_Empty(t *testing.T) {
  	got, err := expandRunDisplayName("", map[string]string{"env": "prod"})
  	require.NoError(t, err)
  	require.Empty(t, got)
  }

  func TestExpandRunDisplayName_InterpolatesParams(t *testing.T) {
  	got, err := expandRunDisplayName("deploy {{ .Params.env }} @ {{ .Params.ref }}", map[string]string{"env": "prod", "ref": "abc123"})
  	require.NoError(t, err)
  	require.Equal(t, "deploy prod @ abc123", got)
  }

  func TestExpandRunDisplayName_UndeclaredParamExpandsToEmpty(t *testing.T) {
  	got, err := expandRunDisplayName("deploy {{ .Params.typo }}", map[string]string{"env": "prod"})
  	require.NoError(t, err)
  	require.Equal(t, "deploy ", got)
  }

  func TestExpandRunDisplayName_MalformedTemplateErrors(t *testing.T) {
  	_, err := expandRunDisplayName("deploy {{ .Params.env", map[string]string{"env": "prod"})
  	require.Error(t, err)
  }

  func TestExpandRunDisplayName_SanitizesNUL(t *testing.T) {
  	got, err := expandRunDisplayName("deploy {{ .Params.env }}", map[string]string{"env": "prod\x00staging"})
  	require.NoError(t, err)
  	require.NotContains(t, got, "\x00")
  	require.Contains(t, got, "�")
  }

  func TestExpandRunDisplayName_TruncatesAtCap(t *testing.T) {
  	long := strings.Repeat("x", maxDisplayNameLength+50)
  	got, err := expandRunDisplayName("{{ .Params.long }}", map[string]string{"long": long})
  	require.NoError(t, err)
  	require.Equal(t, maxDisplayNameLength+1, len([]rune(got))) // cap runes + trailing "…"
  	require.True(t, strings.HasSuffix(got, "…"))
  }

  func TestExpandRunDisplayName_ShortStringNotTruncated(t *testing.T) {
  	got, err := expandRunDisplayName("{{ .Params.short }}", map[string]string{"short": "deploy prod"})
  	require.NoError(t, err)
  	require.Equal(t, "deploy prod", got)
  	require.False(t, strings.HasSuffix(got, "…"))
  }
  ```

- [ ] **Step 2: Run the tests and confirm they fail**

  Run: `go test ./internal/controller/ -run TestExpandRunDisplayName -v`
  Expected: compile error — `undefined: expandRunDisplayName` (and `undefined: maxDisplayNameLength`).

- [ ] **Step 3: Implement `expandRunDisplayName`**

  Create `internal/controller/run_display_name.go`:

  ```go
  package controller

  import (
  	"unicode/utf8"

  	"github.com/<module>/internal/dsl" // match the exact import path used elsewhere in this package, e.g. api_runs.go's import block
  )

  // maxDisplayNameLength caps the number of runes kept in a run's display
  // name after template interpolation. The value is derived from an
  // attacker-influenceable source (params can be mapped from webhook
  // payloads -- see internal/dsl/types.go's DisplayName doc comment), so it
  // has no natural upper bound; 200 is comfortably larger than any label
  // that stays readable in a WebUI table cell or a run-detail page header,
  // while still bounding storage and layout cost. Exceeding it truncates
  // rather than rejects the run: unlike an author-declared identifier
  // (see dsl.ValidateName), a display name is cosmetic, so failing run
  // creation over a cosmetic overflow would be a worse failure mode than a
  // truncated label.
  const maxDisplayNameLength = 200

  // expandRunDisplayName interpolates a job's spec.displayName template
  // with the run's own resolved params, then makes the result safe to
  // store and display.
  //
  // It reuses dsl.ExpandTemplate -- the same Go-template path
  // ExpandAgentSelector already uses at this same run-creation moment --
  // rather than a second template implementation. That path's
  // missingkey=zero option means a reference to an undeclared param (e.g.
  // {{ .Params.typo }}) expands to "" rather than erroring; that is not a
  // bug to guard against here, it is the same behavior PR #162
  // deliberately made if: conditions consistent with, and this function
  // stays consistent with both by doing nothing extra for it. A malformed
  // template (bad syntax, an unknown function call) is a genuine error and
  // is returned to the caller, which aborts run creation exactly like
  // ExpandAgentSelector's own template errors do -- a broken displayName:
  // template is an authoring mistake in the job, not a runtime input to
  // silently swallow.
  //
  // The expanded string is then run through sanitizeAgentText: params can
  // be mapped from webhook payloads (see internal/controller/params.go's
  // resolveParams), so the interpolated text is exactly as untrusted as
  // agent-submitted log lines, which is what sanitizeAgentText already
  // exists to make Postgres-storable. See that function's doc comment for
  // why fixing bytes up front beats failing the request over one bad byte.
  //
  // Finally the result is capped at maxDisplayNameLength runes. An empty
  // template (the common case: no spec.displayName declared) short-circuits
  // before any of this, so a run with no displayName: costs one string
  // comparison, not a template parse.
  func expandRunDisplayName(tmpl string, params map[string]string) (string, error) {
  	if tmpl == "" {
  		return "", nil
  	}
  	expanded, err := dsl.ExpandTemplate(tmpl, dsl.TemplateData{Params: params})
  	if err != nil {
  		return "", err
  	}
  	expanded, _ = sanitizeAgentText(expanded)
  	return truncateDisplayName(expanded, maxDisplayNameLength), nil
  }

  // truncateDisplayName returns s unchanged if it has at most max runes,
  // otherwise the first max runes followed by a single "…" so a truncated
  // name is visually distinguishable from one that happens to be exactly
  // max runes long. Operates on runes, not bytes, so a cap never lands
  // mid-multi-byte-character -- sanitizeAgentText has already guaranteed s
  // is valid UTF-8 by the time this runs, so a rune-by-rune walk here is
  // safe and cheap.
  func truncateDisplayName(s string, max int) string {
  	if utf8.RuneCountInString(s) <= max {
  		return s
  	}
  	runes := []rune(s)
  	return string(runes[:max]) + "…"
  }
  ```

  Before finalizing, `Read` `internal/controller/api_runs.go`'s import block to copy the exact module path used for `internal/dsl` (the placeholder `github.com/<module>/internal/dsl` above must be replaced with the real one).

- [ ] **Step 4: Run the tests again and confirm they pass**

  Run: `go test ./internal/controller/ -run TestExpandRunDisplayName -v`
  Expected: PASS, all 7 cases.

- [ ] **Step 5: Wire `expandRunDisplayName` into `createRunFromJob`**

  In `internal/controller/api_runs.go`, inside `createRunFromJob` (current lines 44-88), immediately after the existing `agentSelector, err = dsl.ExpandAgentSelector(agentSelector, params)` block (lines 63-66), add:

  ```go
  	displayName, err := expandRunDisplayName(spec.DisplayName, params)
  	if err != nil {
  		return nil, http.StatusBadRequest, "displayName: " + err.Error()
  	}
  ```

  Then change the `s.store.CreateRun(...)` call (current line 83) from its Task-3 placeholder `""` to `displayName`:

  ```go
  	run, err := s.store.CreateRun(ctx, job.Name, params, runSpec, agentSelector, requiredCaps, triggeredBy, displayName)
  ```

- [ ] **Step 6: Wire it into `handleReplayRun`, the webhook path, and the scheduler path**

  `Read` each of the three remaining call sites first (`internal/controller/api_runs.go` around `handleReplayRun`, `internal/controller/api_webhooks.go` around line 261, `internal/controller/scheduler.go` around line 193) to find their exact local variable names for the unmarshaled `dsl.Spec` and resolved `params map[string]string` (each already unmarshals a spec and resolves params to call `dsl.ExpandAgentSelector`, per Task 3's research — reuse those same locals). At each site, add the identical two-step pattern from Step 5 (call `expandRunDisplayName`, propagate/log its error the same way that site already handles `ExpandAgentSelector`'s error — a 400 in the two HTTP-handler paths (`api_runs.go`, `api_webhooks.go`), and whatever the scheduler already does when `ExpandAgentSelector` fails in `scheduler.go`, e.g. skip firing that occurrence and log — mirror it exactly rather than inventing new behavior for one field), then replace that site's `""` placeholder in its `CreateRun(...)` call with the computed `displayName`.

- [ ] **Step 7: `go build ./...`**

  Run: `go build ./...`
  Expected: succeeds, and no `""`  placeholder / `// TODO(Task 4)` comments remain at any production `CreateRun` call site (`grep -rn "TODO(Task 4)" internal/` should return nothing).

- [ ] **Step 8: Write and run integration-level tests at `createRunFromJob`**

  Find the existing test(s) for `createRunFromJob`/`handleTriggerRun` (likely in `internal/controller/api_runs_test.go` — search first). Add cases modeled on its existing shape:
  - Triggering a run for a job whose spec has `displayName: "deploy {{ .Params.env }}"` with `params: {"env": "prod"}` produces a run whose `DisplayName == "deploy prod"`.
  - Triggering a run for a job whose spec has a malformed `displayName:` (e.g. `"deploy {{ .Params.env"`) returns `400` and does not create a run.
  - Triggering a run for a job with no `displayName:` produces a run whose `DisplayName == ""`.

  Run: `go test ./internal/controller/ -run TestCreateRunFromJob -v` (adjust the `-run` pattern to whatever the actual test function names turn out to be) and confirm PASS.

- [ ] **Step 9: Full controller suite**

  Run: `go test ./internal/controller/ -count=1`
  Expected: PASS (multi-minute suite — wait for it; per the task brief, `internal/agent`'s retry tests are separately known to be load-sensitive noise, but that package is not part of this suite).

- [ ] **Step 10: Commit**

  ```bash
  git add internal/controller/run_display_name.go internal/controller/run_display_name_test.go internal/controller/api_runs.go internal/controller/api_webhooks.go internal/controller/scheduler.go internal/controller/api_runs_test.go
  git commit -m "feat(controller): interpolate, sanitize, and cap spec.displayName at run creation"
  ```

---

## Task 5: WebUI — show the display name in the run list and run detail page

**Files:**
- Modify: `web/src/routes/JobDetail.svelte` (run list table, currently lines 89-124 + `hasParams` at 16-18)
- Modify: `web/src/routes/RunDetail.svelte` (run detail field grid, currently lines 1145-1181)
- Test: `web/src/routes/JobDetail.test.js`, `web/src/routes/RunDetail.test.js`

**Interfaces:**
- Consumes: `r.displayName` / `run.displayName` (a plain JS string, present and non-empty only when `api.Run.DisplayName` is non-empty — Task 3/4) on run objects returned by the existing `/api/v1/jobs/:name/runs` and `/api/v1/runs/:id` endpoints. No shared type file exists on the frontend (confirmed: no `.d.ts`/JSDoc `@typedef` for `Run` anywhere under `web/src`), so no type definition needs updating — just read the new JSON field directly.

- [ ] **Step 1: Write the failing JobDetail test**

  In `web/src/routes/JobDetail.test.js`, following the file's existing `jsonResponse`/`fetchMock`/`vi.waitFor` pattern (see the `describe('JobDetail — description display (Task 2)', ...)` block for the shape), add:

  ```js
  describe('JobDetail — run display name (run-display-name feature)', () => {
    it('shows the display name instead of the truncated run ID when present', async () => {
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/schedulability')) return jsonResponse({ satisfiable: true });
        if (u.includes('/runs')) return jsonResponse([{
          id: 'abcdef1234567890',
          status: 'Succeeded',
          triggeredBy: 'api',
          createdAt: new Date().toISOString(),
          params: {},
          displayName: 'deploy prod',
        }]);
        if (u.includes('/api/v1/jobs/')) return jsonResponse({ name: 'deploy', description: '' });
        return jsonResponse({});
      });
      global.fetch = fetchMock;

      const { container } = render(JobDetail, { props: { params: { name: 'deploy' } } });

      await vi.waitFor(() => {
        expect(container.textContent).toContain('deploy prod');
      });
    });

    it('falls back to the truncated run ID when displayName is absent, unchanged from today', async () => {
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/schedulability')) return jsonResponse({ satisfiable: true });
        if (u.includes('/runs')) return jsonResponse([{
          id: 'abcdef1234567890',
          status: 'Succeeded',
          triggeredBy: 'api',
          createdAt: new Date().toISOString(),
          params: {},
        }]);
        if (u.includes('/api/v1/jobs/')) return jsonResponse({ name: 'deploy', description: '' });
        return jsonResponse({});
      });
      global.fetch = fetchMock;

      const { container } = render(JobDetail, { props: { params: { name: 'deploy' } } });

      await vi.waitFor(() => {
        expect(container.textContent).toContain('abcdef12…');
      });
    });
  });
  ```

- [ ] **Step 2: Run and confirm it fails**

  Run (from `web/`): `npx vitest run src/routes/JobDetail.test.js`
  Expected: the first new test FAILS (`container.textContent` does not contain `'deploy prod'` — the table only ever renders `r.id.slice(0, 8)}…`); the second new test passes already (today's unchanged behavior).

- [ ] **Step 3: Update the run list table**

  In `web/src/routes/JobDetail.svelte`, change the Run ID cell (current line 103) from:
  ```svelte
  <td><a href="#/runs/{r.id}">{r.id.slice(0, 8)}…</a></td>
  ```
  to:
  ```svelte
  <td>
    <a href="#/runs/{r.id}">{r.displayName || r.id.slice(0, 8) + '…'}</a>
    {#if r.displayName}<div class="meta run-id-sub" title={r.id}>{r.id.slice(0, 8)}…</div>{/if}
  </td>
  ```
  This keeps a run with no `displayName` rendering byte-for-byte identical to today (`{r.id.slice(0, 8)}…`, no extra markup), while a named run shows its label as the primary link text with the truncated ID as a small secondary line underneath (using the existing `.meta` class already used elsewhere in this file for secondary text, e.g. line 105-106). Add a small `.run-id-sub` rule to the file's `<style>` block if one doesn't already produce a suitably muted, smaller line (check the existing `.meta` styling first — it likely already suffices on its own without a new class; only add `.run-id-sub` if `.meta` alone doesn't read as a visually secondary line under a link).

- [ ] **Step 4: Run and confirm both tests pass**

  Run: `npx vitest run src/routes/JobDetail.test.js`
  Expected: PASS.

- [ ] **Step 5: Write the failing RunDetail test**

  In `web/src/routes/RunDetail.test.js`, following the existing `describe('RunDetail — single SSE/events connection per run (TODO #10)', ...)` block's pattern for an optional field (`claimedBy`'s "renders an Agent link when present" / "omits ... when absent" pair, lines ~135-180), add an analogous pair for `displayName`. Read that existing pair first (`Read web/src/routes/RunDetail.test.js` lines 130-190) to copy its exact fetch-mocking/render/assert shape, then add:

  ```js
  describe('RunDetail — display name (run-display-name feature)', () => {
    it('renders the display name near the run header when present', async () => {
      // mock the run fetch to include displayName: 'deploy prod', following
      // the exact mocking pattern used by the neighboring claimedBy tests
      // ... await vi.waitFor(() => expect(container.textContent).toContain('deploy prod'));
    });

    it('renders nothing extra when displayName is absent', async () => {
      // mock the run fetch with no displayName field
      // ... assert the page still renders (no crash) and shows no display-name element
    });
  });
  ```

  (Write these two test bodies with the real mock/assert code once the exact `claimedBy` pair's source is read — the plan intentionally leaves the literal test bodies to be filled from that read rather than guessing RunDetail's large file's exact helper names, per this task's Step 5 instruction to read it first.)

- [ ] **Step 6: Run and confirm the "present" case fails**

  Run: `npx vitest run src/routes/RunDetail.test.js`
  Expected: the "renders the display name" test FAILS; the "absent" test already passes.

- [ ] **Step 7: Update the run detail header**

  In `web/src/routes/RunDetail.svelte`, inside the field grid card (current lines 1151-1181), add a conditional row immediately after the `Status` field (lines 1152-1155), following the exact `{#if run.claimedBy}` pattern used at lines 1160-1165:

  ```svelte
          {#if run.displayName}
            <div class="run-display-name" style="grid-column:1/-1">
              <div class="meta">Name</div>
              <div>{run.displayName}</div>
            </div>
          {/if}
  ```

- [ ] **Step 8: Run and confirm both RunDetail tests pass**

  Run: `npx vitest run src/routes/RunDetail.test.js`
  Expected: PASS.

- [ ] **Step 9: Full web test suite**

  Run (from `web/`): `npm test`
  Expected: PASS, no regressions in unrelated tests.

- [ ] **Step 10: Commit**

  ```bash
  git add web/src/routes/JobDetail.svelte web/src/routes/RunDetail.svelte web/src/routes/JobDetail.test.js web/src/routes/RunDetail.test.js
  git commit -m "feat(webui): show a run's display name in the run list and run detail page"
  ```

---

## Task 6: Docs — user guide + expressions reference

**Files:**
- Modify: `docs/user-guide/writing-jobs/job-structure.md` (mirror the existing `## Job Description` section, currently around lines 81-90)
- Modify: `docs/user-guide/writing-jobs/expressions.md` (the "Available variables" table, currently lines 1-14)

**Interfaces:**
- Consumes: the final behavior decided/implemented in Tasks 1-4 (interpolation, undefined-param-is-empty, template-error-is-400, sanitization, truncation at 200 runes) — this task documents that behavior, it doesn't change it.

- [ ] **Step 1: Add a "Run Display Name" section to `job-structure.md`**

  In `docs/user-guide/writing-jobs/job-structure.md`, add a new section following the exact format of the existing `## Job Description` section (current lines 81-90):

  ```markdown
  ## Run Display Name

  ​```yaml
  spec:
    displayName: "deploy {{ .Params.env }} @ {{ .Params.ref }}"
  ​```

  `spec.displayName` *(optional, string, templated)* — a human-readable label for this job's runs, shown in the WebUI run list and run detail page in place of the run's truncated ID. Interpolated with the run's own resolved params at run-creation time (see [Expressions and Conditions](expressions.md)).

  A run whose job has no `displayName:` falls back to the truncated run ID, exactly as every run rendered before this field existed. Referencing a param that wasn't declared/passed expands to an empty string rather than failing (consistent with `{{ .Params.NAME }}` everywhere else); a malformed template (bad syntax) fails the run trigger with an error instead of creating a run with a broken label. The interpolated value is capped at 200 characters (longer values are truncated with a trailing `…`) and is not searchable/filterable in this release.

  ---
  ```

  (Use literal triple-backtick fences, not the escaped `​```` shown above — that escaping is only to keep this plan's own Markdown from breaking.)

- [ ] **Step 2: Add `displayName` to the "Available variables" table in `expressions.md`**

  In `docs/user-guide/writing-jobs/expressions.md`, update the `{{ .Params.NAME }}` row's "Available in" column (current line 11) to include `displayName`:
  ```markdown
  | `{{ .Params.NAME }}` | `run`, `env`, `agentSelector`, `concurrency`, `outputs`, `displayName`, `call.with`, `uses.with`, `cache.key`, `cache.path`, `cache.restoreKeys` | Input parameter value |
  ```

- [ ] **Step 3: Build the docs site and confirm no errors**

  Run: `python -m mkdocs build --strict` (from repo root)
  Expected: succeeds with no warnings/errors (strict mode fails the build on broken links or malformed nav — confirms the new section doesn't break anything and needs no new `mkdocs.yml` nav entry, since it's a subsection of an already-linked page).

- [ ] **Step 4: Commit**

  ```bash
  git add docs/user-guide/writing-jobs/job-structure.md docs/user-guide/writing-jobs/expressions.md
  git commit -m "docs: document spec.displayName"
  ```

---

## Final Integration (main session, not a dispatched task)

After Tasks 1-6 all land:

1. Run the full verification suite from the task brief:
   ```
   go build ./...
   go vet ./...
   go test ./internal/dsl/ ./internal/store/ ./internal/controller/ ./cmd/schemagen/ -count=1
   python -m mkdocs build --strict
   ```
   plus `npm test` from `web/`.
2. `git fetch origin && git rebase origin/main` — run bare, never piped, so a conflicted rebase's exit status is visible.
3. Re-run the full verification suite after rebasing.
4. Push and open a PR with `gh pr create`, leading with the run-list before/after, then untrusted-text handling, then the design decisions (interpolation-failure behavior, no-display-name UI fallback, no search/filter), ending with the required Claude Code attribution line.
