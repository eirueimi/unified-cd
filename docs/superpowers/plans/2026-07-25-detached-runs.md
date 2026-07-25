# Detached Runs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a job opt into `spec.detached: true` so its runs are exempt from the agent's `MaxConcurrent` budget (claimed via a separate `MaxDetachedConcurrent` pool) and get an independent per-run workspace, breaking the `call:` slot deadlock.

**Architecture:** A new persisted `detached` boolean on the run row is derived from the job spec at run creation. `ClaimNextRun` gains a `detached` filter so agents claim normal and detached runs from separate budgets. The host agent adds a second pool of claim goroutines for detached runs with per-run workspaces; the k8s agent claims detached runs outside its `MaxConcurrent` semaphore. Detached does not change pod allocation (Approach A).

**Tech Stack:** Go, PostgreSQL (pgx), existing DSL/store/controller/agent packages.

## Global Constraints

- All code, comments, commit messages, and tests in English. No PII. (AGENTS.md)
- Work in the current worktree; never commit to main. Commit trailer: `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- TDD: write the failing test first, watch it fail, implement minimally, watch it pass, commit.
- `detached` defaults to `false` everywhere — the feature is fully backward compatible and opt-in.
- Identifier/flag values copied verbatim from the spec: flag `spec.detached`; agent config `maxDetachedConcurrent` / `--max-detached-concurrent` / `UNIFIED_AGENT_MAX_DETACHED`; default `16`, `0` = off, `-1` = unlimited.
- `native: true` + `detached: true` is **allowed** — the two are orthogonal (execution mode vs concurrency accounting); no validation error.

---

## File structure

- `internal/dsl/types.go` — add `Detached bool` to `Spec` (orthogonal to `native`).
- `internal/store/migrations/017_run_detached.up.sql` / `.down.sql` — new `detached` column.
- `internal/store/postgres.go` — `CreateRun` persists `detached`; `ClaimNextRun` filters by it.
- `internal/store/store.go` — update the `ClaimNextRun` interface signature.
- `internal/controller/api_agent.go` — claim handler reads `?kind=` and passes the flag.
- `internal/agent/client.go` — `Claim` sends the kind.
- `internal/config/agent.go`, `cmd/unified-cd-agent/main.go` — `MaxDetachedConcurrent` plumbing.
- `internal/agent/agent.go` — detached claim pool + per-run workspace.
- `internal/k8sagent/config.go`, `internal/k8sagent/agent.go` — detached semaphore exemption.
- `docs/jobs.md`, `docs/configuration.md` — documentation.

---

### Task 1: DSL `detached` field

**Files:**
- Modify: `internal/dsl/types.go` (the `Spec` struct, ~line 23)
- Test: `internal/dsl/parse_test.go`

**Interfaces:**
- Produces: `dsl.Spec.Detached bool` (yaml `detached`).

Note: `native: true` + `detached: true` is intentionally allowed — the two are
orthogonal (execution mode vs concurrency accounting), so there is no validation
rule here.

- [ ] **Step 1: Write the failing test**

```go
// in internal/dsl/parse_test.go
func TestParse_Detached_ParsesAndDefaultsFalse(t *testing.T) {
	job, err := Parse(strings.NewReader(`apiVersion: unified-cd/v1
kind: Job
metadata: {name: orch}
spec:
  detached: true
  steps:
    - {name: s, call: {job: child}}`))
	require.NoError(t, err)
	assert.True(t, job.Spec.Detached)

	job2, err := Parse(strings.NewReader(`apiVersion: unified-cd/v1
kind: Job
metadata: {name: normal}
spec:
  steps:
    - {name: s, run: "true"}`))
	require.NoError(t, err)
	assert.False(t, job2.Spec.Detached, "detached defaults to false")
}

func TestParse_Detached_AllowedWithNative(t *testing.T) {
	_, err := Parse(strings.NewReader(`apiVersion: unified-cd/v1
kind: Job
metadata: {name: native-orch}
spec:
  native: true
  detached: true
  steps:
    - {name: s, call: {job: child}}`))
	require.NoError(t, err, "native and detached are orthogonal and may be combined")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dsl/ -run 'TestParse_Detached' -count=1`
Expected: FAIL (unknown field `Detached`).

- [ ] **Step 3: Add the field**

In `internal/dsl/types.go`, in the `Spec` struct next to `Native`:

```go
	// Detached marks this job's runs as lightweight orchestrators: they do not
	// consume an agent's MaxConcurrent budget and get an independent per-run
	// workspace. Intended for jobs that mostly issue call: steps and wait.
	// Orthogonal to Native — a native host orchestrator may be detached.
	Detached bool `yaml:"detached,omitempty"`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -run 'TestParse_Detached' -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/types.go internal/dsl/parse_test.go
git commit -m "feat(dsl): add spec.detached (orthogonal to native)"
```

---

### Task 2: DB migration — `detached` column on `runs`

**Files:**
- Create: `internal/store/migrations/017_run_detached.up.sql`
- Create: `internal/store/migrations/017_run_detached.down.sql`

**Interfaces:**
- Produces: `runs.detached BOOLEAN NOT NULL DEFAULT false`.

- [ ] **Step 1: Write the up migration**

`internal/store/migrations/017_run_detached.up.sql`:

```sql
ALTER TABLE runs ADD COLUMN detached BOOLEAN NOT NULL DEFAULT false;
-- Partial index so the claim query's detached filter stays cheap on the hot
-- Queued path.
CREATE INDEX idx_runs_queued_detached ON runs (detached) WHERE status = 'Queued';
```

- [ ] **Step 2: Write the down migration**

`internal/store/migrations/017_run_detached.down.sql`:

```sql
DROP INDEX IF EXISTS idx_runs_queued_detached;
ALTER TABLE runs DROP COLUMN IF EXISTS detached;
```

- [ ] **Step 3: Verify migrations apply**

Run: `go test ./internal/store/ -run 'TestMigrations|TestPostgres_CreateRun' -count=1`
Expected: PASS (the test harness applies all migrations against a dockertest Postgres; requires Docker).

- [ ] **Step 4: Commit**

```bash
git add internal/store/migrations/017_run_detached.up.sql internal/store/migrations/017_run_detached.down.sql
git commit -m "feat(store): add runs.detached column (migration 017)"
```

---

### Task 3: `CreateRun` persists `detached` from the spec

**Files:**
- Modify: `internal/store/postgres.go` (`CreateRun`, ~line 137)
- Test: `internal/store/postgres_runs_test.go`

**Interfaces:**
- Consumes: `dsl.Spec.Detached` (Task 1), `runs.detached` (Task 2).
- Produces: rows created with `detached` set from the run's spec.

- [ ] **Step 1: Write the failing test**

```go
// in internal/store/postgres_runs_test.go
func TestPostgres_CreateRun_PersistsDetached(t *testing.T) {
	pg := newTestStore(t) // existing helper
	ctx := t.Context()
	spec := []byte(`{"detached":true,"steps":[{"name":"s","call":{"job":"c"}}]}`)
	run, err := pg.CreateRun(ctx, "orch", nil, spec, nil, nil, "")
	require.NoError(t, err)
	var detached bool
	require.NoError(t, pg.Pool().QueryRow(ctx, `SELECT detached FROM runs WHERE id=$1`, run.ID).Scan(&detached))
	assert.True(t, detached)
}
```

(If there is no `Pool()` accessor, query via the existing test helper used elsewhere in this file to read a run column.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestPostgres_CreateRun_PersistsDetached -count=1`
Expected: FAIL (column always false; not inserted).

- [ ] **Step 3: Implement**

In `internal/store/postgres.go` `CreateRun`, after marshaling params and before the INSERT, derive detached from the spec:

```go
	var detached bool
	if len(spec) > 0 {
		var s dsl.Spec
		if json.Unmarshal(spec, &s) == nil {
			detached = s.Detached
		}
	}
```

Change the INSERT to include the column:

```go
	const q = `
		INSERT INTO runs(job_name, params, spec, agent_selector, required_caps, triggered_by, detached)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, job_name, status, params, created_at, updated_at, triggered_by;
	`
	// ...
	err = p.pool.QueryRow(ctx, q, jobName, paramsJSON, spec, agentSelector, requiredCaps, triggeredBy, detached).
		Scan(&r.ID, &r.JobName, &status, &paramsOut, &r.CreatedAt, &r.UpdatedAt, &r.TriggeredBy)
```

Ensure `github.com/eirueimi/unified-cd/internal/dsl` is imported in postgres.go (it already is for other spec parsing).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestPostgres_CreateRun_PersistsDetached -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/postgres.go internal/store/postgres_runs_test.go
git commit -m "feat(store): persist detached on CreateRun from spec"
```

---

### Task 4: `ClaimNextRun` filters by `detached`

**Files:**
- Modify: `internal/store/store.go` (the `ClaimNextRun` interface method)
- Modify: `internal/store/postgres.go` (`ClaimNextRun`, ~line 576)
- Modify: every caller (`internal/controller/api_agent.go:162`, and tests that call it directly)
- Test: `internal/store/postgres_concurrency_test.go` (or a new `postgres_claim_test.go`)

**Interfaces:**
- Produces: `ClaimNextRun(ctx context.Context, agentID string, agentLabels []string, detached bool) (*ClaimedRun, error)` — returns only runs whose `detached` equals the argument.

- [ ] **Step 1: Write the failing test**

```go
// in internal/store/postgres_concurrency_test.go
func TestPostgres_ClaimNextRun_DetachedFilter(t *testing.T) {
	pg := newTestStore(t)
	ctx := t.Context()
	normal, err := pg.CreateRun(ctx, "n", nil, []byte(`{"steps":[{"name":"s","run":"true"}]}`), nil, nil, "")
	require.NoError(t, err)
	det, err := pg.CreateRun(ctx, "d", nil, []byte(`{"detached":true,"steps":[{"name":"s","call":{"job":"c"}}]}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	// A normal claim never returns the detached run.
	c1, err := pg.ClaimNextRun(ctx, "a1", nil, false)
	require.NoError(t, err)
	require.NotNil(t, c1)
	assert.Equal(t, normal.ID, c1.ID)

	// A detached claim returns only the detached run.
	c2, err := pg.ClaimNextRun(ctx, "a2", nil, true)
	require.NoError(t, err)
	require.NotNil(t, c2)
	assert.Equal(t, det.ID, c2.ID)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestPostgres_ClaimNextRun_DetachedFilter -count=1`
Expected: FAIL to compile (signature has 3 params).

- [ ] **Step 3: Implement**

In `internal/store/store.go`, update the interface method:

```go
	ClaimNextRun(ctx context.Context, agentID string, agentLabels []string, detached bool) (*ClaimedRun, error)
```

In `internal/store/postgres.go`, add the `detached bool` parameter and a WHERE clause + query arg:

```go
func (p *Postgres) ClaimNextRun(ctx context.Context, agentID string, agentLabels []string, detached bool) (*ClaimedRun, error) {
	if agentLabels == nil {
		agentLabels = []string{}
	}
	const q = `
		WITH picked AS (
		    SELECT r.id FROM runs r
		    LEFT JOIN agents a ON a.id = $1
		    WHERE r.status = 'Queued'
		      AND r.detached = $3
		      AND (r.agent_selector = '{}' OR r.agent_selector <@ $2::TEXT[])
		      AND (a.capabilities IS NULL OR r.required_caps <@ a.capabilities)
		    ORDER BY r.created_at
		    LIMIT 1
		    FOR UPDATE OF r SKIP LOCKED
		)
		UPDATE runs r SET claimed_by = $1, claimed_at = NOW(), updated_at = NOW(), status = 'Running'
		FROM picked WHERE r.id = picked.id
		RETURNING r.id, r.job_name, r.status, r.params, r.spec, r.created_at, r.updated_at;
	`
	var cr ClaimedRun
	var status string
	var paramsOut []byte
	err := p.pool.QueryRow(ctx, q, agentID, agentLabels, detached).
		Scan(&cr.ID, &cr.JobName, &status, &paramsOut, &cr.Spec, &cr.CreatedAt, &cr.UpdatedAt)
	// ... unchanged tail ...
```

- [ ] **Step 4: Fix all callers**

- `internal/controller/api_agent.go:162`: temporarily pass `false` (the kind param is wired in Task 5): `s.store.ClaimNextRun(r.Context(), agentID, agentLabels, false)`.
- Any store/mock in `internal/metrics/store.go` embedding pass-through is fine (it embeds `*Postgres`). Update any hand-written fakes implementing `store.Store` to the new signature.
- Update direct test callers in `internal/store/*_test.go` and `internal/controller/*_test.go` that call `ClaimNextRun(...)` to add the `false` argument (search: `ClaimNextRun(`).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/store/ -run TestPostgres_ClaimNextRun_DetachedFilter -count=1`
Expected: build clean, test PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_concurrency_test.go internal/controller/api_agent.go
git commit -m "feat(store): filter ClaimNextRun by detached"
```

---

### Task 5: Controller claim endpoint + agent client send the kind

**Files:**
- Modify: `internal/controller/api_agent.go` (claim handler, ~line 130-170)
- Modify: `internal/agent/client.go` (`Claim`, ~line 183)
- Test: `internal/controller/api_agent_test.go`

**Interfaces:**
- Consumes: `ClaimNextRun(..., detached bool)` (Task 4).
- Produces: `Client.Claim(ctx, agentID, timeout string, labels []string, detached bool)` — sends `&kind=detached` when `detached` is true; claim endpoint reads `kind` (default normal).

- [ ] **Step 1: Write the failing test**

```go
// in internal/controller/api_agent_test.go
func TestClaim_KindDetachedOnlyReturnsDetachedRuns(t *testing.T) {
	st := newTestStore(t)
	ctx := t.Context()
	_, err := st.CreateRun(ctx, "n", nil, []byte(`{"steps":[{"name":"s","run":"true"}]}`), nil, nil, "")
	require.NoError(t, err)
	_, err = st.CreateRun(ctx, "d", nil, []byte(`{"detached":true,"steps":[{"name":"s","call":{"job":"c"}}]}`), nil, nil, "")
	require.NoError(t, err)
	_, err = st.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)

	srv := newTestServer(t, st) // existing helper
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/claim?timeout=1ms&kind=detached", nil)
	// ... attach agent auth as sibling tests do ...
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp api.ClaimResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, "d", resp.JobName, "kind=detached must only hand out detached runs")
}
```

(Mirror the auth/setup of the existing claim tests in this file, e.g. `TestClaim*`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestClaim_KindDetachedOnlyReturnsDetachedRuns -count=1`
Expected: FAIL (kind ignored; normal run may be returned).

- [ ] **Step 3: Implement the handler**

In `internal/controller/api_agent.go` claim handler, read the kind and pass it:

```go
	detached := r.URL.Query().Get("kind") == "detached"
	// ... existing long-poll loop ...
	claimed, err := s.store.ClaimNextRun(r.Context(), agentID, agentLabels, detached)
```

- [ ] **Step 4: Implement the client**

In `internal/agent/client.go` `Claim`, add a `detached bool` parameter and append `&kind=detached` to the query when true:

```go
func (c *Client) Claim(ctx context.Context, agentID, timeout string, labels []string, detached bool) (api.ClaimResponse, error) {
	q := url.Values{}
	q.Set("timeout", timeout)
	for _, l := range labels {
		q.Add("labels", l)
	}
	if detached {
		q.Set("kind", "detached")
	}
	// ... build path with q.Encode(), rest unchanged ...
}
```

Update existing `Claim(...)` callers in `internal/agent/agent.go` to pass `false` for now (Task 7 introduces the detached caller).

- [ ] **Step 5: Run tests to verify they pass**

Run: `go build ./... && go test ./internal/controller/ -run TestClaim_KindDetachedOnlyReturnsDetachedRuns -count=1`
Expected: build clean, PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/api_agent.go internal/agent/client.go internal/agent/agent.go internal/controller/api_agent_test.go
git commit -m "feat(controller,agent): claim kind=detached filter"
```

---

### Task 6: Agent config `MaxDetachedConcurrent`

**Files:**
- Modify: `internal/config/agent.go` (`AgentConfig`, ~line 37; merge logic ~line 172)
- Modify: `cmd/unified-cd-agent/main.go` (flag/env wiring)
- Test: `internal/config/agent_test.go`

**Interfaces:**
- Produces: `AgentConfig.MaxDetachedConcurrent int` (yaml `maxDetachedConcurrent`). Semantics: `0` = off (no detached claiming), `-1` = unlimited, positive = cap. Default `16` applied by the agent binary when unset (Task 7 reads it).

- [ ] **Step 1: Write the failing test**

```go
// in internal/config/agent_test.go
func TestLoadAgent_MaxDetachedConcurrent(t *testing.T) {
	cfg := AgentConfig{MaxDetachedConcurrent: 8}
	merged := mergeAgent(AgentConfig{}, cfg) // use the same merge entry point the other tests use
	assert.Equal(t, 8, merged.MaxDetachedConcurrent)
}
```

(Match the exact merge/load helper the neighboring tests exercise.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoadAgent_MaxDetachedConcurrent -count=1`
Expected: FAIL (unknown field).

- [ ] **Step 3: Implement**

In `internal/config/agent.go`, add to `AgentConfig` next to `MaxConcurrent`:

```go
	MaxDetachedConcurrent int `yaml:"maxDetachedConcurrent"`
```

In the merge function next to the `MaxConcurrent` merge (~line 172), add:

```go
	if file.MaxDetachedConcurrent != 0 {
		eff.MaxDetachedConcurrent = file.MaxDetachedConcurrent
	}
```

In `cmd/unified-cd-agent/main.go`, mirror the `MaxConcurrent` flag/env wiring: add a `--max-detached-concurrent` flag and `UNIFIED_AGENT_MAX_DETACHED` env override, defaulting to `16` when unset (0 from config means "not set" → default 16; to disable, set `-1`... — NOTE: because `0` is both the zero value and "off", represent "unset" as the flag default `16` and let an explicit `0` disable. Wire the flag default to 16 and only override from env/config when provided.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestLoadAgent_MaxDetachedConcurrent -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/agent.go cmd/unified-cd-agent/main.go internal/config/agent_test.go
git commit -m "feat(config): agent MaxDetachedConcurrent"
```

---

### Task 7: Host agent — detached claim pool + per-run workspace

**Files:**
- Modify: `internal/agent/agent.go` (`Run`, slot spawn ~line 303-309; `runLoop`)
- Test: `internal/agent/agent_test.go` (or a focused `agent_detached_test.go`)

**Interfaces:**
- Consumes: `Client.Claim(..., detached bool)` (Task 5), `AgentConfig.MaxDetachedConcurrent` (Task 6).
- Produces: the host agent claims detached runs in a separate goroutine pool; each detached run executes with workspace `detached/<runID>` under the workspace base.

- [ ] **Step 1: Write the failing test**

Drive the fake-client harness (mirror `agent_callrun_test.go`) with `MaxConcurrent: 1` where the single normal slot is occupied by a long run, and assert a `detached` run is still claimed concurrently. Concretely, assert the agent issues a claim with `kind=detached` while the normal slot is busy:

```go
// in internal/agent/agent_detached_test.go
func TestAgent_ClaimsDetachedOutsideMaxConcurrent(t *testing.T) {
	var sawDetachedClaim atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/register", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("POST /api/v1/agents/a1/claim", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("kind") == "detached" {
			sawDetachedClaim.Store(true)
		}
		w.WriteHeader(http.StatusNoContent) // 204 = nothing to claim; keeps the loop idle
	})
	// heartbeat/other endpoints: 204
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	a := &Agent{ID: "a1", Client: NewClient(srv.URL, "tok"), MaxConcurrent: 1, MaxDetachedConcurrent: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = a.Run(ctx) // returns when ctx expires
	assert.True(t, sawDetachedClaim.Load(), "agent must poll for detached claims in a separate pool")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestAgent_ClaimsDetachedOutsideMaxConcurrent -count=1`
Expected: FAIL (`MaxDetachedConcurrent` field missing / no detached claim ever issued).

- [ ] **Step 3: Implement**

Add `MaxDetachedConcurrent int` to the `Agent` struct (mirroring `MaxConcurrent`, ~line 69). In `Run`, after spawning the `n` normal slot goroutines (~line 303-309), spawn the detached pool:

```go
	d := a.MaxDetachedConcurrent
	if d < 0 {
		d = 256 // "unlimited" — a high but finite backstop to bound goroutines
	}
	for i := 0; i < d; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			a.detachedRunLoop(ctx, runCtx, slot, wsBase, activeRuns, activeWorkDirs)
		}(i)
	}
```

Add `detachedRunLoop`, a copy of `runLoop` that (a) calls `a.Client.Claim(ctx, a.ID, timeout, labels, true)` (detached=true), and (b) computes the workspace as a per-run directory instead of the slot-keyed `working<slot>/<job>`:

```go
// detachedWorkDir returns a per-run workspace path for a detached run, decoupled
// from the slot-keyed working<slot>/<job> pool so detached claims never contend
// on a normal slot's workspace.
func detachedWorkDir(wsBase, runID string) string {
	return filepath.Join(wsBase, "detached", runID)
}
```

Wire `detachedRunLoop` to build the claim's workspace via `detachedWorkDir(wsBase, claim.RunID)`, enroll it in `activeWorkDirs` for GC (same as `runLoop`), and remove it after the run finishes. Keep everything else (execute, report, heartbeat enrollment via `activeRuns`) identical to `runLoop`.

Update the existing normal `Claim` call in `runLoop` to pass `false`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run TestAgent_ClaimsDetachedOutsideMaxConcurrent -count=1`
Expected: PASS

- [ ] **Step 5: Run the full agent package**

Run: `go test ./internal/agent/ -count=1`
Expected: PASS (no regression in existing slot/drain/heartbeat tests).

- [ ] **Step 6: Commit**

```bash
git add internal/agent/agent.go internal/agent/agent_detached_test.go
git commit -m "feat(agent): detached claim pool with per-run workspace"
```

---

### Task 8: k8s agent — detached claim exemption

**Files:**
- Modify: `internal/k8sagent/config.go` (`MaxDetachedConcurrent` + env, mirroring `PodStartTimeout`)
- Modify: `internal/k8sagent/agent.go` (claim loop ~line 141-190)
- Test: `internal/k8sagent/agent_test.go` (fake-backend claim loop tests already exist)

**Interfaces:**
- Consumes: `Client.Claim(..., detached bool)` (Task 5).
- Produces: the k8s agent claims detached runs against a separate `MaxDetachedConcurrent` semaphore, not the `MaxConcurrent` one.

- [ ] **Step 1: Write the failing test**

Mirror the existing k8s claim-loop test that exercises concurrency without a real pod backend (see the comment at `agent.go:53`). Assert that with `MaxConcurrent: 1` saturated, a detached run is still dispatched, and that detached claims use `kind=detached`.

```go
// in internal/k8sagent/agent_test.go (follow the existing fake-claim harness)
func TestK8sAgent_DetachedClaimedOutsideSemaphore(t *testing.T) {
	// Arrange a fake claim source returning one detached run while the single
	// normal semaphore token is held; assert the detached run is dispatched.
	// (Use the same fakePM / injected-claim seam the other agent_test.go cases use.)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestK8sAgent_DetachedClaimedOutsideSemaphore -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `internal/k8sagent/config.go`, add `MaxDetachedConcurrent int` (yaml `maxDetachedConcurrent`) with an `UNIFIED_K8S_MAX_DETACHED` env override in `Validate()`, mirroring `PodStartTimeout` handling.

In `internal/k8sagent/agent.go`, run a second claim loop for detached runs gated by a separate semaphore:

```go
	var detSem chan struct{}
	if a.cfg.MaxDetachedConcurrent > 0 {
		detSem = make(chan struct{}, a.cfg.MaxDetachedConcurrent)
	}
```

Factor the existing claim loop body so it can run twice: once with `(sem, detached=false)` and once with `(detSem, detached=true)`, each calling `a.claim(ctx, detached)` → `Client.Claim(ctx, a.ID, timeout, labels, detached)`, acquiring/releasing its own semaphore around dispatch (the `defer func(){ <-sem }()` pattern at agent.go:183-184). Run the detached loop in its own goroutine so both poll concurrently. Everything downstream (pod creation, execution) is unchanged (Approach A).

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/k8sagent/ -run TestK8sAgent_DetachedClaimedOutsideSemaphore -count=1`
Expected: PASS

- [ ] **Step 5: Run the full k8s package**

Run: `go test ./internal/k8sagent/ -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/config.go internal/k8sagent/agent.go internal/k8sagent/agent_test.go
git commit -m "feat(k8sagent): claim detached runs outside MaxConcurrent semaphore"
```

---

### Task 9: Documentation

**Files:**
- Modify: `docs/jobs.md` (add "Detached runs"; update the `call:` slot-deadlock warning ~line 665)
- Modify: `docs/configuration.md` (agent `maxDetachedConcurrent` knob)

- [ ] **Step 1: Add the "Detached runs" section to `docs/jobs.md`**

Under the `call:`/Concurrency area, add:

```markdown
### Detached runs

A job with `spec.detached: true` is a **lightweight orchestrator**: its runs do
not consume an agent's `maxConcurrent` budget and are claimed from a separate
`maxDetachedConcurrent` pool. Use it for jobs that mostly issue `call:` steps and
wait, so the orchestrator does not hold a scarce execution slot while its child
runs — this is the recommended fix for the call-slot deadlock below.

- `detached` runs get an independent per-run workspace on host agents (on
  Kubernetes each run already has its own pod/volume).
- `detached: true` may not be combined with `native: true` (rejected at apply).
- Do **not** combine `detached: true` with `podTemplate.reuse`: a detached run
  holds its pod idle while it waits, so pooling gives no benefit.
- Set `maxDetachedConcurrent` on the agents that should host orchestrators
  (default 16; `0` disables detached claiming there).
```

Update the existing call-slot-deadlock warning to point at `detached: true` as the primary fix (alongside `max-concurrent >= 2`).

- [ ] **Step 2: Add the agent knob to `docs/configuration.md`**

Document `maxDetachedConcurrent` / `--max-detached-concurrent` / `UNIFIED_AGENT_MAX_DETACHED` next to `maxConcurrent`: default 16, `0` = off, `-1` = unlimited.

- [ ] **Step 3: Commit**

```bash
git add docs/jobs.md docs/configuration.md
git commit -m "docs: document detached runs and maxDetachedConcurrent"
```

---

## Final verification

- [ ] Run `go build ./...` — clean.
- [ ] Run `go vet ./...` — clean.
- [ ] Run `go test ./...` — all green (integration/e2e need Docker + Postgres; run where available).
- [ ] Manually confirm the deadlock scenario: a `detached: true` parent job that `call:`s a child, on a single-slot agent, now lets the child be claimed while the parent waits.
