# Manual Approval Step (`approval`) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an `approval` step that pauses a run until an authenticated user approves or rejects it (or it times out); approval continues the run, reject/timeout fails the step so the `finally` block runs.

**Architecture:** A new mutually-exclusive `approval` step action. The agent executing the run blocks on the step, creating a pending approval row and polling for the decision via agent-authed (`/agents/*`) endpoints. Humans decide via a `ServerAuth` `/runs/*` endpoint that records the principal as `decided_by`. A shared `WaitForApproval` helper is used by both the standard agent and the k8s agent.

**Tech Stack:** Go 1.26, chi router, pgx/golang-migrate, cobra CLI, Svelte 5 UI, testify.

## Global Constraints

- Go module `github.com/eirueimi/unified-cd`, Go 1.26.2.
- Spec of record: `docs/superpowers/specs/2026-06-30-approval-step-design.md`.
- Any authenticated principal may approve/reject; the principal identity (PAT name or OIDC email/sub) is recorded as `decided_by`.
- Wait is agent-blocking poll. Reject/timeout → step `Failed` → run `Failed` (finally runs). No new run-status enum; outcomes distinguished in `run_approvals`.
- `timeoutMinutes` optional, **default 60**; on timeout the agent records a `TimedOut` decision (`decided_by='system'`) and fails the step.
- New step status string: `WaitingApproval` (run stays `Running`).
- `approval` is mutually exclusive with `run`/`call`/`uses`/`cache`/`uploadArtifact`/`downloadArtifact`, and is **rejected in `finally`** (alongside `cache`/`post`).
- **Spec refinement (justified):** the agent creates AND polls the approval via agent-authed endpoints `POST/GET /api/v1/agents/{agentId}/runs/{runId}/approvals[/{stepIndex}]` (BearerAuth — the path the agent is guaranteed authed on). This replaces the spec's "controller creates the row on a WaitingApproval report by re-parsing the stored spec." Humans decide via `POST /api/v1/runs/{runID}/approvals/{stepIndex}` (ServerAuth); UI/CLI read via `GET /api/v1/runs/{runID}/approvals` (ServerAuth).
- Conditional `UPDATE ... WHERE status='Pending'` for decisions (first writer wins → 409 otherwise).

---

## File map

| File | Change |
|---|---|
| `internal/controller/auth.go` | store `Principal` in request context for all 3 auth paths; `principalFromContext` |
| `internal/dsl/types.go`, `parse.go` | `ApprovalStep` action + validation; reject in finally |
| `internal/store/migrations/016_run_approvals.up/down.sql` | `run_approvals` table |
| `internal/store/store.go`, `postgres.go` | approval store methods |
| `internal/api/types.go` | `ClaimStep.Approval`, `ClaimApproval`, `RunApproval`, `ApprovalDecisionRequest`, `CreateApprovalRequest` |
| `internal/controller/api_agent.go` | compile approval into ClaimStep (default timeout) |
| `internal/controller/api_approvals.go` (new) | decision + list + agent create/poll handlers |
| `internal/controller/server.go` | route registration |
| `internal/agent/approval.go` (new) | `WaitForApproval` helper |
| `internal/agent/client.go` | agent client methods (create/poll approval) |
| `internal/agent/agent.go` | dispatch `step.Approval` in `makeStepRunner` |
| `internal/k8sagent/agent.go` | dispatch `step.Approval` in `makeRunStep` |
| `internal/cli/approvals.go` (new), `root.go` | `approve`/`reject` commands |
| `web/src/routes/RunDetail.svelte` | Approve/Reject buttons on `WaitingApproval` steps |
| `docs/jobs.md`, schema | document `approval`; regenerate |

---

## Task 1: Auth principal in request context (+ record run trigger identity)

**Files:** Modify `internal/controller/auth.go`, `internal/controller/api_runs.go`; Test `internal/controller/auth_principal_test.go` (new), `internal/controller/api_runs_test.go`.

**Interfaces:**
- Produces: `type Principal struct { Name string; Kind string }`; `func principalFromContext(ctx context.Context) (Principal, bool)`; `ServerAuth` now stores the `Principal` in the request context on each success path. `handleTriggerRun` records the authenticated principal as the run's `TriggeredBy` (falling back to `"api"` when no principal is present).

- [ ] **Step 1: Write the failing test**

Create `internal/controller/auth_principal_test.go`:

```go
package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerAuth_StoresPATPrincipal(t *testing.T) {
	st := newTestStore(t) // reuse the controller package's existing test store helper; if none, see note
	// create a PAT named "ci-bot"
	_, plain := mustCreatePAT(t, st, "ci-bot")

	var got Principal
	var ok bool
	h := ServerAuth(st, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = principalFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/v1/runs/x", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.Equal(t, http.StatusOK, rr.Code)
	require.True(t, ok, "principal must be in context")
	assert.Equal(t, "ci-bot", got.Name)
	assert.Equal(t, "pat", got.Kind)
}
```

NOTE: check how existing controller tests build a `store.Store` and a PAT (look in `internal/controller/*_test.go` and `internal/store` for `NewTestPostgres`). If the controller package has no store-test helper, use `store.NewTestPostgres(t)` and the store's PAT-create method (find its name in `internal/store/store.go`, e.g. `CreatePAT`), and `HashToken`. Replace `newTestStore`/`mustCreatePAT` with the real calls. The test needs Postgres (same as other store-backed tests) — it will skip if `NewTestPostgres` skips without a DB.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestServerAuth_StoresPATPrincipal -v`
Expected: FAIL — `Principal`/`principalFromContext` undefined.

- [ ] **Step 3: Implement principal-in-context**

In `internal/controller/auth.go`, add near the top:

```go
type ctxKey string

const principalCtxKey ctxKey = "principal"

// Principal identifies the authenticated caller.
type Principal struct {
	Name string // PAT name, or OIDC email (fallback to sub)
	Kind string // "pat" | "oidc" | "session"
}

func withPrincipal(r *http.Request, p Principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalCtxKey, p))
}

// principalFromContext returns the authenticated principal, if any.
func principalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey).(Principal)
	return p, ok
}
```

In `ServerAuth`, on EACH success path, attach the principal before calling `next` and pass the new request:
- PAT path (after `pat != nil`): `next.ServeHTTP(w, withPrincipal(r, Principal{Name: pat.Name, Kind: "pat"}))`.
- OIDC bearer path: extract the verified claims' email (fallback sub) and `withPrincipal(r, Principal{Name: email, Kind: "oidc"})`.
- Session cookie path: use the session's `email` (fallback `sub`) → `withPrincipal(r, Principal{Name: email, Kind: "session"})`.

Read the current three success branches in `auth.go` and thread `withPrincipal` through each (replace the bare `next.ServeHTTP(w, r)` with the principal-carrying request). Confirm `context` and `net/http` are imported.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestServerAuth_StoresPATPrincipal -v`
Expected: PASS (or SKIP if no DB — then verify compilation with `go build ./...`).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/auth.go internal/controller/auth_principal_test.go
git commit -m "feat(controller): expose authenticated principal via request context"
```

- [ ] **Step 6: Write the failing test for run-trigger identity**

Append to `internal/controller/api_runs_test.go` (or create it; mirror the package's existing server-test harness used by other `api_*_test.go`). The test triggers a run via `POST /api/v1/runs` with a PAT named `"alice"` and asserts the created run's `TriggeredBy == "alice"`:

```go
func TestTriggerRun_RecordsPrincipal(t *testing.T) {
	// Build the server + store harness (mirror existing api_*_test.go).
	// Create a PAT "alice"; UpsertJob "j".
	// POST /api/v1/runs {"jobName":"j"} with Authorization: Bearer <alice token>.
	// Assert response 200 and the returned run.TriggeredBy == "alice".
}
```

Write it concretely against the real harness (PAT create + bearer header), asserting `run.TriggeredBy == "alice"`.

- [ ] **Step 7: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestTriggerRun_RecordsPrincipal -v`
Expected: FAIL — `TriggeredBy` is the literal `"api"`, not `"alice"`.

- [ ] **Step 8: Populate `TriggeredBy` from the principal**

In `internal/controller/api_runs.go`, `handleTriggerRun`, replace the hardcoded `"api"` in the `CreateRun(...)` call with the principal's name, falling back to `"api"`:

```go
	triggeredBy := "api"
	if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
		triggeredBy = p.Name
	}
	run, err := s.store.CreateRun(r.Context(), job.Name, req.Params, job.Spec, agentSelector, triggeredBy)
```

(Leave the webhook/schedule trigger paths unchanged — they have no human principal and keep their source label.)

- [ ] **Step 9: Run test + commit**

Run: `go test ./internal/controller/ -run TestTriggerRun_RecordsPrincipal -v && go build ./...`
Expected: PASS (or SKIP without DB; build succeeds).

```bash
git add internal/controller/api_runs.go internal/controller/api_runs_test.go
git commit -m "feat(controller): record triggering principal as run TriggeredBy"
```

---

## Task 2: DSL `approval` step + validation

**Files:** Modify `internal/dsl/types.go`, `parse.go`; Test `internal/dsl/parse_test.go`.

**Interfaces:**
- Produces: `type ApprovalStep struct { Message string; TimeoutMinutes float64 }`; `Approval *ApprovalStep` on `StepEntry` and `Step`; validation treats `approval` as one action, mutually exclusive, rejected in finally.

- [ ] **Step 1: Write the failing tests**

Append to `internal/dsl/parse_test.go`:

```go
func TestParse_ApprovalStep(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: gated
spec:
  steps:
    - name: build
      run: make build
    - name: gate
      approval:
        message: "Deploy to prod?"
        timeoutMinutes: 30`
	job, err := Parse(strings.NewReader(y))
	require.NoError(t, err)
	require.NotNil(t, job.Spec.Steps[1].Approval)
	assert.Equal(t, "Deploy to prod?", job.Spec.Steps[1].Approval.Message)
	assert.Equal(t, 30.0, job.Spec.Steps[1].Approval.TimeoutMinutes)
}

func TestParse_ApprovalMutuallyExclusiveWithRun(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: bad
spec:
  steps:
    - name: gate
      run: echo hi
      approval:
        message: x`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one of")
}

func TestParse_ApprovalRejectedInFinally(t *testing.T) {
	y := `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: bad
spec:
  steps:
    - name: build
      run: make build
  finally:
    - name: gate
      approval:
        message: x`
	_, err := Parse(strings.NewReader(y))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "finally")
	assert.Contains(t, err.Error(), "approval")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/dsl/ -run TestParse_Approval -v`
Expected: FAIL — `Approval` field undefined.

- [ ] **Step 3: Add the type and field**

In `internal/dsl/types.go`, add the struct (near `PostStep`):

```go
// ApprovalStep pauses the run until an authenticated user approves or rejects.
// TimeoutMinutes defaults to 60 (applied at compile time) when zero.
type ApprovalStep struct {
	Message        string  `yaml:"message,omitempty"`
	TimeoutMinutes float64 `yaml:"timeoutMinutes,omitempty"`
}
```

Add `Approval *ApprovalStep` to BOTH `StepEntry` (after `DownloadArtifact`) and `Step`:

```go
	Approval         *ApprovalStep         `yaml:"approval,omitempty"`
```

- [ ] **Step 4: Validation**

In `internal/dsl/parse.go`, `validateStepFull` counts actions. Add `approval` to the action count and the mutual-exclusion message. Find the `actionCount` block and add:

```go
	if approval != nil {
		actionCount++
	}
```

Change `validateStepFull`'s signature to accept the approval pointer, OR (simpler) validate approval separately. Cleanest: add an `approval *ApprovalStep` parameter to `validateStepFull` and pass `entry.Approval`/`st.Approval` from `validateStepEntries`. Update the action-count and the "only one of run, call, cache, uses may be specified" error to read "only one of run, call, cache, uses, approval may be specified". Update `actionCount == 0` message to "one of run, call, uses, or approval is required".

In `validateStepEntries`, when `allowDeferredHooks` is false (i.e. `finally`), reject approval like cache/post:

```go
		if !allowDeferredHooks && entry.Approval != nil {
			return fmt.Errorf("%s[%d]: approval: is not supported in finally steps", pathPrefix, i)
		}
```

(and the same for parallel sub-entries `st.Approval`). Apply the default timeout at compile time, not here.

Update the `validateStepFull` calls in `validateStepEntries` (both the parallel and serial branches) to pass the approval pointer.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -run 'TestParse' -v && go build ./...`
Expected: PASS; existing dsl tests still pass.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/types.go internal/dsl/parse.go internal/dsl/parse_test.go
git commit -m "feat(dsl): approval step action with validation"
```

---

## Task 3: Store — `run_approvals` table + methods

**Files:** Create `internal/store/migrations/016_run_approvals.up.sql` + `.down.sql`; Modify `internal/store/store.go`, `postgres.go`; Test `internal/store/postgres_approvals_test.go`.

**Interfaces:**
- Produces (on `store.Store`):
  - `CreatePendingApproval(ctx, runID string, stepIndex int, stepName, message string, timeoutAt *time.Time) error` — idempotent: leaves an existing row untouched.
  - `DecideApproval(ctx, runID string, stepIndex int, status, decidedBy, comment string) (bool, error)` — conditional update WHERE status='Pending'; returns whether a row changed.
  - `GetApproval(ctx, runID string, stepIndex int) (api.RunApproval, error)`.
  - `ListRunApprovals(ctx, runID string) ([]api.RunApproval, error)`.
- Consumes: `api.RunApproval` (defined in Task 4 — but the store signatures reference it; define `api.RunApproval` in THIS task's first step so the store compiles, then Task 4 adds the wire/claim types).

- [ ] **Step 1: Define `api.RunApproval` and write the failing test**

In `internal/api/types.go` add:

```go
type RunApproval struct {
	RunID     string     `json:"runId"`
	StepIndex int        `json:"stepIndex"`
	StepName  string     `json:"stepName"`
	Message   string     `json:"message"`
	Status    string     `json:"status"` // Pending | Approved | Rejected | TimedOut
	DecidedBy string     `json:"decidedBy,omitempty"`
	Comment   string     `json:"comment,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	TimeoutAt *time.Time `json:"timeoutAt,omitempty"`
	DecidedAt *time.Time `json:"decidedAt,omitempty"`
}
```

Create `internal/store/postgres_approvals_test.go`:

```go
package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_Approvals_CreateDecide(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", nil))

	// idempotent: second create does not error or overwrite
	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", nil))

	got, err := pg.GetApproval(ctx, run.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, "Pending", got.Status)

	changed, err := pg.DecideApproval(ctx, run.ID, 1, "Approved", "alice", "lgtm")
	require.NoError(t, err)
	assert.True(t, changed)

	// second decision: no change (already decided)
	changed, err = pg.DecideApproval(ctx, run.ID, 1, "Rejected", "bob", "")
	require.NoError(t, err)
	assert.False(t, changed)

	got, _ = pg.GetApproval(ctx, run.ID, 1)
	assert.Equal(t, "Approved", got.Status)
	assert.Equal(t, "alice", got.DecidedBy)

	list, err := pg.ListRunApprovals(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestPostgres_Approvals -v`
Expected: FAIL — methods undefined (or SKIP if no DB; if it skips, you still must implement and rely on CI's DB — but write the code so it compiles).

- [ ] **Step 3: Migration**

Create `internal/store/migrations/016_run_approvals.up.sql`:

```sql
CREATE TABLE run_approvals (
    run_id      UUID NOT NULL,
    step_index  INT  NOT NULL,
    step_name   TEXT NOT NULL,
    message     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,
    decided_by  TEXT,
    comment     TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    timeout_at  TIMESTAMPTZ,
    decided_at  TIMESTAMPTZ,
    PRIMARY KEY (run_id, step_index)
);
```

Create `internal/store/migrations/016_run_approvals.down.sql`:

```sql
DROP TABLE IF EXISTS run_approvals;
```

(Confirm the `runs.id` type is UUID — the explore report showed `run_id UUID` usage elsewhere; match it. If runs.id is TEXT, use TEXT here.)

- [ ] **Step 4: Store interface + Postgres impl**

Add the four methods to the `Store` interface in `internal/store/store.go` (match the existing signature style). Implement in `internal/store/postgres.go`:

```go
func (p *Postgres) CreatePendingApproval(ctx context.Context, runID string, stepIndex int, stepName, message string, timeoutAt *time.Time) error {
	const q = `
		INSERT INTO run_approvals(run_id, step_index, step_name, message, status, timeout_at)
		VALUES ($1, $2, $3, $4, 'Pending', $5)
		ON CONFLICT (run_id, step_index) DO NOTHING;
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, stepName, message, timeoutAt)
	return err
}

func (p *Postgres) DecideApproval(ctx context.Context, runID string, stepIndex int, status, decidedBy, comment string) (bool, error) {
	const q = `
		UPDATE run_approvals
		SET status = $3, decided_by = $4, comment = $5, decided_at = now()
		WHERE run_id = $1 AND step_index = $2 AND status = 'Pending';
	`
	tag, err := p.pool.Exec(ctx, q, runID, stepIndex, status, decidedBy, comment)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (p *Postgres) GetApproval(ctx context.Context, runID string, stepIndex int) (api.RunApproval, error) {
	const q = `
		SELECT run_id, step_index, step_name, message, status, decided_by, comment, created_at, timeout_at, decided_at
		FROM run_approvals WHERE run_id = $1 AND step_index = $2;
	`
	var a api.RunApproval
	var decidedBy, comment *string
	err := p.pool.QueryRow(ctx, q, runID, stepIndex).Scan(
		&a.RunID, &a.StepIndex, &a.StepName, &a.Message, &a.Status, &decidedBy, &comment, &a.CreatedAt, &a.TimeoutAt, &a.DecidedAt)
	if err != nil {
		return api.RunApproval{}, err
	}
	if decidedBy != nil { a.DecidedBy = *decidedBy }
	if comment != nil { a.Comment = *comment }
	return a, nil
}

func (p *Postgres) ListRunApprovals(ctx context.Context, runID string) ([]api.RunApproval, error) {
	const q = `
		SELECT run_id, step_index, step_name, message, status, decided_by, comment, created_at, timeout_at, decided_at
		FROM run_approvals WHERE run_id = $1 ORDER BY step_index;
	`
	rows, err := p.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []api.RunApproval
	for rows.Next() {
		var a api.RunApproval
		var decidedBy, comment *string
		if err := rows.Scan(&a.RunID, &a.StepIndex, &a.StepName, &a.Message, &a.Status, &decidedBy, &comment, &a.CreatedAt, &a.TimeoutAt, &a.DecidedAt); err != nil {
			return nil, err
		}
		if decidedBy != nil { a.DecidedBy = *decidedBy }
		if comment != nil { a.Comment = *comment }
		out = append(out, a)
	}
	return out, rows.Err()
}
```

If `internal/store` has any in-memory/mock Store implementation besides Postgres (check for a second implementor of the `Store` interface), add the four methods there too (no-op or map-backed) so the build stays green.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/store/ -run TestPostgres_Approvals -v && go build ./...`
Expected: PASS (or SKIP without DB; build must succeed).

- [ ] **Step 6: Commit**

```bash
git add internal/store/migrations/016_run_approvals.up.sql internal/store/migrations/016_run_approvals.down.sql internal/store/store.go internal/store/postgres.go internal/store/postgres_approvals_test.go internal/api/types.go
git commit -m "feat(store): run_approvals table and CRUD methods"
```

---

## Task 4: API types + controller compile

**Files:** Modify `internal/api/types.go`, `internal/controller/api_agent.go`; Test `internal/controller/api_agent_test.go`.

**Interfaces:**
- Produces: `ClaimStep.Approval *ClaimApproval`; `type ClaimApproval struct { Message string; TimeoutMinutes float64 }`; `type ApprovalDecisionRequest struct { Decision string; Comment string }`; `type CreateApprovalRequest struct { StepIndex int; StepName string; Message string; TimeoutMinutes float64 }`. `buildOneClaimStep` copies approval, defaulting `TimeoutMinutes` to 60 when 0.

- [ ] **Step 1: Write the failing test**

Append to `internal/controller/api_agent_test.go`:

```go
func TestBuildClaimResponse_ApprovalDefaultsTimeout(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{
		{Name: "gate", Approval: &dsl.ApprovalStep{Message: "ok?"}}, // no timeout
	}}
	raw, _ := json.Marshal(spec)
	resp, err := buildClaimResponse(&store.ClaimedRun{Run: api.Run{ID: "r1", JobName: "j"}, Spec: raw})
	require.NoError(t, err)
	require.Len(t, resp.Stages, 1)
	require.NotNil(t, resp.Stages[0].Step.Approval)
	assert.Equal(t, "ok?", resp.Stages[0].Step.Approval.Message)
	assert.Equal(t, 60.0, resp.Stages[0].Step.Approval.TimeoutMinutes, "default timeout applied")
}
```

(Match the `store.ClaimedRun` construction used by the existing `TestBuildClaimResponse_Finally` in the same file.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestBuildClaimResponse_Approval -v`
Expected: FAIL — `Approval` field undefined on ClaimStep.

- [ ] **Step 3: Add wire types + compile**

In `internal/api/types.go`:

```go
type ClaimApproval struct {
	Message        string  `json:"message,omitempty"`
	TimeoutMinutes float64 `json:"timeoutMinutes"`
}

type ApprovalDecisionRequest struct {
	Decision string `json:"decision"` // "approve" | "reject"
	Comment  string `json:"comment,omitempty"`
}

type CreateApprovalRequest struct {
	StepIndex      int     `json:"stepIndex"`
	StepName       string  `json:"stepName"`
	Message        string  `json:"message,omitempty"`
	TimeoutMinutes float64 `json:"timeoutMinutes"`
}
```

Add `Approval *ClaimApproval` to `ClaimStep`:

```go
	Approval *ClaimApproval `json:"approval,omitempty"`
```

In `internal/controller/api_agent.go`, `buildOneClaimStep`, after the other action copies:

```go
	if entry.Approval != nil {
		timeout := entry.Approval.TimeoutMinutes
		if timeout == 0 {
			timeout = 60
		}
		cs.Approval = &api.ClaimApproval{Message: entry.Approval.Message, TimeoutMinutes: timeout}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestBuildClaimResponse -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/types.go internal/controller/api_agent.go internal/controller/api_agent_test.go
git commit -m "feat(api): approval wire types; compile approval into ClaimStep with default timeout"
```

---

## Task 5: Controller endpoints (decision + list + agent create/poll)

**Files:** Create `internal/controller/api_approvals.go`; Modify `internal/controller/server.go`; Test `internal/controller/api_approvals_test.go`.

**Interfaces:**
- Consumes: `principalFromContext` (Task 1), store approval methods (Task 3), `api.ApprovalDecisionRequest`/`CreateApprovalRequest`/`RunApproval` (Task 4/3).
- Produces handlers:
  - `handleDecideApproval` — `POST /api/v1/runs/{runID}/approvals/{stepIndex}` (ServerAuth). Body `ApprovalDecisionRequest`. `decided_by` from principal. 404 if no pending row, 409 if already decided.
  - `handleListRunApprovals` — `GET /api/v1/runs/{runID}/approvals` (ServerAuth).
  - `handleAgentCreateApproval` — `POST /api/v1/agents/{agentId}/runs/{runId}/approvals` (BearerAuth) → `CreatePendingApproval`.
  - `handleAgentGetApproval` — `GET /api/v1/agents/{agentId}/runs/{runId}/approvals/{stepIndex}` (BearerAuth) → `GetApproval`.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/api_approvals_test.go` using the package's existing server-test harness (find how other `api_*_test.go` build a `*Server` + `httptest` server with auth — mirror it). Test the decision flow:

```go
func TestApprovals_DecideFlow(t *testing.T) {
	// Build server + store, create a run, create a pending approval (via store directly).
	// 1. POST decision with a valid PAT -> 200/204; GetApproval shows Approved + decided_by.
	// 2. Second POST -> 409 (already decided).
	// 3. POST to a (run,step) with no pending row -> 404.
}
```

Write it concretely against the real harness: create a run + `st.CreatePendingApproval(ctx, runID, 0, "gate", "ok?", nil)`, then `POST /api/v1/runs/{runID}/approvals/0` with `{"decision":"approve","comment":"lgtm"}` and a valid PAT bearer; assert status and that `st.GetApproval` returns `Approved`/decided_by = the PAT name. Then assert 409 on repeat, 404 on a fresh step index.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestApprovals -v`
Expected: FAIL — handlers/routes undefined.

- [ ] **Step 3: Implement handlers**

Create `internal/controller/api_approvals.go`:

```go
package controller

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/go-chi/chi/v5"
)

func (s *Server) handleDecideApproval(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	stepIndex, err := strconv.Atoi(chi.URLParam(r, "stepIndex"))
	if err != nil {
		http.Error(w, "bad step index", http.StatusBadRequest)
		return
	}
	var req api.ApprovalDecisionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	status := ""
	switch req.Decision {
	case "approve":
		status = "Approved"
	case "reject":
		status = "Rejected"
	default:
		http.Error(w, `decision must be "approve" or "reject"`, http.StatusBadRequest)
		return
	}
	decidedBy := "unknown"
	if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
		decidedBy = p.Name
	}
	changed, err := s.store.DecideApproval(r.Context(), runID, stepIndex, status, decidedBy, req.Comment)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !changed {
		// either no pending row (404) or already decided (409): disambiguate
		if _, err := s.store.GetApproval(r.Context(), runID, stepIndex); err != nil {
			http.Error(w, "no pending approval", http.StatusNotFound)
			return
		}
		http.Error(w, "already decided", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListRunApprovals(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	list, err := s.store.ListRunApprovals(r.Context(), runID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleAgentCreateApproval(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	var req api.CreateApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	var timeoutAt *time.Time
	if req.TimeoutMinutes > 0 {
		t := time.Now().Add(time.Duration(req.TimeoutMinutes * float64(time.Minute)))
		timeoutAt = &t
	}
	if err := s.store.CreatePendingApproval(r.Context(), runID, req.StepIndex, req.StepName, req.Message, timeoutAt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentGetApproval(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")
	stepIndex, err := strconv.Atoi(chi.URLParam(r, "stepIndex"))
	if err != nil {
		http.Error(w, "bad step index", http.StatusBadRequest)
		return
	}
	a, err := s.store.GetApproval(r.Context(), runID, stepIndex)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
```

(Add `"time"` to imports. Use the package's existing `writeJSON` helper — confirm its signature matches `writeJSON(w, status, v)`.)

- [ ] **Step 4: Register routes**

In `internal/controller/server.go`, inside the `/api/v1` `ServerAuth` block (near the run routes ~line 205):

```go
		r.Get("/runs/{runID}/approvals", s.handleListRunApprovals)
		r.Post("/runs/{runID}/approvals/{stepIndex}", s.handleDecideApproval)
```

Inside the `/api/v1/agents` block, add two `BearerAuth(s.cfg.AgentToken)` routes:

```go
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/runs/{runId}/approvals", s.handleAgentCreateApproval)
		r.With(BearerAuth(s.cfg.AgentToken)).Get("/{agentId}/runs/{runId}/approvals/{stepIndex}", s.handleAgentGetApproval)
```

(NOTE: the `/runs/{id}` ServerAuth routes use `{id}`; the new approval routes use `{runID}` — chi allows different param names on different routes, but be consistent within a route. Use `{runID}` in the new run-scoped approval routes and read `chi.URLParam(r, "runID")`.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/controller/ -run TestApprovals -v && go build ./...`
Expected: PASS (or SKIP without DB; build succeeds).

- [ ] **Step 6: Commit**

```bash
git add internal/controller/api_approvals.go internal/controller/server.go internal/controller/api_approvals_test.go
git commit -m "feat(controller): approval decision/list endpoints + agent create/poll endpoints"
```

---

## Task 6: Agent `WaitForApproval` helper + wire into standard agent

**Files:** Create `internal/agent/approval.go`; Modify `internal/agent/client.go`, `internal/agent/agent.go`; Test `internal/agent/approval_test.go`.

**Interfaces:**
- Produces:
  - `Client.CreateApproval(ctx, agentID, runID string, req api.CreateApprovalRequest) error`
  - `Client.GetApproval(ctx, agentID, runID string, stepIndex int) (api.RunApproval, error)`
  - `func WaitForApproval(ctx context.Context, c *Client, agentID, runID string, step api.ClaimStep, poll time.Duration) (approved bool)` — reports `WaitingApproval`, creates the pending row, polls until decided or `step.Approval.TimeoutMinutes` elapses; on timeout records `TimedOut` via a decision (best-effort) and returns false.
- Consumes: `makeStepRunner` dispatch (agent.go).

- [ ] **Step 1: Write the failing test**

Create `internal/agent/approval_test.go` using the existing `runJobStages`/mock-controller harness pattern (see `agent_finally_test.go`). Add a mock handler for the agent approval create + get endpoints, and have the GET return `Approved` after the first poll. Assert: a job `[run "build", approval "gate", run "deploy"]` → gate reported `WaitingApproval` then the run reaches `deploy` and finishes `Succeeded`. Add a second test where the GET returns `Rejected` → gate fails, `deploy` skipped, run `Failed`.

(The harness's mock controller must serve `POST /api/v1/agents/{id}/runs/{runId}/approvals` (204) and `GET /api/v1/agents/{id}/runs/{runId}/approvals/{idx}` (returns a `api.RunApproval` with a controllable status). Use a small atomic/counter so the first GET returns Pending and the next returns Approved/Rejected to exercise polling.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestApproval -v`
Expected: FAIL — `WaitForApproval` / dispatch undefined.

- [ ] **Step 3: Client methods**

In `internal/agent/client.go`:

```go
func (c *Client) CreateApproval(ctx context.Context, agentID, runID string, req api.CreateApprovalRequest) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/runs/"+runID+"/approvals", req, nil)
	return err
}

func (c *Client) GetApproval(ctx context.Context, agentID, runID string, stepIndex int) (api.RunApproval, error) {
	var a api.RunApproval
	_, err := c.do(ctx, http.MethodGet, "/api/v1/agents/"+agentID+"/runs/"+runID+"/approvals/"+strconv.Itoa(stepIndex), nil, &a)
	return a, err
}
```

(Add `"strconv"` import if missing.)

- [ ] **Step 4: `WaitForApproval` helper**

Create `internal/agent/approval.go`:

```go
package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// WaitForApproval creates a pending approval, reports the step as WaitingApproval,
// and polls until the approval is decided or its timeout elapses. Returns true
// only on an explicit Approved decision. On timeout it records a TimedOut
// decision (best-effort) and returns false.
func WaitForApproval(ctx context.Context, c *Client, agentID, runID string, step api.ClaimStep, poll time.Duration) bool {
	timeoutMin := 60.0
	msg := ""
	if step.Approval != nil {
		if step.Approval.TimeoutMinutes > 0 {
			timeoutMin = step.Approval.TimeoutMinutes
		}
		msg = step.Approval.Message
	}
	_ = c.CreateApproval(ctx, agentID, runID, api.CreateApprovalRequest{
		StepIndex: step.Index, StepName: step.Name, Message: msg, TimeoutMinutes: timeoutMin,
	})
	_ = c.ReportStep(ctx, agentID, api.StepReportRequest{
		RunID: runID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "WaitingApproval", StartedAt: time.Now().UTC(),
	})

	deadline := time.Now().Add(time.Duration(timeoutMin * float64(time.Minute)))
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		a, err := c.GetApproval(ctx, agentID, runID, step.Index)
		if err == nil {
			switch a.Status {
			case "Approved":
				return true
			case "Rejected", "TimedOut":
				return false
			}
		} else {
			slog.Warn("approval poll failed", "runID", runID, "step", step.Name, "error", err)
		}
		if time.Now().After(deadline) {
			// best-effort: there is no agent decision endpoint; the controller-side
			// row stays Pending. Record timeout by failing the step (caller does so).
			slog.Warn("approval timed out", "runID", runID, "step", step.Name)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
```

NOTE on TimedOut recording: the agent has no decision endpoint (decisions are human-only via ServerAuth). For v1, on timeout the helper returns false and the caller fails the step; the `run_approvals` row remains `Pending` (acceptable — the run is Failed and `finally` runs; the audit row simply shows no decision). If you want the row to read `TimedOut`, add a tiny agent endpoint `POST /agents/{id}/runs/{runId}/approvals/{stepIndex}/timeout` that calls `DecideApproval(..., "TimedOut", "system", "")` and call it here — OPTIONAL, only if the reviewer/spec insists. Keep v1 minimal: leave the row Pending on timeout and document it.

- [ ] **Step 5: Dispatch in `makeStepRunner`**

In `internal/agent/agent.go`, inside the `makeStepRunner` returned function, BEFORE the cache/artifact/run dispatch (near where `step.Cache != nil` is checked), add:

```go
		if step.Approval != nil {
			approved := WaitForApproval(stepCtx, a.Client, a.ID, c.RunID, step, 3*time.Second)
			if approved {
				_ = a.Client.ReportStep(stepCtx, a.ID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Succeeded", EndedAt: time.Now().UTC(),
				})
			} else {
				_ = a.Client.ReportStep(stepCtx, a.ID, api.StepReportRequest{
					RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Failed", EndedAt: time.Now().UTC(),
				})
				recordFailure()
			}
			return nil
		}
```

(Use the existing `recordFailure()` closure from `makeStepRunner` — confirm its name from Task 4/5 of the finally feature. The `if:` evaluation that precedes the dispatch already applies, so an approval step can itself be gated by `if:`.)

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/agent/ -run TestApproval -v && go test ./internal/agent/... && go build ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/approval.go internal/agent/client.go internal/agent/agent.go internal/agent/approval_test.go
git commit -m "feat(agent): WaitForApproval helper; approval step dispatch in standard agent"
```

---

## Task 7: Wire approval into the k8s agent

**Files:** Modify `internal/k8sagent/agent.go` (`makeRunStep`); Test `internal/k8sagent/orchestrate_test.go`.

**Interfaces:**
- Consumes: `agentlib.WaitForApproval` (Task 6), the k8s `makeRunStep` factory + its `failedFlag`.

- [ ] **Step 1: Write the failing test**

Append to `internal/k8sagent/orchestrate_test.go` a test that the orchestrate harness serves the agent approval endpoints and that an approved gate lets a later step run, a rejected gate skips it. Mirror Task 6's harness additions (add the two agent approval routes to the `runOrchestrate` mux, with a controllable decision).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate_Approval -v`
Expected: FAIL — no approval handling in `makeRunStep`.

- [ ] **Step 3: Dispatch in `makeRunStep`**

In `internal/k8sagent/agent.go`, inside the `makeRunStep` returned function, AFTER the `if:` skip gate and BEFORE the Running report / `stepExec`, add:

```go
		if step.Approval != nil {
			approved := agentlib.WaitForApproval(ctx, a.client, a.cfg.AgentID, c.RunID, step, 3*time.Second)
			status := "Succeeded"
			if !approved {
				status = "Failed"
			}
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: status, EndedAt: time.Now().UTC(),
			})
			if !approved && !step.ContinueOnError {
				failedFlag.Store(true)
			}
			return
		}
```

(`WaitForApproval` itself reports the `WaitingApproval` status; here we report only the terminal Succeeded/Failed. Confirm `agentlib` is the import alias for `internal/agent`.)

- [ ] **Step 4: Run tests + builds**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate -v && go vet ./internal/k8sagent/ && go build ./... && go build -tags k8s ./internal/k8sagent/`
Expected: PASS; all compile.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/orchestrate_test.go
git commit -m "feat(k8sagent): approval step dispatch via shared WaitForApproval"
```

---

## Task 8: CLI `approve` / `reject` commands

**Files:** Create `internal/cli/approvals.go`; Modify `internal/cli/root.go`; Test `internal/cli/approvals_test.go` (if the package has CLI tests; otherwise manual + a httptest-based test).

**Interfaces:**
- Produces: `unified-cd approve <runID> <stepIndex> [--comment ...]` and `unified-cd reject <runID> <stepIndex> [--comment ...]`, POSTing to `/api/v1/runs/{runID}/approvals/{stepIndex}`.

- [ ] **Step 1: Write the command**

Create `internal/cli/approvals.go` mirroring `internal/cli/token.go`'s POST pattern:

```go
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

func newApprovalDecisionCmd(resolve func() (Config, error), use, short, decision string) *cobra.Command {
	var comment string
	cmd := &cobra.Command{
		Use:   use + " <run-id> <step-index>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			body, _ := json.Marshal(api.ApprovalDecisionRequest{Decision: decision, Comment: comment})
			url := cfg.Server + "/api/v1/runs/" + args[0] + "/approvals/" + args[1]
			req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("server: %s (%d)", string(b), resp.StatusCode)
			}
			fmt.Printf("%sd step %s of run %s\n", decision, args[1], args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "optional comment")
	return cmd
}

func newApproveCmd(resolve func() (Config, error)) *cobra.Command {
	return newApprovalDecisionCmd(resolve, "approve", "Approve a waiting approval step", "approve")
}
func newRejectCmd(resolve func() (Config, error)) *cobra.Command {
	return newApprovalDecisionCmd(resolve, "reject", "Reject a waiting approval step", "reject")
}
```

In `internal/cli/root.go`, register both:

```go
	root.AddCommand(newApproveCmd(resolve))
	root.AddCommand(newRejectCmd(resolve))
```

(Confirm the `Config` field names `Server`/`Token` and `resolve` signature against `internal/cli/root.go`.)

- [ ] **Step 2: Verify**

Run: `go build ./... && go run ./cmd/unified-cli approve --help`
Expected: builds; help shows `approve <run-id> <step-index>` with `--comment`.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/approvals.go internal/cli/root.go
git commit -m "feat(cli): approve/reject commands"
```

---

## Task 9: Web UI Approve/Reject buttons

**Files:** Modify `web/src/routes/RunDetail.svelte`.

- [ ] **Step 1: Add the actions + buttons**

In `web/src/routes/RunDetail.svelte`, add action functions mirroring `cancelRun`:

```javascript
async function decideApproval(stepIndex, decision, comment) {
    try {
        await apiFetch("/api/v1/runs/" + runID + "/approvals/" + stepIndex, {
            method: "POST",
            body: JSON.stringify({ decision, comment: comment || "" }),
        });
        await loadSteps();
    } catch (e) {
        error = e.message;
    }
}
```

In the step rendering, when a step's `status === "WaitingApproval"`, render Approve/Reject buttons (and an optional comment input) that call `decideApproval(step.index, "approve"|"reject", comment)`. Follow the existing step-row markup and button classes used by the cancel button. Match the existing Svelte 5 reactivity style in the file (runes or stores as used).

- [ ] **Step 2: Verify**

Run: `cd web && npm run build` (or the repo's `make ui-build`).
Expected: builds without error.

- [ ] **Step 3: Commit**

```bash
git add web/src/routes/RunDetail.svelte
git commit -m "feat(ui): approve/reject buttons for waiting-approval steps"
```

---

## Task 10: Docs + schema regen

**Files:** Modify `docs/jobs.md`; regenerate `schemas/unified-cd.schema.json`, `docs/field-reference.md`.

- [ ] **Step 1: Regenerate**

Run: `go generate ./internal/dsl/`
Verify `git diff --stat schemas/ docs/field-reference.md` shows `approval`/`ApprovalStep`. If absent, investigate (the generator parses `internal/dsl/types.go`).

- [ ] **Step 2: Document in `docs/jobs.md`**

Add an `## Approval Step (approval)` section: the YAML example, that any authenticated user approves/rejects via CLI (`unified-cd approve <run> <step>`) / Web UI / API, that reject or timeout fails the step (so `finally` runs), default timeout 60 min, and that the agent is held while waiting (so prefer short approvals / set a timeout). Add a TOC entry. Note the v1 limitation that a timeout leaves the audit row Pending (if that path was kept minimal).

- [ ] **Step 3: Verify + commit**

Run: `python -c "import json;json.load(open('schemas/unified-cd.schema.json'))" && grep -c approval schemas/unified-cd.schema.json` (expect ≥1).

```bash
git add docs/jobs.md docs/field-reference.md schemas/unified-cd.schema.json
git commit -m "docs: document approval step; regenerate schema"
```

---

## Final verification

- [ ] `go build ./...` and `go build -tags k8s ./internal/k8sagent/` compile.
- [ ] `go test ./... -short` green (store/controller approval tests may SKIP without Postgres — note which skipped).
- [ ] Manual smoke (if a stack is available): a job with `build → approval → deploy`; `unified-cd approve` continues, `reject` fails the run and runs `finally`.

## Self-review notes (coverage vs spec)

- Approver = any authenticated principal; `decided_by` recorded → Tasks 1, 5.
- Run-trigger identity recorded as `TriggeredBy` (principal name, fallback `"api"`) → Task 1 (Steps 6–9).
- `approval` step action + validation + finally-rejection → Task 2.
- `run_approvals` store + idempotent create + conditional decide → Task 3.
- Compile + default timeout 60 → Task 4.
- Decision/list (ServerAuth) + agent create/poll (BearerAuth) endpoints, 404/409 → Task 5.
- Agent-blocking poll via shared `WaitForApproval`; reject/timeout → fail → finally; `WaitingApproval` status → Tasks 6, 7.
- CLI + UI → Tasks 8, 9.
- Docs + schema → Task 10.
- **Deviation from spec (flagged):** agent creates+polls via `/agents/*` endpoints instead of controller-infers-on-WaitingApproval-report (auth-correct, avoids spec re-parse). **v1 limitation:** on timeout the audit row stays `Pending` (no agent decision endpoint) unless the optional timeout-record endpoint is added.
