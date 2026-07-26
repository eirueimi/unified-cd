# Call Step Child Status Badge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a call step's run-detail status follow its linked child run instead of a stale parent step report.

**Architecture:** Resolve the effective status in `Postgres.GetRunSteps` by left-joining `step_reports.child_run_id` to `runs.id`. The existing API and web UI keep their current shape and automatically receive the corrected status.

**Tech Stack:** Go 1.24, PostgreSQL, pgx, testify

## Global Constraints

- Once a call step has a valid child run, the child run's current status is authoritative.
- Steps without a child run and links to missing child rows fall back to `step_reports.status`.
- Do not change step timing, exit-code fields, API types, the database schema, or the web UI.
- Keep all repository text in English and do not include personally identifiable information.
- Update `docs/jobs.md`; examples and templates do not change because the DSL and execution semantics are unchanged.

---

### Task 1: Derive Call Step Status From the Child Run

**Files:**
- Modify: `internal/store/postgres_callrun_test.go`
- Modify: `internal/store/postgres.go:728-750`
- Modify: `docs/jobs.md:669-680`

**Interfaces:**
- Consumes: `Postgres.GetRunSteps(ctx context.Context, runID string) ([]api.StepReport, error)`, `step_reports.child_run_id`, and `runs.status`.
- Produces: the existing `api.StepReport.Status string`, populated from the linked child run when present and from the stored step report otherwise.

- [ ] **Step 1: Write the failing regression test**

Add this integration test to `internal/store/postgres_callrun_test.go`:

```go
func TestStepReport_ChildRunStatusIsAuthoritative(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	p := NewTestPostgres(t)
	ctx := t.Context()

	parent := mustCreateRun(t, p, "status-parent-job")
	child := mustCreateRun(t, p, "status-child-job")

	require.NoError(t, p.UpsertStepReport(
		ctx, parent, 0, 0, "call-child", "", "Running",
		nil, nil, nil, child, "status-child-job",
	))
	require.NoError(t, p.UpsertStepReport(
		ctx, parent, 1, 1, "plain-step", "", "Succeeded",
		nil, nil, nil, "", "",
	))

	steps, err := p.GetRunSteps(ctx, parent)
	require.NoError(t, err)
	require.Len(t, steps, 2)
	assert.Equal(t, "Pending", steps[0].Status)
	assert.Equal(t, "Succeeded", steps[1].Status)

	require.NoError(t, p.MarkRunRunning(ctx, child))
	steps, err = p.GetRunSteps(ctx, parent)
	require.NoError(t, err)
	assert.Equal(t, "Running", steps[0].Status)

	require.NoError(t, p.MarkRunFinished(ctx, child, api.RunFailed))
	steps, err = p.GetRunSteps(ctx, parent)
	require.NoError(t, err)
	assert.Equal(t, "Failed", steps[0].Status)
}
```

Add the missing API import:

```go
"github.com/eirueimi/unified-cd/internal/api"
```

The production mutation this test catches is removing the child-run join or selecting `step_reports.status` unconditionally. The expected status literals are derived from the child transitions performed by the test, not from production helpers.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```bash
go test ./internal/store -run TestStepReport_ChildRunStatusIsAuthoritative -count=1
```

Expected: FAIL on the first status assertion with `expected: "Pending"` and `actual: "Running"`, proving the current query returns the stale parent step report.

- [ ] **Step 3: Implement the minimal query change**

Replace the query in `Postgres.GetRunSteps` with:

```go
const q = `
	SELECT sr.step_index, sr.stage_index, sr.step_name,
	       COALESCE(child.status, sr.status),
	       sr.exit_code, sr.started_at, sr.ended_at, sr.variant,
	       COALESCE(sr.child_run_id::text, ''), COALESCE(sr.call_job_name, '')
	FROM step_reports sr
	LEFT JOIN runs child ON child.id = sr.child_run_id
	WHERE sr.run_id = $1
	ORDER BY sr.step_index, sr.variant;
`
```

Keep the existing scan order and return behavior unchanged.

- [ ] **Step 4: Run the focused test and verify GREEN**

Run:

```bash
go test ./internal/store -run TestStepReport_ChildRunStatusIsAuthoritative -count=1
```

Expected: PASS.

- [ ] **Step 5: Document the status source**

After the paragraph beginning `` `call` steps wait for the called job to complete``
in `docs/jobs.md`, add:

```markdown
Once the child run exists, the call step's status badge in the run detail view
follows the child run's current status. This remains accurate if the parent
agent stops before it can submit a final step report.
```

- [ ] **Step 6: Run package and formatting verification**

Run:

```bash
gofmt -w internal/store/postgres_callrun_test.go
go test ./internal/store -count=1
go test ./internal/controller -count=1
git diff --check
```

Expected: both Go package suites PASS and `git diff --check` exits 0 without output.

- [ ] **Step 7: Commit the implementation**

```bash
git add internal/store/postgres.go internal/store/postgres_callrun_test.go docs/jobs.md
git commit -m "fix: derive call step status from child run"
```

- [ ] **Step 8: Perform final branch verification**

Run:

```bash
go test ./internal/store ./internal/controller -count=1
git status --short --branch
```

Expected: both packages PASS and the branch is clean.
