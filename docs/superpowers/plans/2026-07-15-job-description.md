# Job Description in WebUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional `spec.description` job field, surface it through the jobs API, and display it (plain text) in the WebUI job list and job detail pages.

**Architecture:** `description` rides through the existing stored-spec-JSON pipeline exactly like `spec.params.inputs` does: added to `dsl.Spec`, extracted by the job handlers into `api.Job`, rendered by two Svelte routes. No schema change, no store change.

**Tech Stack:** Go, Svelte + Vite (WebUI), testify, `store.NewTestPostgres` (dockerized Postgres for handler tests), Vitest/jsdom (web tests).

**Spec:** `docs/superpowers/specs/2026-07-15-job-description-design.md`

## Global Constraints

- All code, comments, commit messages, docs in **English** (AGENTS.md). No PII.
- Worktree `../unified-cd-job-description`, branch `job-description` (base main). Never commit from the main tree.
- Description is **optional, free plain text**; no validation, no Markdown.
- Extraction is lenient: a spec that fails to parse or has no description yields `""` without error (mirrors the existing `specInputs`).
- Handler tests need Docker (`store.NewTestPostgres`, skipped under `-short`). `docs/field-reference.md` is generated — do NOT hand-edit (run `go generate ./...` if the DSL change regenerates it, and commit that diff).

---

### Task 1: DSL field + API extraction

**Files:**
- Modify: `internal/dsl/types.go:23-46` (`Spec` struct — add `Description`)
- Modify: `internal/api/types.go:23-32` (`api.Job` — add `Description`)
- Modify: `internal/controller/api_jobs.go` (`specInputs`→`specMeta`, and the two call sites at `listJobsDecorated` ~:122 and `serveJob` ~:171)
- Test: `internal/dsl/` parse test (extend an existing spec-parse test file) + `internal/controller/api_jobs_test.go` (extend)

**Interfaces:**
- Produces: `dsl.Spec.Description string`; `api.Job.Description string`; handlers populate `Description` from the stored spec.

- [ ] **Step 1: Write the failing tests**

Add to `internal/controller/api_jobs_test.go` (mirrors `TestAPI_GetJob_ReturnsInputs`):

```go
func TestAPI_GetJob_ReturnsDescription(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := `{"description":"Deploys the app to production","steps":[]}`
	_, _ = pg.UpsertJob(t.Context(), "deploy", "unified-cd/v1", []byte(specJSON))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/deploy", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "Deploys the app to production", got.Description)
}

func TestAPI_ListJobs_ReturnsDescription(t *testing.T) {
	s, pg := newTestServer(t)
	specJSON := `{"description":"Nightly build","steps":[]}`
	_, _ = pg.UpsertJob(t.Context(), "build", "unified-cd/v1", []byte(specJSON))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got []api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "Nightly build", got[0].Description)
}

func TestAPI_GetJob_NoDescriptionWhenEmpty(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "simple", "unified-cd/v1", []byte(`{"steps":[]}`))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/simple", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got api.Job
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Empty(t, got.Description)
}
```

(Check the auth token literal the existing job tests use — grep `Bearer ` in `api_jobs_test.go`; if it's not `secret`, match theirs.)

Add a DSL round-trip test — find the spec-parse test file (`grep -rln "func TestParse\|dsl.Parse" internal/dsl/*_test.go`) and add:

```go
func TestParse_JobDescription(t *testing.T) {
	y := []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  description: Builds and deploys\n  steps: []\n")
	job, err := dsl.Parse(y) // use the package's real parse entrypoint/signature
	require.NoError(t, err)
	assert.Equal(t, "Builds and deploys", job.Spec.Description)
}
```

(Adjust `dsl.Parse` to the actual parse function name/signature the other tests in that file use — it may return a `*dsl.Job` or `(dsl.Job, error)`; match it.)

- [ ] **Step 2: Run to verify failure**

Run: `cd /path/to/unified-cd-job-description && go test ./internal/controller/ -run 'ReturnsDescription|NoDescriptionWhenEmpty' -v; go test ./internal/dsl/ -run TestParse_JobDescription -v`
Expected: compile error (`got.Description` / `job.Spec.Description` undefined).

- [ ] **Step 3: Add the fields and extraction**

`internal/dsl/types.go` — add to `Spec` (after `Shell`, keeping the job-level-config grouping):

```go
	// Description is a human-readable summary of the job, shown in the WebUI.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
```

`internal/api/types.go` — add to `Job` (after `Inputs`):

```go
	Description string     `json:"description,omitempty"`
```

`internal/controller/api_jobs.go` — replace `specInputs` with a `specMeta` that returns both, and update the two call sites. New helper:

```go
// specMeta extracts the WebUI-facing metadata (inputs + description) from the
// stored spec JSON in a single parse. Lenient: a spec that fails to parse or
// omits a field yields the zero value for that field, never an error.
func specMeta(specJSON []byte) (inputs []api.InputDef, description string) {
	var spec dsl.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, ""
	}
	description = spec.Description
	if len(spec.Params.Inputs) == 0 {
		return nil, description
	}
	inputs = make([]api.InputDef, len(spec.Params.Inputs))
	for i, in := range spec.Params.Inputs {
		inputs[i] = api.InputDef{
			Name:        in.Name,
			Type:        in.Type,
			Required:    in.Required,
			Default:     in.Default,
			Description: in.Description,
		}
	}
	return inputs, description
}
```

Delete the old `specInputs` function. Update `listJobsDecorated`:

```go
	for i := range jobs {
		jobs[i].Inputs, jobs[i].Description = specMeta(jobs[i].Spec)
		jobs[i].Path, jobs[i].Leaf = dsl.SplitQualifiedName(jobs[i].Name)
	}
```

and `serveJob`:

```go
	job.Inputs, job.Description = specMeta(job.Spec)
	job.Path, job.Leaf = dsl.SplitQualifiedName(job.Name)
```

Grep for any other `specInputs(` caller and update it: `grep -rn "specInputs" internal/`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/controller/ ./internal/dsl/ -count=1 && go build ./...`
Then run `go generate ./...` and `git status` — if `docs/field-reference.md` or `schemas/` regenerated (the DSL schema is generated), commit that diff with this task.
Expected: PASS; pre-existing input tests still green (they now exercise `specMeta` via the same handlers).

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/types.go internal/api/types.go internal/controller/api_jobs.go internal/controller/api_jobs_test.go internal/dsl/
git add -A docs/field-reference.md schemas/ 2>/dev/null || true
git commit -m "feat(jobs): spec.description field surfaced through the jobs API"
```

---

### Task 2: WebUI — job list + detail display

**Files:**
- Modify: `web/src/routes/JobList.svelte` (subtitle under the name)
- Modify: `web/src/routes/JobDetail.svelte` (fetch job, show description under the heading)
- Test: `web/src/routes/JobDetail.test.js` (extend) + `web/src/routes/JobList.test.js` (create if absent)

**Interfaces:**
- Consumes: `GET /api/v1/jobs` and `GET /api/v1/jobs/{name}` now include `description` (Task 1).
- Produces: no new symbols; UI renders description when present, nothing when absent.

- [ ] **Step 1: Write the failing web tests**

Extend `web/src/routes/JobDetail.test.js` — check the file's existing test setup (how it mocks `apiFetch` and mounts the component), then add a case: when the job fetch resolves with `{name:'deploy', description:'Deploys the app'}`, the rendered output contains `Deploys the app`; when description is absent/empty, that text is not present and the component still renders the runs area. Mirror the existing mock style in that file exactly (do not invent a new harness).

For `JobList.test.js` (create only if there's an existing web test to copy the harness from — otherwise add the list assertion into a shared web test that already mounts JobList; if no JobList test infra exists, add a minimal one following `JobDetail.test.js`'s pattern): when `/api/v1/jobs` returns a job with `description:'Nightly build'`, the list shows `Nightly build`; a job without a description shows no description node.

- [ ] **Step 2: Run to verify failure**

Run: `cd web && npm test -- JobDetail` (and `JobList` if created)
Expected: FAIL — the description text is not yet rendered.

- [ ] **Step 3: Implement the display**

`web/src/routes/JobDetail.svelte`:
- Add a `job` state and a fetch in the script:
```js
  let job = null;
  async function loadJob() {
    try {
      job = await apiFetch("/api/v1/jobs/" + encodeURIComponent(jobName));
    } catch (_) {
      job = null; // description just won't show; runs list is primary
    }
  }
```
  Call `loadJob()` in `onMount` alongside `load()`/`loadSched()`, and add `$: jobName, loadJob();` next to the existing reactive statements.
- In the markup, under the `<h1>{jobName}</h1>` header block (after the `</div>` closing the flex header at line ~61, before the tab bar), add:
```svelte
  {#if job?.description}
    <p class="meta" style="margin:-0.5rem 0 1rem">{job.description}</p>
  {/if}
```

`web/src/routes/JobList.svelte` — in the job row's name cell (inside the `<td>` at line ~101, after the `<a ...>{row.job.leaf}</a>` and the active-run badges block, still inside that `<td>`), add:
```svelte
              {#if row.job.description}
                <div class="meta" style="font-size:0.8rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;max-width:40rem">{row.job.description}</div>
              {/if}
```

(Plain text via Svelte's default `{...}` interpolation, which HTML-escapes — no `{@html}`, so no injection risk.)

- [ ] **Step 4: Run to verify pass**

Run: `cd web && npm test` (full web suite) and `npm run build`
Expected: PASS including all pre-existing web tests; build succeeds.

- [ ] **Step 5: Commit**

```bash
git add web/src/routes/JobList.svelte web/src/routes/JobDetail.svelte web/src/routes/JobDetail.test.js web/src/routes/JobList.test.js
git commit -m "feat(web): show job description in the job list and detail pages"
```

---

### Task 3: Docs

**Files:**
- Modify: `docs/jobs.md` (document `spec.description`)

**Interfaces:**
- Consumes: the final field name `spec.description`.

- [ ] **Step 1: Document the field**

In `docs/jobs.md`, in the job spec reference (near `timeoutMinutes`/`native`/`shell` — grep `timeoutMinutes` in the file for the right section), add:

> `spec.description` *(optional, string)* — a human-readable summary of the job, shown in the WebUI job list and job detail pages. Plain text.

Add a one-line example to a nearby YAML snippet if the section has one (a `description:` line under `spec:`).

- [ ] **Step 2: Sweep**

```bash
grep -rn "spec.description\|description" docs/jobs.md | head
go build ./...   # sanity (docs-only, but confirm the tree still builds)
git status       # only docs/jobs.md changed in this task
```

Expected: the new entry present; nothing else changed.

- [ ] **Step 3: Commit**

```bash
git add docs/jobs.md
git commit -m "docs: document spec.description"
```
