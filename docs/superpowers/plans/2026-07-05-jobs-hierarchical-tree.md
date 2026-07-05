# Jobs hierarchical tree (directory-only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show jobs in the Web UI as a collapsible directory tree whose folders come from the AppSource repository directory structure, without changing job identity.

**Architecture:** A job's directory is carried in a new `metadata.annotations.path`. At apply time the controller joins `path` + short `name` into a single **qualified name** stored in the existing unique `jobs.name` column (no schema change, no composite key). The API derives `path`/`leaf` from the qualified name; the frontend assembles a folder tree client-side. AppSource is deliberately excluded from the UI and from identity.

**Tech Stack:** Go 1.26 (chi router, testify, pgx), Svelte + svelte-spa-router, Vitest.

## Global Constraints

- `jobs.name` remains the sole unique identifier. Do NOT add a composite key, relax the `UNIQUE(name)` constraint, or add a `path` column. No DB migration.
- Do NOT touch run association, schedules, or webhook receivers — they reference jobs by `jobName`, which is now the qualified name, and need no code change.
- AppSource name must not appear in the UI, the API, or any job reference.
- Reserved annotation key is exactly `path`. Empty/absent `path` ⇒ qualified name == short name (backward compatible; existing flat jobs render at tree root).
- Short `name` is joined to `path` with a single `/`; leading/trailing slashes on both are trimmed.

---

### Task 1: dsl — annotations + qualified-name helpers

**Files:**
- Modify: `internal/dsl/types.go:17-20` (Metadata struct)
- Create: `internal/dsl/qualname.go`
- Test: `internal/dsl/qualname_test.go`

**Interfaces:**
- Produces: `dsl.QualifyName(path, name string) string`; `func (m Metadata) QualifiedName() string`; `dsl.SplitQualifiedName(qualified string) (path, leaf string)`. Later tasks call these.

- [ ] **Step 1: Write the failing test**

Create `internal/dsl/qualname_test.go`:

```go
package dsl

import "testing"

func TestQualifyName(t *testing.T) {
	cases := []struct{ path, name, want string }{
		{"", "build", "build"},
		{"team-a", "build", "team-a/build"},
		{"team-b/edge", "test", "team-b/edge/test"},
		{"/team-a/", "build", "team-a/build"},
		{"team-a", "/build/", "team-a/build"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := QualifyName(c.path, c.name); got != c.want {
			t.Errorf("QualifyName(%q,%q)=%q want %q", c.path, c.name, got, c.want)
		}
	}
}

func TestSplitQualifiedName(t *testing.T) {
	cases := []struct{ q, wantPath, wantLeaf string }{
		{"build", "", "build"},
		{"team-a/build", "team-a", "build"},
		{"team-b/edge/test", "team-b/edge", "test"},
		{"", "", ""},
	}
	for _, c := range cases {
		p, l := SplitQualifiedName(c.q)
		if p != c.wantPath || l != c.wantLeaf {
			t.Errorf("SplitQualifiedName(%q)=(%q,%q) want (%q,%q)", c.q, p, l, c.wantPath, c.wantLeaf)
		}
	}
}

func TestMetadataQualifiedName(t *testing.T) {
	m := Metadata{Name: "build", Annotations: map[string]string{"path": "team-a"}}
	if got := m.QualifiedName(); got != "team-a/build" {
		t.Errorf("QualifiedName()=%q want team-a/build", got)
	}
	m2 := Metadata{Name: "hello"}
	if got := m2.QualifiedName(); got != "hello" {
		t.Errorf("QualifiedName()=%q want hello", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dsl/ -run 'TestQualifyName|TestSplitQualifiedName|TestMetadataQualifiedName' -v`
Expected: FAIL — `undefined: QualifyName` (and `Metadata has no field Annotations`).

- [ ] **Step 3: Add the Annotations field**

In `internal/dsl/types.go`, change the Metadata struct:

```go
type Metadata struct {
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}
```

- [ ] **Step 4: Write the helpers**

Create `internal/dsl/qualname.go`:

```go
package dsl

import "strings"

// QualifyName joins a directory path and a short name into a single qualified
// name (e.g. "team-a" + "build" -> "team-a/build"). Leading/trailing slashes on
// both parts are trimmed. An empty path yields the name unchanged.
func QualifyName(path, name string) string {
	path = strings.Trim(path, "/")
	name = strings.Trim(name, "/")
	if path == "" {
		return name
	}
	return path + "/" + name
}

// SplitQualifiedName splits a qualified name on its LAST slash into a directory
// path and a leaf. "team-a/build" -> ("team-a","build"); "build" -> ("","build").
func SplitQualifiedName(qualified string) (path, leaf string) {
	i := strings.LastIndex(qualified, "/")
	if i < 0 {
		return "", qualified
	}
	return qualified[:i], qualified[i+1:]
}

// QualifiedName returns the metadata's qualified name, folding in the reserved
// "path" annotation.
func (m Metadata) QualifiedName() string {
	return QualifyName(m.Annotations["path"], m.Name)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -v`
Expected: PASS (new tests plus existing dsl tests).

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/types.go internal/dsl/qualname.go internal/dsl/qualname_test.go
git commit -m "feat(dsl): metadata annotations + qualified-name helpers"
```

---

### Task 2: Direct apply stores the qualified name

**Files:**
- Modify: `internal/controller/api_jobs.go:15-37` (handleApplyJob)
- Test: `internal/controller/api_jobs_test.go` (create if absent)

**Interfaces:**
- Consumes: `job.Metadata.QualifiedName()` from Task 1.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/api_jobs_test.go` (create the file with this content if it does not exist; otherwise append the test):

```go
package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/eirueimi/unified-cd/internal/store"
)

func TestHandleApplyJob_UsesQualifiedName(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := &Server{store: pg}
	ctx := context.Background()

	const y = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
  annotations:
    path: team-a
spec:
  steps:
    - name: c
      run: "true"
`
	require.NoError(t, applyJobYAML(t, s, y))

	job, err := pg.GetJob(ctx, "team-a/build")
	require.NoError(t, err)
	assert.Equal(t, "team-a/build", job.Name)
}
```

Also add this test helper at the bottom of the same file (drives the handler through the store the same way the handler does):

```go
func applyJobYAML(t *testing.T, s *Server, yaml string) error {
	t.Helper()
	return s.applyJobFromYAML(context.Background(), yaml)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestHandleApplyJob_UsesQualifiedName -v`
Expected: FAIL — `s.applyJobFromYAML undefined`.

- [ ] **Step 3: Extract the apply logic and use the qualified name**

In `internal/controller/api_jobs.go`, replace the body of `handleApplyJob` so the store write goes through a small reusable method that uses the qualified name:

```go
// handleApplyJob parses a Job YAML definition and saves it to the database.
func (s *Server) handleApplyJob(w http.ResponseWriter, r *http.Request) {
	var req api.ApplyJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	stored, err := s.upsertJobFromYAML(r.Context(), req.YAML, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, stored)
}

// applyJobFromYAML is a thin wrapper used by tests.
func (s *Server) applyJobFromYAML(ctx context.Context, yaml string) error {
	_, err := s.upsertJobFromYAML(ctx, yaml, "")
	return err
}

// upsertJobFromYAML parses a Job document, folds dirOverride (when non-empty)
// into metadata.annotations["path"], and upserts under the qualified name.
func (s *Server) upsertJobFromYAML(ctx context.Context, yaml, dirOverride string) (*api.Job, error) {
	job, err := dsl.Parse(strings.NewReader(yaml))
	if err != nil {
		return nil, fmt.Errorf("invalid yaml: %w", err)
	}
	if dirOverride != "" {
		if job.Metadata.Annotations == nil {
			job.Metadata.Annotations = map[string]string{}
		}
		job.Metadata.Annotations["path"] = dirOverride
	}
	specJSON, err := json.Marshal(job.Spec)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}
	return s.store.UpsertJob(ctx, job.Metadata.QualifiedName(), job.APIVersion, specJSON)
}
```

Add `"context"` and `"fmt"` to the import block of `internal/controller/api_jobs.go`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestHandleApplyJob_UsesQualifiedName -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/api_jobs.go internal/controller/api_jobs_test.go
git commit -m "feat(controller): store jobs under qualified name on direct apply"
```

---

### Task 3: AppSource reconcile derives the directory and dedups on the qualified name

**Files:**
- Modify: `internal/controller/appsource_apply.go:46-60` (applyResource Job case)
- Modify: `internal/controller/appsource_reconciler.go:128-148` (reconcile loop)
- Create: `internal/controller/reldir.go`
- Test: `internal/controller/reldir_test.go`, `internal/controller/appsource_reconciler_test.go` (append)

**Interfaces:**
- Consumes: `dsl.QualifyName` (Task 1).
- Produces: `relDir(specPath, filePath string) string`; `applyResource(ctx, st, kind, dir, doc)` gains a `dir` parameter (empty for non-Job kinds).

- [ ] **Step 1: Write the failing unit test for relDir**

Create `internal/controller/reldir_test.go`:

```go
package controller

import "testing"

func TestRelDir(t *testing.T) {
	cases := []struct{ specPath, filePath, want string }{
		{"jobs/", "jobs/build.yaml", ""},
		{"jobs/", "jobs/team-a/build.yaml", "team-a"},
		{"jobs/", "jobs/team-b/edge/test.yaml", "team-b/edge"},
		{"jobs", "jobs/team-a/build.yaml", "team-a"},
		{"", "team-a/build.yaml", "team-a"},
	}
	for _, c := range cases {
		if got := relDir(c.specPath, c.filePath); got != c.want {
			t.Errorf("relDir(%q,%q)=%q want %q", c.specPath, c.filePath, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestRelDir -v`
Expected: FAIL — `undefined: relDir`.

- [ ] **Step 3: Implement relDir**

Create `internal/controller/reldir.go`:

```go
package controller

import (
	"path"
	"strings"
)

// relDir returns the directory of filePath relative to specPath (the AppSource
// root). "jobs/", "jobs/team-a/build.yaml" -> "team-a". A file directly under
// specPath yields "".
func relDir(specPath, filePath string) string {
	prefix := strings.Trim(specPath, "/")
	rel := strings.TrimPrefix(strings.TrimPrefix(filePath, prefix), "/")
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	return dir
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestRelDir -v`
Expected: PASS.

- [ ] **Step 5: Thread the directory into applyResource**

In `internal/controller/appsource_apply.go`, change the signature and the `Job` case only:

```go
func applyResource(ctx context.Context, st store.Store, kind, dir string, doc []byte) (string, error) {
	switch kind {
	case "Job":
		job, err := dsl.Parse(strings.NewReader(string(doc)))
		if err != nil {
			return "", err
		}
		if job.Metadata.Annotations == nil {
			job.Metadata.Annotations = map[string]string{}
		}
		job.Metadata.Annotations["path"] = dir
		specJSON, err := json.Marshal(job.Spec)
		if err != nil {
			return "", err
		}
		name := job.Metadata.QualifiedName()
		if _, err := st.UpsertJob(ctx, name, job.APIVersion, specJSON); err != nil {
			return "", fmt.Errorf("%w: %v", errStoreWrite, err)
		}
		return name, nil
```

Leave every other `case` unchanged (they ignore `dir`).

Then fix the three existing callers in `internal/controller/appsource_apply_test.go`
(lines 26, 39, 45) to pass `""` for the new `dir` argument:

```go
got, err := applyResource(ctx, pg, c.kind, "", []byte(c.doc))
```
```go
_, err := applyResource(ctx, pg, "Nope", "", []byte("kind: Nope"))
```
```go
_, err = applyResource(ctx, pg, "Job", "", []byte("kind: Job\nmetadata: {name: x}\nspec: {steps: []}"))
```

`QualifyName("", "j1")` returns `"j1"`, so `TestApplyResource_EachKind`'s
expected names are unchanged.

- [ ] **Step 6: Update the reconcile loop to compute dir and dedup on the qualified name**

In `internal/controller/appsource_reconciler.go`, replace the loop body at lines 128-148 with:

```go
	for _, fp := range paths {
		kind := probeKind(files[fp])
		dir := relDir(spec.Path, fp)
		refName := probeName(files[fp])
		if kind == "Job" {
			refName = dsl.QualifyName(dir, refName)
		}
		// Skip duplicates BEFORE writing to the store, so the first file (sorted)
		// wins. Dedup on the qualified name so team-a/build and team-b/build are
		// distinct, not collapsed.
		if ref := (store.ResourceRef{Kind: kind, Name: refName}); seen[ref] {
			slog.Warn("appsource reconciler: duplicate resource, keeping first", "name", src.Name, "kind", kind, "resource", ref.Name, "file", fp)
			continue
		}
		name, err := applyResource(ctx, st, kind, dir, files[fp])
		if err != nil {
			if errors.Is(err, errStoreWrite) {
				return fmt.Errorf("apply %s (%s): %w", kind, fp, err)
			}
			slog.Warn("appsource reconciler: skipping file", "name", src.Name, "file", fp, "kind", kind, "error", err)
			continue
		}
		ref := store.ResourceRef{Kind: kind, Name: name}
		seen[ref] = true
		current = append(current, ref)
	}
```

Ensure `internal/controller/appsource_reconciler.go` imports `"github.com/eirueimi/unified-cd/internal/dsl"` (it references `dsl.AppSourceSpec` already, so the import exists).

- [ ] **Step 7: Write the failing reconcile behavior test**

Append to `internal/controller/appsource_reconciler_test.go`:

```go
func TestReconciler_DirectoryBecomesQualifiedName(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	job := func(name string) []byte {
		return []byte("apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: " + name +
			"\nspec:\n  steps:\n    - name: c\n      run: \"true\"\n")
	}
	fetcher := &mockAppSourceFetcher{
		sha: "sha1",
		files: map[string][]byte{
			"jobs/team-a/build.yaml": job("build"),
			"jobs/team-b/build.yaml": job("build"),
			"jobs/root.yaml":         job("root"),
		},
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	for _, want := range []string{"team-a/build", "team-b/build", "root"} {
		j, err := pg.GetJob(ctx, want)
		require.NoErrorf(t, err, "expected job %q", want)
		assert.Equal(t, want, j.Name)
	}
}
```

- [ ] **Step 8: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestReconciler|TestRelDir' -v`
Expected: PASS. (Both `team-a/build` and `team-b/build` exist — the dedup fix prevents the second from being dropped.)

- [ ] **Step 9: Commit**

```bash
git add internal/controller/appsource_apply.go internal/controller/appsource_reconciler.go internal/controller/reldir.go internal/controller/reldir_test.go internal/controller/appsource_reconciler_test.go
git commit -m "feat(controller): appsource directory becomes the job qualified name"
```

---

### Task 4: API returns derived path and leaf

**Files:**
- Modify: `internal/api/types.go:23-30` (Job struct)
- Modify: `internal/controller/api_jobs.go` (handleListJobs, and set fields in handleGetJob — Task 5 later refactors handleGetJob into serveJob, which carries the same Path/Leaf line)
- Test: `internal/controller/api_jobs_test.go` (append)

**Interfaces:**
- Consumes: `dsl.SplitQualifiedName` (Task 1).
- Produces: `api.Job.Path`, `api.Job.Leaf` JSON fields.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/api_jobs_test.go`:

```go
func TestListJobs_DerivesPathAndLeaf(t *testing.T) {
	pg := store.NewTestPostgres(t)
	s := &Server{store: pg}
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "team-a/build", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	jobs, err := s.listJobsDecorated(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "team-a/build", jobs[0].Name)
	assert.Equal(t, "team-a", jobs[0].Path)
	assert.Equal(t, "build", jobs[0].Leaf)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestListJobs_DerivesPathAndLeaf -v`
Expected: FAIL — `s.listJobsDecorated undefined` and `jobs[0].Path undefined`.

- [ ] **Step 3: Add fields to api.Job**

In `internal/api/types.go`:

```go
type Job struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Path       string     `json:"path"`
	Leaf       string     `json:"leaf"`
	APIVersion string     `json:"apiVersion"`
	Spec       []byte     `json:"spec"`
	Inputs     []InputDef `json:"inputs,omitempty"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}
```

- [ ] **Step 4: Decorate list results**

In `internal/controller/api_jobs.go`, replace `handleListJobs` and add the helper:

```go
// handleListJobs returns all registered Jobs, decorated with path/leaf.
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.listJobsDecorated(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Server) listJobsDecorated(ctx context.Context) ([]api.Job, error) {
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	if jobs == nil {
		jobs = []api.Job{}
	}
	for i := range jobs {
		jobs[i].Inputs = specInputs(jobs[i].Spec)
		jobs[i].Path, jobs[i].Leaf = dsl.SplitQualifiedName(jobs[i].Name)
	}
	return jobs, nil
}
```

Also set the fields in `handleGetJob` (after `job.Inputs = specInputs(job.Spec)`):

```go
	job.Path, job.Leaf = dsl.SplitQualifiedName(job.Name)
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestListJobs_DerivesPathAndLeaf -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/api/types.go internal/controller/api_jobs.go internal/controller/api_jobs_test.go
git commit -m "feat(api): derive path/leaf from the job qualified name"
```

---

### Task 5: Catch-all routing for slash-containing job names

**Files:**
- Modify: `internal/controller/server.go:211-213` (routes)
- Modify: `internal/controller/api_jobs.go` (handleGetJob/handleGetJobYAML/handleDeleteJob → catch-all dispatch)
- Test: `internal/controller/api_jobs_test.go` (append)

**Interfaces:**
- Produces: `GET /api/v1/jobs/*` and `DELETE /api/v1/jobs/*` handle names containing `/`.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/api_jobs_test.go` (pure helper test — no DB needed):

```go
func TestExtractJobName(t *testing.T) {
	assert.Equal(t, "team-a/build", extractJobName("team-a/build"))
	assert.Equal(t, "team-a/build", extractJobName("/team-a/build"))

	name, isYAML := extractJobNameAndYAML("team-a/build/yaml")
	assert.Equal(t, "team-a/build", name)
	assert.True(t, isYAML)

	name2, isYAML2 := extractJobNameAndYAML("team-a/build")
	assert.Equal(t, "team-a/build", name2)
	assert.False(t, isYAML2)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestExtractJobName -v`
Expected: FAIL — `undefined: extractJobName`.

- [ ] **Step 3: Add the wildcard parsing helpers and dispatch handler**

In `internal/controller/api_jobs.go`, add:

```go
// extractJobName returns the catch-all wildcard as the job name.
func extractJobName(wild string) string {
	return strings.TrimPrefix(wild, "/")
}

// extractJobNameAndYAML strips an optional trailing "/yaml" segment, reporting
// whether it was present.
func extractJobNameAndYAML(wild string) (name string, yaml bool) {
	wild = strings.TrimPrefix(wild, "/")
	if strings.HasSuffix(wild, "/yaml") {
		return strings.TrimSuffix(wild, "/yaml"), true
	}
	return wild, false
}

// handleGetJobOrYAML dispatches GET /jobs/* to the job or its YAML.
func (s *Server) handleGetJobOrYAML(w http.ResponseWriter, r *http.Request) {
	name, yaml := extractJobNameAndYAML(chi.URLParam(r, "*"))
	if yaml {
		s.serveJobYAML(w, r, name)
		return
	}
	s.serveJob(w, r, name)
}
```

Refactor `handleGetJob`, `handleGetJobYAML`, and `handleDeleteJob` to read the wildcard and delegate to `name`-taking helpers:

```go
func (s *Server) serveJob(w http.ResponseWriter, r *http.Request, name string) {
	job, err := s.store.GetJob(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	job.Inputs = specInputs(job.Spec)
	job.Path, job.Leaf = dsl.SplitQualifiedName(job.Name)
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) serveJobYAML(w http.ResponseWriter, r *http.Request, name string) {
	job, err := s.store.GetJob(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	yamlBytes, err := specJSONToYAML(job.Spec)
	if err != nil {
		slog.Warn("job yaml render failed", "job", name, "error", err)
		http.Error(w, "render yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(yamlBytes)
}

func (s *Server) handleDeleteJob(w http.ResponseWriter, r *http.Request) {
	name := extractJobName(chi.URLParam(r, "*"))
	if err := s.store.DeleteJob(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

Delete the old `handleGetJob` and `handleGetJobYAML` function bodies (their logic now lives in `serveJob`/`serveJobYAML`).

- [ ] **Step 4: Update routes**

In `internal/controller/server.go`, replace lines 211-213:

```go
		r.With(view).Get("/jobs/*", s.handleGetJobOrYAML)
		r.With(dev).Delete("/jobs/*", s.handleDeleteJob)
```

(Keep `POST /jobs` and `GET /jobs` on lines 209-210 unchanged.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/controller/ -v`
Expected: PASS (whole controller package, including existing route tests).

- [ ] **Step 6: Note the edge case in a comment**

Add above `handleGetJobOrYAML`:

```go
// NOTE: a job whose leaf is literally "yaml" (qualified name ".../yaml") is
// unreachable via GET as a job — the suffix is read as the YAML discriminator.
// Such names are not expected; direct apply can still create/delete them.
```

- [ ] **Step 7: Commit**

```bash
git add internal/controller/server.go internal/controller/api_jobs.go internal/controller/api_jobs_test.go
git commit -m "feat(controller): catch-all routing for slash-containing job names"
```

---

### Task 6: Frontend — name-with-slash plumbing

**Files:**
- Modify: `web/src/lib/api.js` (add `jobPath`)
- Modify: `web/src/routes/JobDetail.svelte`, `web/src/routes/JobRun.svelte`, `web/src/routes/JobYaml.svelte`
- Modify: `web/src/routes/ScheduleList.svelte:34`, `web/src/routes/WebhookList.svelte:38`, `web/src/routes/RunDetail.svelte:210`
- Test: `web/src/lib/api.test.js` (append)

**Interfaces:**
- Produces: `jobPath(name)` — per-segment-encoded path for `/api/v1/jobs/<path>` URLs. Hash links elsewhere use `encodeURIComponent(name)`; route components decode `params.name`.

- [ ] **Step 1: Write the failing test**

Append to `web/src/lib/api.test.js`:

```js
import { jobPath } from './api.js';

describe('jobPath', () => {
  it('encodes segments but keeps slashes', () => {
    expect(jobPath('team-a/build')).toBe('team-a/build');
    expect(jobPath('hello')).toBe('hello');
    expect(jobPath('a b/c')).toBe('a%20b/c');
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/lib/api.test.js`
Expected: FAIL — `jobPath is not a function`.

- [ ] **Step 3: Add jobPath**

Append to `web/src/lib/api.js`:

```js
// jobPath encodes a qualified job name for use as a URL path under
// /api/v1/jobs/ — each segment is percent-encoded but the slashes are kept
// literal so the controller's catch-all route captures the full name.
export function jobPath(name) {
  return String(name).split('/').map(encodeURIComponent).join('/');
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npx vitest run src/lib/api.test.js`
Expected: PASS.

- [ ] **Step 5: Decode params.name and use jobPath in the three job routes**

In `web/src/routes/JobDetail.svelte`, change line 8:

```js
  $: jobName = decodeURIComponent(params.name);
```

Update the tab links (near lines 49-51) to encode the name:

```svelte
    <a href="#/jobs/{encodeURIComponent(jobName)}" class="tab-link tab-active">History</a>
    <a href="#/jobs/{encodeURIComponent(jobName)}/run" class="tab-link">▶ Run</a>
    <a href="#/jobs/{encodeURIComponent(jobName)}/yaml" class="tab-link">YAML</a>
```

(The runs fetch at line 30 already `encodeURIComponent(jobName)` for the `?jobName=` query — leave it.)

In `web/src/routes/JobRun.svelte`: change line 8 to `$: jobName = decodeURIComponent(params.name);`, change the job fetch (line 20) to:

```js
      const job = await apiFetch("/api/v1/jobs/" + jobPath(jobName));
```

Add `jobPath` to its `import { apiFetch } from "../lib/api.js";` line → `import { apiFetch, jobPath } from "../lib/api.js";`. Update its three tab links (lines 76-78) to `#/jobs/{encodeURIComponent(jobName)}...` as above. The POST body `{ jobName, ... }` (line 55) stays raw.

In `web/src/routes/JobYaml.svelte`: change line 7 to `$: jobName = decodeURIComponent(params.name);`, change the yaml fetch (line 19) to:

```js
                "/api/v1/jobs/" + jobPath(jobName) + "/yaml",
```

Add `jobPath` to its api import. Update its three tab links (lines 38-40) to `#/jobs/{encodeURIComponent(jobName)}...`.

- [ ] **Step 6: Encode hash links that point at jobs from other pages**

- `web/src/routes/ScheduleList.svelte:34` → `<td><a href="#/jobs/{encodeURIComponent(s.jobName)}">{s.jobName}</a></td>`
- `web/src/routes/WebhookList.svelte:38` → `<td><a href="#/jobs/{encodeURIComponent(w.jobName)}">{w.jobName}</a></td>`
- `web/src/routes/RunDetail.svelte:210` → `{#if run}<a href="#/jobs/{encodeURIComponent(run.jobName)}">← {run.jobName}</a>{/if}`

- [ ] **Step 7: Run the frontend test suite**

Run: `cd web && npx vitest run`
Expected: PASS (existing suites plus the new `jobPath` test).

- [ ] **Step 8: Commit**

```bash
git add web/src/lib/api.js web/src/lib/api.test.js web/src/routes/JobDetail.svelte web/src/routes/JobRun.svelte web/src/routes/JobYaml.svelte web/src/routes/ScheduleList.svelte web/src/routes/WebhookList.svelte web/src/routes/RunDetail.svelte
git commit -m "feat(web): route and fetch slash-containing job names"
```

---

### Task 7: Frontend — directory tree in JobList

**Files:**
- Modify: `web/src/lib/utils.js` (add `buildJobTree`, `flattenJobTree`)
- Modify: `web/src/routes/JobList.svelte`
- Test: `web/src/lib/utils.test.js` (append)

**Interfaces:**
- Consumes: `Job` list from `GET /api/v1/jobs` with `path`/`leaf` (Task 4); `encodeURIComponent(name)` for hash links.
- Produces: `buildJobTree(jobs)` and `flattenJobTree(root, collapsed, query)`.

- [ ] **Step 1: Write the failing test**

Append to `web/src/lib/utils.test.js`:

```js
import { buildJobTree, flattenJobTree } from './utils.js';

const jobs = [
  { name: 'team-a/build', path: 'team-a', leaf: 'build' },
  { name: 'team-a/deploy', path: 'team-a', leaf: 'deploy' },
  { name: 'team-b/edge/test', path: 'team-b/edge', leaf: 'test' },
  { name: 'hello', path: '', leaf: 'hello' },
];

describe('job tree', () => {
  it('flattens folders and root jobs (all expanded)', () => {
    const rows = flattenJobTree(buildJobTree(jobs), new Set(), '');
    const shape = rows.map((r) => r.kind === 'folder' ? `D${r.depth}:${r.name}` : `J${r.depth}:${r.job.leaf}`);
    expect(shape).toEqual([
      'D0:team-a', 'J1:build', 'J1:deploy',
      'D0:team-b', 'D1:edge', 'J2:test',
      'J0:hello',
    ]);
  });

  it('hides collapsed folder children', () => {
    const rows = flattenJobTree(buildJobTree(jobs), new Set(['team-a']), '');
    expect(rows.some((r) => r.kind === 'job' && r.job.leaf === 'build')).toBe(false);
    expect(rows.some((r) => r.kind === 'folder' && r.name === 'team-a')).toBe(true);
  });

  it('filter keeps matches and their ancestor folders, ignoring collapse', () => {
    const rows = flattenJobTree(buildJobTree(jobs), new Set(['team-b', 'team-b/edge']), 'test');
    const shape = rows.map((r) => r.kind === 'folder' ? `D:${r.name}` : `J:${r.job.leaf}`);
    expect(shape).toEqual(['D:team-b', 'D:edge', 'J:test']);
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && npx vitest run src/lib/utils.test.js`
Expected: FAIL — `buildJobTree is not a function`.

- [ ] **Step 3: Implement the tree helpers**

Append to `web/src/lib/utils.js`:

```js
// buildJobTree groups a flat job list into a nested folder tree keyed by the
// job's `path`. Root-level jobs (empty path) attach to the returned root node.
export function buildJobTree(jobs) {
  const root = { name: '', path: '', folders: new Map(), jobs: [] };
  for (const j of jobs) {
    const segs = j.path ? j.path.split('/') : [];
    let node = root, acc = '';
    for (const seg of segs) {
      acc = acc ? acc + '/' + seg : seg;
      if (!node.folders.has(seg)) node.folders.set(seg, { name: seg, path: acc, folders: new Map(), jobs: [] });
      node = node.folders.get(seg);
    }
    node.jobs.push(j);
  }
  return root;
}

// flattenJobTree produces ordered display rows. Folders come before their
// sibling jobs, both sorted by name. A folder is open when the query is
// non-empty (search auto-expands) or its path is not in `collapsed`. With a
// query, only matching jobs and their ancestor folders are emitted.
export function flattenJobTree(root, collapsed, query) {
  const rows = [];
  const q = (query || '').toLowerCase();
  const jobMatches = (j) => !q || j.name.toLowerCase().includes(q);
  const folderHasMatch = (node) =>
    node.jobs.some(jobMatches) || [...node.folders.values()].some(folderHasMatch);
  function walk(node, depth) {
    for (const name of [...node.folders.keys()].sort()) {
      const f = node.folders.get(name);
      if (q && !folderHasMatch(f)) continue;
      rows.push({ kind: 'folder', name, path: f.path, depth });
      const open = q ? true : !collapsed.has(f.path);
      if (open) walk(f, depth + 1);
    }
    for (const j of [...node.jobs].filter(jobMatches).sort((a, b) => a.name.localeCompare(b.name))) {
      rows.push({ kind: 'job', job: j, depth });
    }
  }
  walk(root, 0);
  return rows;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && npx vitest run src/lib/utils.test.js`
Expected: PASS.

- [ ] **Step 5: Rewrite the JobList table body as a tree**

In `web/src/routes/JobList.svelte`, add to the imports (line 5):

```js
  import { fmtTime, fmtRelative, statusBadge, matchesFilter, buildJobTree, flattenJobTree } from '../lib/utils.js';
```

Add collapse state and reactive rows to the `<script>` (after line 17's `filteredJobs` — replace that line):

```js
  let collapsed = new Set();
  $: rows = flattenJobTree(buildJobTree(jobs), collapsed, filterQuery);

  function toggleFolder(path) {
    if (collapsed.has(path)) collapsed.delete(path); else collapsed.add(path);
    collapsed = collapsed;
  }
```

Replace the table (lines 82-130) with a tree table:

```svelte
  <table>
    <thead><tr><th>Name</th><th>Updated</th><th></th></tr></thead>
    <tbody>
      {#each rows as row (row.kind === 'folder' ? 'D:' + row.path : 'J:' + row.job.name)}
        {#if row.kind === 'folder'}
          <tr style="cursor:pointer" on:click={() => toggleFolder(row.path)}>
            <td colspan="3" style="padding-left:{0.75 + row.depth * 1.4}rem">
              <span class="meta">{collapsed.has(row.path) && !filterQuery ? '▸' : '▾'}</span>
              📁 {row.name}
            </td>
          </tr>
        {:else}
          <tr style="cursor:pointer" on:click={() => toggleExpand(row.job.name)}>
            <td style="padding-left:{0.75 + (row.depth + 1) * 1.4}rem">
              <a href="#/jobs/{encodeURIComponent(row.job.name)}" on:click|stopPropagation>{row.job.leaf}</a>
              {#if activeRunsByJob[row.job.name]?.length}
                <span class="badge badge-running" style="margin-left:0.5rem">
                  ● 実行中 {activeRunsByJob[row.job.name].length > 1 ? `(${activeRunsByJob[row.job.name].length})` : ''}
                </span>
              {/if}
            </td>
            <td class="meta">{fmtTime(row.job.updatedAt)}</td>
            <td><a href="#/jobs/{encodeURIComponent(row.job.name)}" class="btn" on:click|stopPropagation>Runs →</a></td>
          </tr>
          {#if expandedJob === row.job.name}
            <tr>
              <td colspan="3" style="padding:0">
                <div style="background:var(--bg-elev);border-top:1px solid var(--border);padding:0.5rem 1rem">
                  {#if expandedLoading}
                    <div class="meta" style="padding:0.25rem 0">Loading...</div>
                  {:else if !expandedRuns.length}
                    <div class="meta" style="padding:0.25rem 0">Runはありません。</div>
                  {:else}
                    {#each expandedRuns as r (r.id)}
                      <div
                        style="display:flex;align-items:center;gap:0.75rem;padding:0.3rem 0;cursor:pointer"
                        on:click|stopPropagation={() => { window.location.hash = '/runs/' + r.id; }}
                      >
                        <span class={statusBadge(r.status)}>{r.status}</span>
                        <span class="meta" style="font-family:monospace;font-size:0.8rem">{r.id.slice(0,8)}</span>
                        <span class="meta">{fmtRelative(r.createdAt)}</span>
                      </div>
                    {/each}
                    <div style="margin-top:0.25rem">
                      <a href="#/jobs/{encodeURIComponent(row.job.name)}" class="meta" style="font-size:0.8rem" on:click|stopPropagation>すべて見る →</a>
                    </div>
                  {/if}
                </div>
              </td>
            </tr>
          {/if}
        {/if}
      {/each}
    </tbody>
  </table>
```

Replace the empty-filter guard (lines 79-81) so it checks `rows`:

```svelte
  {#if !rows.length}
    <div class="empty">No jobs match "{filterQuery}".</div>
  {:else}
```

(The `matchesFilter` import is now unused in this file — remove it from the import if the linter complains; harmless otherwise.)

- [ ] **Step 6: Run the frontend suite and a build**

Run: `cd web && npx vitest run && npm run build`
Expected: tests PASS; build succeeds with no errors.

- [ ] **Step 7: Commit**

```bash
git add web/src/lib/utils.js web/src/lib/utils.test.js web/src/routes/JobList.svelte
git commit -m "feat(web): render jobs as a collapsible directory tree"
```

---

### Task 8: Docs and example

**Files:**
- Modify: `docs/jobs.md` (annotations.path + qualified name)
- Modify: `examples/jobs/` (add a nested example) or `examples/resources/appsource.yaml` comment

- [ ] **Step 1: Document the annotation and tree behavior**

In `docs/jobs.md`, under the metadata description, add:

```markdown
### Hierarchical grouping (annotations.path)

A job's position in the Web UI tree comes from `metadata.annotations.path`.
Jobs synced by an AppSource get this set automatically from their directory
(relative to the AppSource `spec.path`), so `jobs/team-a/build.yaml` shows as
`build` under a `team-a` folder. The stored, unique job name is the *qualified*
name `team-a/build` — trigger it with `unified-cli run trigger team-a/build`.
Jobs applied directly with no `path` appear at the tree root.
```

- [ ] **Step 2: Add a nested example**

Create `examples/jobs/team-a/build.yaml`:

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
  annotations:
    path: team-a   # optional for direct apply; AppSource sets this from the directory
spec:
  agentSelector:
    - kind:docker
  steps:
    - name: build
      run: echo "building team-a/build"
```

- [ ] **Step 3: Commit**

```bash
git add docs/jobs.md examples/jobs/team-a/build.yaml
git commit -m "docs: qualified job names and directory-tree grouping"
```

---

## Final verification

- [ ] Run the full Go suite: `go test ./...` — Expected: PASS.
- [ ] Run the frontend suite and build: `cd web && npx vitest run && npm run build` — Expected: PASS.
- [ ] Manual smoke (optional): apply `examples/jobs/team-a/build.yaml`, confirm `unified-cli jobs list` shows `team-a/build`, `unified-cli run trigger team-a/build` starts a run, and the Web UI Jobs page shows `build` under a `team-a` folder.
