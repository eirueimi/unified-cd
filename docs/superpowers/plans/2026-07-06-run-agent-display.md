# Run Detail: Show Executing Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface the claiming agent's ID on the Run Detail page, linked to the existing agent detail page, by threading the already-stored `claimed_by` value through the API and UI.

**Architecture:** No new columns or endpoints. `api.Run` gains a `ClaimedBy` field; `GetRun` selects the existing `claimed_by` column (NULL until claim); `RunDetail.svelte` renders a guarded "Agent" metadata row linking to `#/agents/<id>`.

**Tech Stack:** Go (pgx v5), Svelte, vitest + @testing-library/svelte, real-Postgres tests via `store.NewTestPostgres`.

Spec: `docs/superpowers/specs/2026-07-06-run-agent-display-design.md`

## Global Constraints

- English only (code, comments, commit messages).
- No new DB columns, no new API endpoints, no `Migrate` change.
- `ClaimedBy` empty string means "never claimed" — the UI must not render the Agent row in that case.
- Agent link href format exactly `#/agents/<claimedBy>` (matches the existing `/agents/:id` route and the `calledBy` link style).
- `gofmt -w` touched Go files; Go tests via `NewTestPostgres` need Docker; web tests run in the vite container (`docker compose exec -T vite npx vitest run`) because host node_modules are Linux-built.

---

### Task 1: expose ClaimedBy from the API and store

**Files:**
- Modify: `internal/api/types.go` (Run struct, ~line 50)
- Modify: `internal/store/postgres.go` (GetRun, ~line 197-216)
- Test: `internal/store/postgres_getrun_test.go` (new)

**Interfaces:**
- Consumes: existing `UpsertJob`, `CreateRun`, `TransitionPendingToQueued`, `ClaimNextRun`, `GetRun`.
- Produces: `api.Run.ClaimedBy string` (json `claimedBy,omitempty`), populated by `GetRun` from the `claimed_by` column.

- [ ] **Step 1: Write the failing test**

`internal/store/postgres_getrun_test.go`:

```go
package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetRun_ClaimedByReflectsClaimingAgent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// Unclaimed run: ClaimedBy is empty.
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ClaimedBy, "a freshly created run has no claiming agent")

	// After claim: ClaimedBy is the claiming agent's ID.
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	claimed, err := pg.ClaimNextRun(ctx, "agent-xyz", nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)

	got, err = pg.GetRun(ctx, claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent-xyz", got.ClaimedBy)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestGetRun_ClaimedByReflectsClaimingAgent -v`
Expected: FAIL — `got.ClaimedBy undefined (type *api.Run has no field or method ClaimedBy)` (compile error).

- [ ] **Step 3: Add the API field**

In `internal/api/types.go`, in the `Run` struct, add the field after `TriggeredBy`:

```go
type Run struct {
	ID          string            `json:"id"`
	JobName     string            `json:"jobName"`
	Status      RunStatus         `json:"status"`
	Params      map[string]string `json:"params"`
	CreatedAt   time.Time         `json:"createdAt"`
	UpdatedAt   time.Time         `json:"updatedAt"`
	TriggeredBy string            `json:"triggeredBy"`
	ClaimedBy   string            `json:"claimedBy,omitempty"` // Claiming agent's ID; empty until claimed.
	CalledBy    *CalledBy         `json:"calledBy,omitempty"`
}
```

- [ ] **Step 4: Select claimed_by in GetRun**

In `internal/store/postgres.go`, update `GetRun` (the whole function) to select and scan the nullable column via a `*string` (pgx sets it to nil when the column is NULL):

```go
func (p *Postgres) GetRun(ctx context.Context, id string) (*api.Run, error) {
	const q = `SELECT id, job_name, status, params, created_at, updated_at, triggered_by, claimed_by FROM runs WHERE id = $1`
	var r api.Run
	var paramsOut []byte
	var status string
	var claimedBy *string
	err := p.pool.QueryRow(ctx, q, id).
		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy, &claimedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrRunNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	r.Status = api.RunStatus(status)
	if claimedBy != nil {
		r.ClaimedBy = *claimedBy
	}
	_ = json.Unmarshal(paramsOut, &r.Params)
	if r.Params == nil {
		r.Params = map[string]string{}
	}
	return &r, nil
}
```

- [ ] **Step 5: Run the test to verify it passes, then build**

Run: `go test ./internal/store/ -run TestGetRun_ClaimedByReflectsClaimingAgent -v`
Expected: PASS.
Run: `go build ./... && go test ./internal/store/ ./internal/controller/ -count=1`
Expected: PASS (no existing test regresses — `GetRun` is used by the run-detail API handler, which passes the struct through as JSON).

- [ ] **Step 6: Commit**

```bash
git add internal/api/types.go internal/store/postgres.go internal/store/postgres_getrun_test.go
git commit -m "feat(api): expose claiming agent id (ClaimedBy) on GetRun"
```

---

### Task 2: render the Agent row on the Run Detail page

**Files:**
- Modify: `web/src/routes/RunDetail.svelte` (metadata block, near the "Triggered by" field ~line 498)
- Test: `web/src/routes/RunDetail.test.js` (add two cases)

**Interfaces:**
- Consumes: `run.claimedBy` from the run JSON (Task 1). The existing `/agents/:id` route (`App.svelte`) renders `AgentDetail`.
- Produces: an `.run-agent` element containing an `<a href="#/agents/<id>">` when `run.claimedBy` is set.

- [ ] **Step 1: Write the failing tests**

Append inside the top-level `describe(...)` block in `web/src/routes/RunDetail.test.js` (it already imports `render`, `vi`, and defines `jsonResponse` / `emptyEventsResponse`):

```js
  it("renders an Agent link when run.claimedBy is present", async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({
        id: 'run-3', status: 'Running', jobName: 'job-a', triggeredBy: 'x',
        createdAt: null, params: {}, claimedBy: 'k8s-agent-1',
      });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-3' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('.run-agent')).toBeTruthy();
    });
    const link = container.querySelector('.run-agent a');
    expect(link).toBeTruthy();
    expect(link.getAttribute('href')).toBe('#/agents/k8s-agent-1');
    expect(link.textContent).toContain('k8s-agent-1');
  });

  it("omits the Agent row when run.claimedBy is absent", async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({
        id: 'run-4', status: 'Queued', jobName: 'job-a', triggeredBy: 'x',
        createdAt: null, params: {},
      });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-4' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('job-a');
    });
    expect(container.querySelector('.run-agent')).toBeFalsy();
  });
```

Note: if `emptyEventsResponse` is not the exact helper name in this file, use whatever the existing tests use for the `/events` SSE stub (check the other tests in the file — they all stub `/events`). Match it verbatim.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `docker compose exec -T vite npx vitest run src/routes/RunDetail.test.js`
Expected: FAIL — the two new cases fail (`.run-agent` never appears) while existing cases pass.

- [ ] **Step 3: Add the Agent metadata row**

In `web/src/routes/RunDetail.svelte`, in the metadata block, immediately after the "Triggered by" `<div>` (the one rendering `{run.triggeredBy}`), add:

```svelte
      {#if run.claimedBy}
        <div class="run-agent">
          <div class="meta">Agent</div>
          <div><a href="#/agents/{run.claimedBy}">{run.claimedBy} ↗</a></div>
        </div>
      {/if}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `docker compose exec -T vite npx vitest run src/routes/RunDetail.test.js`
Expected: PASS (all cases). Then run the full web suite to confirm no regression:
Run: `docker compose exec -T vite npx vitest run`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js
git commit -m "feat(web): show claiming agent on the run detail page, linked to agent detail"
```

---

## Verification note

The dev stack is running on this machine. After both tasks, a quick manual
check: restart the controller (`docker compose restart controller`; wait for
air to rebuild) and open a Running/finished run in the UI — the metadata panel
shows an "Agent" row linking to that agent's page. Unclaimed (Queued) runs show
no Agent row.
