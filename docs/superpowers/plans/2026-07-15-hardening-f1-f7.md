# Hardening F1–F7 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the seven audit findings: audit-middleware body truncation (F1), missing terminal/ownership guards on agent write endpoints (F2/F3), poison-candidate starvation in the archiver/retention/git-resolver loops (F4/F5/F6), immortal-Pending git runs (F6), and the cache meta-failure object leak (F7).

**Architecture:** F1 is a body-reconstruction fix in one middleware. F2/F3 are one shared `agentRunGuard` helper (with an immutable-`claimed_by` LRU cache) wired into eight handlers. F4/F5/F6 share one `failureBackoff` type whose exclusion list feeds each loop's candidate SQL (`!= ALL($n)`). F6's deadline and F7's compensating delete are small local changes.

**Tech Stack:** Go, pgx, chi, testify, `store.NewTestPostgres` (dockerized Postgres).

**Spec:** `docs/superpowers/specs/2026-07-15-hardening-f1-f7-design.md`

## Global Constraints

- All code, comments, commit messages, docs in **English** (AGENTS.md). No PII.
- Worktree `../unified-cd-hardening`, branch `hardening` (base main). Never commit from the main tree.
- F3 is enforced immediately: ownership mismatch → **403** with body `run %s is claimed by another agent`. Terminal no-ops → **200 `{"runId": ..., "alreadyFinalized": true}`**.
- Backoff: base **1 min**, doubling, max **1 h**, cap **10 000** entries.
- Resolve deadline: default **1 h**, env `UNIFIED_GIT_RESOLVE_DEADLINE` (Go duration; unset/invalid/`0` → default).
- No schema changes in this branch.
- Store integration tests need Docker (skip under `-short`). `docs/field-reference.md` is generated — untouched.

---

### Task 1: F1 — audit middleware body pass-through

**Files:**
- Modify: `internal/controller/audit.go:169-173` (body buffering in `auditLogMiddleware`)
- Test: `internal/controller/audit_passthrough_test.go` (create)

**Interfaces:**
- Consumes: existing `auditBodyPeekLimit`, `auditLogMiddleware(st interface{ InsertAuditLog(...) error })`.
- Produces: no new symbols; handlers now receive the complete request body.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/audit_passthrough_test.go`:

```go
package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// auditRecorderStore captures InsertAuditLog calls; satisfies the middleware's
// structural interface.
type auditRecorderStore struct {
	actions   []string
	resources []string
}

func (f *auditRecorderStore) InsertAuditLog(ctx context.Context, actor, method, path, action, resource string, status int) error {
	f.actions = append(f.actions, action)
	f.resources = append(f.resources, resource)
	return nil
}

// TestAuditMiddleware_PassesFullBodyDownstream: the 64 KiB audit peek must
// never truncate what the handler sees (a >64 KiB job YAML or secret used to
// be silently cut).
func TestAuditMiddleware_PassesFullBodyDownstream(t *testing.T) {
	big := strings.Repeat("a", 100_000) // well past auditBodyPeekLimit

	var got string
	router := chi.NewRouter()
	router.Use(auditLogMiddleware(&auditRecorderStore{}))
	router.Post("/api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		got = string(b)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", strings.NewReader(big))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Len(t, got, len(big), "handler must receive the full body, not the audit peek")
	assert.Equal(t, big, got)
}

// TestAuditMiddleware_SmallBodyNameExtractionUnchanged: audit still extracts
// body-derived resource names for normal-sized envelopes.
func TestAuditMiddleware_SmallBodyNameExtractionUnchanged(t *testing.T) {
	st := &auditRecorderStore{}
	router := chi.NewRouter()
	router.Use(auditLogMiddleware(st))
	router.Post("/api/v1/secrets", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(`{"name":"AWS_KEY","value":"x"}`))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, st.resources, 1)
	assert.Equal(t, "AWS_KEY", st.resources[0])
}
```

If `classifyAudit` does not map `POST /api/v1/secrets` to a body-named action in `auditActionTable`/`auditBodyNameSource`, pick a route from those tables that does (check `internal/controller/audit.go`'s tables) and adjust the second test's route + payload accordingly — do not weaken the assertion that a resource name was extracted.

- [ ] **Step 2: Run the tests to verify the pass-through test fails**

Run: `cd /path/to/unified-cd-hardening && go test ./internal/controller/ -run TestAuditMiddleware -v`
Expected: `PassesFullBodyDownstream` FAILS (got 65537 bytes, want 100000); the small-body test passes already.

- [ ] **Step 3: Fix the middleware**

In `internal/controller/audit.go`, replace the body-buffering block:

```go
			// Peek at the body (bounded) for body-derived resource names, but
			// hand the handler the peeked bytes FOLLOWED BY the unread
			// remainder — the peek must never truncate what the handler sees
			// (a >64 KiB job YAML or secret would otherwise be silently cut).
			var reqBody []byte
			if r.Body != nil {
				origBody := r.Body
				reqBody, _ = io.ReadAll(io.LimitReader(origBody, auditBodyPeekLimit+1))
				r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(reqBody), origBody))
			}
```

(`io.NopCloser` matches the previous behavior — the HTTP server closes the underlying body itself.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run TestAuditMiddleware -v && go build ./...`
Expected: PASS (2 tests). Also run the pre-existing audit tests: `go test ./internal/controller/ -run 'Audit' -v` — all green.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/audit.go internal/controller/audit_passthrough_test.go
git commit -m "fix(audit): stop truncating request bodies at the 64 KiB audit peek"
```

---

### Task 2: F2/F3 — `agentRunGuard` helper + claimed-by cache

**Files:**
- Create: `internal/controller/agent_guard.go`
- Test: `internal/controller/agent_guard_test.go` (create)

**Interfaces:**
- Consumes: `store.Store.GetRun(ctx, id) (*api.Run, error)` (`store.ErrRunNotFound`; `api.Run.ClaimedBy string` — empty until claimed), `isTerminalStatus(string) bool` (exists in the package), `writeJSON`.
- Produces (Task 3 wires these exact names):
  - `type runWriteVerdict int` with `runWriteOK`, `runWriteNotFound`, `runWriteNotOwned`, `runWriteTerminal`.
  - `(s *Server) agentRunGuard(ctx context.Context, agentID, runID string, rejectTerminal bool) (runWriteVerdict, error)`.
  - `respondRunWriteVerdict(w http.ResponseWriter, v runWriteVerdict, runID string) bool` — writes 404/403/200-alreadyFinalized and reports handled.
  - `Server.claimedBy *claimedByCache` field, lazily usable when nil-safe (initialize in `NewServer`).

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/agent_guard_test.go`:

```go
package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeGuardStore serves GetRun from a map and counts calls (to prove the
// claimed-by cache short-circuits repeat lookups on the hot path).
type fakeGuardStore struct {
	store.Store
	runs     map[string]*api.Run
	getCalls int
}

func (f *fakeGuardStore) GetRun(ctx context.Context, id string) (*api.Run, error) {
	f.getCalls++
	if r, ok := f.runs[id]; ok {
		return r, nil
	}
	return nil, store.ErrRunNotFound
}

func guardServer(st store.Store) *Server {
	return NewServer(Config{AgentToken: "agent-secret"}, st)
}

func TestAgentRunGuard_Verdicts(t *testing.T) {
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live":     {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
		"done":     {ID: "done", Status: api.RunSucceeded, ClaimedBy: "a1"},
		"unclaimed": {ID: "unclaimed", Status: api.RunPending},
	}}
	s := guardServer(st)
	ctx := context.Background()

	cases := []struct {
		name           string
		agent, run     string
		rejectTerminal bool
		want           runWriteVerdict
	}{
		{"owner live", "a1", "live", true, runWriteOK},
		{"owner live no-terminal-check", "a1", "live", false, runWriteOK},
		{"wrong agent", "a2", "live", true, runWriteNotOwned},
		{"missing run", "a1", "nope", true, runWriteNotFound},
		{"unclaimed run", "a1", "unclaimed", true, runWriteNotOwned},
		{"terminal rejected", "a1", "done", true, runWriteTerminal},
		{"terminal allowed when not rejecting", "a1", "done", false, runWriteOK},
	}
	for _, c := range cases {
		v, err := s.agentRunGuard(ctx, c.agent, c.run, c.rejectTerminal)
		require.NoError(t, err, c.name)
		assert.Equal(t, c.want, v, c.name)
	}
}

func TestAgentRunGuard_CachesClaimedBy(t *testing.T) {
	st := &fakeGuardStore{runs: map[string]*api.Run{
		"live": {ID: "live", Status: api.RunRunning, ClaimedBy: "a1"},
	}}
	s := guardServer(st)
	ctx := context.Background()

	// First call populates the cache; subsequent non-terminal-checking calls
	// must not hit the store again (this is the hot log path).
	_, err := s.agentRunGuard(ctx, "a1", "live", false)
	require.NoError(t, err)
	after := st.getCalls
	for i := 0; i < 5; i++ {
		v, err := s.agentRunGuard(ctx, "a1", "live", false)
		require.NoError(t, err)
		assert.Equal(t, runWriteOK, v)
	}
	assert.Equal(t, after, st.getCalls, "cached ownership must not re-query")

	// Ownership mismatch is also answerable from cache.
	v, err := s.agentRunGuard(ctx, "a2", "live", false)
	require.NoError(t, err)
	assert.Equal(t, runWriteNotOwned, v)
	assert.Equal(t, after, st.getCalls)

	// rejectTerminal always needs live status: it must re-query.
	_, err = s.agentRunGuard(ctx, "a1", "live", true)
	require.NoError(t, err)
	assert.Greater(t, st.getCalls, after)
}

func TestClaimedByCache_EvictsPastCap(t *testing.T) {
	c := newClaimedByCache(3)
	for i := 0; i < 5; i++ {
		c.put(fmt.Sprintf("run-%d", i), "a1")
	}
	assert.Equal(t, 3, c.len())
	_, ok := c.get("run-0")
	assert.False(t, ok, "oldest entries must be evicted")
	_, ok = c.get("run-4")
	assert.True(t, ok)
}
```

Note: `api.RunPending`/`api.RunRunning` — confirm the exact status constant names in `internal/api/types.go` and adjust (the terminal set is `RunSucceeded`/`RunFailed`/`RunCancelled`).

- [ ] **Step 2: Run to verify compile failure**

Run: `go test ./internal/controller/ -run 'TestAgentRunGuard|TestClaimedByCache' -v`
Expected: compile errors (`undefined: runWriteVerdict`, `newClaimedByCache`, ...).

- [ ] **Step 3: Implement**

Create `internal/controller/agent_guard.go`:

```go
package controller

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/eirueimi/unified-cd/internal/store"
)

// runWriteVerdict classifies an agent's write to a run. One policy, one
// place: ownership (the {agentId} path param must match runs.claimed_by) and
// optionally terminal-state rejection.
type runWriteVerdict int

const (
	runWriteOK runWriteVerdict = iota
	runWriteNotFound               // run does not exist -> 404
	runWriteNotOwned               // claimed by another (or no) agent -> 403
	runWriteTerminal               // run already finished -> 200 alreadyFinalized no-op
)

// claimedByCacheCap bounds the runID -> claimed_by cache. claimed_by is
// immutable once set, so entries never go stale; the cap only bounds memory.
const claimedByCacheCap = 10_000

type claimedByEntry struct {
	runID string
	owner string
}

// claimedByCache is a bounded LRU of immutable runID -> claimed_by pairs so
// the per-log-line ownership check is a memory lookup, not a DB query.
type claimedByCache struct {
	mu    sync.Mutex
	m     map[string]*list.Element
	order *list.List // front = most recently used
	cap   int
}

func newClaimedByCache(cap int) *claimedByCache {
	return &claimedByCache{m: map[string]*list.Element{}, order: list.New(), cap: cap}
}

func (c *claimedByCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

func (c *claimedByCache) get(runID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[runID]
	if !ok {
		return "", false
	}
	c.order.MoveToFront(el)
	return el.Value.(*claimedByEntry).owner, true
}

func (c *claimedByCache) put(runID, owner string) {
	if owner == "" {
		return // only immutable, non-empty values are cacheable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[runID]; ok {
		return
	}
	el := c.order.PushFront(&claimedByEntry{runID: runID, owner: owner})
	c.m[runID] = el
	for len(c.m) > c.cap {
		oldest := c.order.Back()
		e := oldest.Value.(*claimedByEntry)
		c.order.Remove(oldest)
		delete(c.m, e.runID)
	}
}

// agentRunGuard validates an agent write against the target run. With
// rejectTerminal=false a cached ownership match answers without touching the
// DB (claimed_by never changes once set); rejectTerminal=true always fetches
// the run because status is only sticky once terminal.
func (s *Server) agentRunGuard(ctx context.Context, agentID, runID string, rejectTerminal bool) (runWriteVerdict, error) {
	if !rejectTerminal {
		if owner, ok := s.claimedBy.get(runID); ok {
			if owner == agentID {
				return runWriteOK, nil
			}
			return runWriteNotOwned, nil
		}
	}
	run, err := s.store.GetRun(ctx, runID)
	if errors.Is(err, store.ErrRunNotFound) {
		return runWriteNotFound, nil
	}
	if err != nil {
		return runWriteOK, err
	}
	s.claimedBy.put(runID, run.ClaimedBy)
	if run.ClaimedBy == "" || run.ClaimedBy != agentID {
		return runWriteNotOwned, nil
	}
	if rejectTerminal && isTerminalStatus(string(run.Status)) {
		return runWriteTerminal, nil
	}
	return runWriteOK, nil
}

// respondRunWriteVerdict writes the response for a non-OK verdict and
// reports whether it handled the request.
func respondRunWriteVerdict(w http.ResponseWriter, v runWriteVerdict, runID string) bool {
	switch v {
	case runWriteNotFound:
		http.Error(w, "run not found", http.StatusNotFound)
		return true
	case runWriteNotOwned:
		http.Error(w, fmt.Sprintf("run %s is claimed by another agent", runID), http.StatusForbidden)
		return true
	case runWriteTerminal:
		writeJSON(w, http.StatusOK, map[string]any{"runId": runID, "alreadyFinalized": true})
		return true
	}
	return false
}
```

In `internal/controller/server.go`: add field `claimedBy *claimedByCache` to `Server` and initialize `claimedBy: newClaimedByCache(claimedByCacheCap)` in `NewServer` (find the struct-literal construction and add the field there).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestAgentRunGuard|TestClaimedByCache' -v && go build ./...`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/agent_guard.go internal/controller/agent_guard_test.go internal/controller/server.go
git commit -m "feat(controller): agentRunGuard with immutable claimed_by cache"
```

---

### Task 3: F2/F3 — wire the guard into the eight agent handlers

**Files:**
- Modify: `internal/controller/api_agent.go` (`handleAgentStepReport` ~:410, `handleAgentLogAppend` ~:452, `handleAgentFinishRun` ~:485, `handleAgentSetStepOutputs` ~:525, `handleAgentSetRunOutputs` ~:549, `handleAgentLogBulk` ~:566, `handleAgentSidecarStatus` ~:596)
- Modify: `internal/controller/api_approvals.go:69-86` (`handleAgentCreateApproval`)
- Test: `internal/controller/agent_guard_http_test.go` (create)

**Interfaces:**
- Consumes: `agentRunGuard` / `respondRunWriteVerdict` (Task 2). Route params: `{agentId}` on all agent routes; `{runId}` on finish/outputs/bulk/sidecars/approvals routes; step-report and single log append carry `RunID` in the body.
- Produces: no new symbols. Behavior matrix (spec): outputs×2, sidecar, approval-create → ownership + terminal; log append/bulk → ownership only; finish, step-report → ownership only (existing terminal CAS/guard unchanged).

- [ ] **Step 1: Write the failing HTTP matrix test**

Create `internal/controller/agent_guard_http_test.go`. Seeding a claimed run: follow the established pattern in `internal/controller/api_agent_test.go` (register an agent then claim via the claim endpoint or the store's claim method — copy whichever helper those tests use; if none exists, register via `POST /api/v1/agents/register` and claim via `POST /api/v1/agents/{id}/claim` after making the run Queued the way those tests do). The test must produce: a run with `ClaimedBy == "owner-agent"` in status Running, plus a terminal run claimed by the same agent.

```go
package controller

// Matrix: every agent write endpoint must 403 for a non-owning agent (no
// mutation), and the four F2 endpoints must no-op with alreadyFinalized on
// terminal runs. Correct-agent live-run behavior is covered by pre-existing
// endpoint tests, which must stay green.

// Pseudostructure (fill in using the seeding helper established above):
//   for each endpoint in:
//     POST /api/v1/agents/{agent}/steps                         (body RunID)
//     POST /api/v1/agents/{agent}/logs                          (body RunID)
//     POST /api/v1/agents/{agent}/runs/{run}/finish
//     POST /api/v1/agents/{agent}/runs/{run}/steps/0/outputs
//     POST /api/v1/agents/{agent}/runs/{run}/outputs
//     POST /api/v1/agents/{agent}/runs/{run}/steps/0/logs/bulk  (body RunID per line)
//     POST /api/v1/agents/{agent}/runs/{run}/sidecars           (body RunID)
//     POST /api/v1/agents/{agent}/runs/{run}/approvals
//   assert with agent = "intruder-agent": 403, body contains
//     "is claimed by another agent", and the write did NOT land
//     (CountLogs / GetRunOutputs / GetRunSteps / ListSidecarStatuses /
//      GetApproval as appropriate).
//   for outputs/outputs-step/sidecars/approvals with agent = owner but the
//     TERMINAL run: 200, body contains "alreadyFinalized", write did NOT land.
```

Write it as real Go table-driven code (each case: method, path builder, body, expected status, expected body substring, and a `verify func(t, st)` asserting non-mutation). This is the largest test in the branch — budget for it.

- [ ] **Step 2: Run to verify failures**

Run: `go test ./internal/controller/ -run TestAgentGuardHTTP -v`
Expected: FAIL — today every intruder write returns 2xx and mutates.

- [ ] **Step 3: Wire the guard**

Pattern per handler — insert after the body/params are decoded and before the first store write. Examples (repeat consistently; `agentID := chi.URLParam(r, "agentId")` in each):

`handleAgentSetStepOutputs` and `handleAgentSetRunOutputs` (ownership + terminal):

```go
	agentID := chi.URLParam(r, "agentId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, runID, true)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, runID) {
		return
	}
```

`handleAgentSidecarStatus` (ownership + terminal — use `req.RunID` after decode).

`handleAgentCreateApproval` (ownership + terminal — `runID` from the path).

`handleAgentLogAppend` (ownership only — after decode, `rejectTerminal=false`, run ID `req.RunID`).

`handleAgentLogBulk` (ownership only — guard each DISTINCT `req.RunID` in the loop; the cache makes repeats free):

```go
	agentID := chi.URLParam(r, "agentId")
	guarded := map[string]bool{}
	for _, req := range lines {
		if !guarded[req.RunID] {
			v, gerr := s.agentRunGuard(r.Context(), agentID, req.RunID, false)
			if gerr != nil {
				http.Error(w, gerr.Error(), http.StatusInternalServerError)
				return
			}
			if respondRunWriteVerdict(w, v, req.RunID) {
				return
			}
			guarded[req.RunID] = true
		}
		// ... existing timestamp/AppendLog/drop-count body unchanged ...
	}
```

`handleAgentFinishRun` (ownership only, before the `FinishRun` call — the existing CAS + `alreadyFinalized` terminal handling stays exactly as is).

`handleAgentStepReport` (ownership only, after decoding `req` and BEFORE the existing terminal-status block — keep that block unchanged).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/controller/ -count=1` (full package — the matrix test plus every pre-existing agent/e2e-style test must be green; pre-existing tests exercise the owner path, so any 403 regression here means a test seeded a run without claiming it — fix by claiming with the same agent ID the test sends, never by weakening the guard) and `go build ./...`.
Also run `go test ./test/e2e/ -count=1` if it compiles without extra infra (it uses dockerized PG; if it needs services that aren't available, note that in the report instead).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/api_agent.go internal/controller/api_approvals.go internal/controller/agent_guard_http_test.go
git commit -m "feat(api): enforce run ownership and terminal no-ops on agent writes"
```

---

### Task 4: F4/F5/F6 — `failureBackoff`

**Files:**
- Create: `internal/controller/failure_backoff.go`
- Test: `internal/controller/failure_backoff_test.go` (create)

**Interfaces:**
- Produces (Tasks 5–6 use these exact names):
  - `newFailureBackoff(base, max time.Duration, cap int) *failureBackoff`
  - `(b *failureBackoff) Excluded(now time.Time) []string` — IDs whose retryAt is after `now` (never nil; empty slice when none).
  - `(b *failureBackoff) Failure(id string, now time.Time)` — increments the entry's failure count and sets `retryAt = now + min(base·2^(n-1), max)`.
  - `(b *failureBackoff) Success(id string)` — removes the entry.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/failure_backoff_test.go`:

```go
package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFailureBackoff_ScheduleAndRecovery(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newFailureBackoff(time.Minute, time.Hour, 100)

	assert.Empty(t, b.Excluded(now))

	b.Failure("r1", now)
	assert.Equal(t, []string{"r1"}, b.Excluded(now), "excluded immediately after failure")
	assert.Empty(t, b.Excluded(now.Add(61*time.Second)), "retryable after base backoff")

	// Second consecutive failure doubles the wait.
	b.Failure("r1", now.Add(61*time.Second))
	assert.NotEmpty(t, b.Excluded(now.Add(61*time.Second+90*time.Second)), "2 min after second failure: still excluded")
	assert.Empty(t, b.Excluded(now.Add(61*time.Second+121*time.Second)))

	// Backoff is capped at max.
	for i := 0; i < 20; i++ {
		b.Failure("r1", now)
	}
	assert.Empty(t, b.Excluded(now.Add(time.Hour+time.Second)), "wait never exceeds max")

	// Success clears the entry entirely.
	b.Failure("r2", now)
	b.Success("r2")
	assert.Empty(t, b.Excluded(now))
}

func TestFailureBackoff_CapEvictsOldest(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newFailureBackoff(time.Minute, time.Hour, 3)
	for i := 0; i < 5; i++ {
		b.Failure(fmt.Sprintf("r%d", i), now)
	}
	ex := b.Excluded(now)
	assert.Len(t, ex, 3)
	assert.NotContains(t, ex, "r0")
	assert.NotContains(t, ex, "r1")
}
```

- [ ] **Step 2: Run to verify compile failure**

Run: `go test ./internal/controller/ -run TestFailureBackoff -v`
Expected: compile errors.

- [ ] **Step 3: Implement**

Create `internal/controller/failure_backoff.go`:

```go
package controller

import (
	"container/list"
	"sync"
	"time"
)

// failureBackoff tracks per-candidate consecutive failures for background
// sweep loops so a permanently-failing ("poison") candidate stops filling
// every oldest-first batch and starving the rest (the wedge class the
// log-trim sweeper fixed with SQL-side filtering; here failures aren't
// recorded in the DB, so the exclusion list is process-local). Leader-local
// by design: a failover or restart clears it, costing one retry per poison
// before it is re-excluded.
type failureBackoff struct {
	mu    sync.Mutex
	m     map[string]*list.Element
	order *list.List // front = most recently failed
	base  time.Duration
	max   time.Duration
	cap   int
}

type backoffEntry struct {
	id       string
	failures int
	retryAt  time.Time
}

func newFailureBackoff(base, max time.Duration, cap int) *failureBackoff {
	return &failureBackoff{m: map[string]*list.Element{}, order: list.New(), base: base, max: max, cap: cap}
}

// Failure records one more consecutive failure for id: the wait doubles per
// failure (base, 2·base, 4·base, ...) up to max.
func (b *failureBackoff) Failure(id string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var e *backoffEntry
	if el, ok := b.m[id]; ok {
		e = el.Value.(*backoffEntry)
		b.order.MoveToFront(el)
	} else {
		e = &backoffEntry{id: id}
		b.m[id] = b.order.PushFront(e)
		for len(b.m) > b.cap {
			oldest := b.order.Back()
			old := oldest.Value.(*backoffEntry)
			b.order.Remove(oldest)
			delete(b.m, old.id)
		}
	}
	e.failures++
	wait := b.base << (e.failures - 1)
	if wait > b.max || wait <= 0 { // <=0 guards shift overflow
		wait = b.max
	}
	e.retryAt = now.Add(wait)
}

// Success forgets id (next failure starts from the base wait again).
func (b *failureBackoff) Success(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if el, ok := b.m[id]; ok {
		b.order.Remove(el)
		delete(b.m, id)
	}
}

// Excluded returns the ids still inside their backoff window. Never nil.
func (b *failureBackoff) Excluded(now time.Time) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []string{}
	for id, el := range b.m {
		if el.Value.(*backoffEntry).retryAt.After(now) {
			out = append(out, id)
		}
	}
	return out
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/controller/ -run TestFailureBackoff -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/failure_backoff.go internal/controller/failure_backoff_test.go
git commit -m "feat(controller): shared failure backoff for sweep loops"
```

---

### Task 5: F4/F5 — wire backoff into archiver and retention sweeper

**Files:**
- Modify: `internal/store/store.go` (`ListRunsNeedingArchival`, `ListExpiredRuns` signatures)
- Modify: `internal/store/postgres.go:1293` (`ListRunsNeedingArchival`), `:1328` (`ListExpiredRuns`)
- Modify: `internal/controller/archiver.go` (`RunLogArchiver`, `runArchiveAsLeader`, `archivePendingLogs`)
- Modify: `internal/controller/run_retention.go` (`RunRunRetention`, `runRunRetentionOnce`)
- Test: `internal/store/postgres_excluded_test.go` (create); extend `internal/controller/archiver_test.go` and `internal/controller/run_retention_test.go`

**Interfaces:**
- Consumes: `failureBackoff` (Task 4).
- Produces: `ListRunsNeedingArchival(ctx, limit int, excluded []string) ([]api.Run, error)`; `ListExpiredRuns(ctx, cutoff time.Time, limit int, excluded []string) ([]string, error)` — both add `AND id != ALL($n::uuid[])` (an empty slice excludes nothing: `x != ALL('{}')` is true). All callers updated. `runRunRetentionOnce(ctx, st, obj, retentionDays int, bo *failureBackoff)`; `archivePendingLogs(ctx, st, obj, bo *failureBackoff)` (and the leader wrapper threads it through).

- [ ] **Step 1: Write the failing tests**

`internal/store/postgres_excluded_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExcludedParam: both sweep candidate queries must honor the excluded
// list, and an empty list must exclude nothing.
func TestExcludedParam(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	mkTerminal := func() string {
		run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
		require.NoError(t, err)
		require.NoError(t, pg.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
		_, err = pg.pool.Exec(ctx, `UPDATE runs SET updated_at = NOW() - interval '40 days' WHERE id = $1`, run.ID)
		require.NoError(t, err)
		return run.ID
	}
	a, b := mkTerminal(), mkTerminal()

	// ListRunsNeedingArchival
	runs, err := pg.ListRunsNeedingArchival(ctx, 10, []string{})
	require.NoError(t, err)
	ids := []string{}
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	assert.Contains(t, ids, a)
	assert.Contains(t, ids, b)

	runs, err = pg.ListRunsNeedingArchival(ctx, 10, []string{a})
	require.NoError(t, err)
	ids = ids[:0]
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	assert.NotContains(t, ids, a)
	assert.Contains(t, ids, b)

	// ListExpiredRuns
	cutoff := time.Now().AddDate(0, 0, -30)
	got, err := pg.ListExpiredRuns(ctx, cutoff, 10, []string{b})
	require.NoError(t, err)
	assert.Contains(t, got, a)
	assert.NotContains(t, got, b)
}
```

Loop tests — extend the existing fakes:
- In `internal/controller/run_retention_test.go`, add `TestRunRetention_PoisonCandidateExcludedNextTick`: fake store whose `ListExpiredRuns` now receives `excluded` (extend `fakeRetentionStore.ListExpiredRuns` signature and record the argument); a candidate whose delete fails once → call `runRunRetentionOnce` twice with the SAME `failureBackoff` instance → second call's recorded `excluded` contains the poison ID.
- In `internal/controller/archiver_test.go`, mirror it: a run whose `archiveRunLogs` fails (failing object store) → second `archivePendingLogs` call receives it in `excluded`; a succeeding run gets `Success` (not excluded).

- [ ] **Step 2: Run to verify failures**

Run: `go test ./internal/store/ -run TestExcludedParam -v; go test ./internal/controller/ -run 'PoisonCandidate' -v`
Expected: compile errors (signature changes not made yet).

- [ ] **Step 3: Implement**

`postgres.go` — `ListRunsNeedingArchival` query becomes:

```sql
		SELECT id, job_name, status, params, created_at, updated_at
		FROM runs
		WHERE status IN ('Succeeded', 'Failed', 'Cancelled')
		  AND id NOT IN (SELECT run_id FROM run_log_archives)
		  AND id != ALL($2::uuid[])
		ORDER BY updated_at
		LIMIT $1;
```

(pass `excluded` as the second arg; keep scan loop unchanged). `ListExpiredRuns` adds `AND id != ALL($3::uuid[])` with `excluded` as `$3`. Update the `Store` interface lines and every caller/fake (`grep -rn "ListRunsNeedingArchival\|ListExpiredRuns(" internal/`); production callers pass `bo.Excluded(time.Now())`, tests without a backoff pass `[]string{}`.

`archiver.go`:

```go
func RunLogArchiver(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration) {
	// ... existing ticker setup ...
	bo := newFailureBackoff(time.Minute, time.Hour, 10_000)
	for { // existing loop
		// ...
		runArchiveAsLeader(ctx, st, obj, bo)
	}
}
```

`archivePendingLogs(ctx, st, obj, bo)`: fetch with `st.ListRunsNeedingArchival(ctx, 20, bo.Excluded(time.Now()))`; on `archiveRunLogs` error → `bo.Failure(run.ID, time.Now())` (keep the existing error log); on success → `bo.Success(run.ID)`.

`run_retention.go`: `RunRunRetention` creates `bo := newFailureBackoff(time.Minute, time.Hour, 10_000)` and passes it to `runRunRetentionOnce(ctx, st, obj, retentionDays, bo)`; the candidate fetch becomes `st.ListExpiredRuns(ctx, cutoff, runRetentionBatchSize, bo.Excluded(time.Now()))`; `deleteRunEverywhere` failure → `bo.Failure(id, time.Now())`, success → `bo.Success(id)`. Existing tests that call `runRunRetentionOnce` directly pass a fresh `newFailureBackoff(time.Minute, time.Hour, 10_000)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/store/ ./internal/controller/ -count=1 && go build ./...`
Expected: PASS including all pre-existing archiver/retention tests (updated signatures only — no behavior change when nothing has failed).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_excluded_test.go internal/controller/archiver.go internal/controller/archiver_test.go internal/controller/run_retention.go internal/controller/run_retention_test.go
git commit -m "fix(controller): poison-candidate backoff for archiver and retention sweeps"
```

---

### Task 6: F6 — git resolver: backoff + resolve deadline

**Files:**
- Modify: `internal/store/store.go` + `internal/store/postgres.go:1932` (`ListPendingRuns` gains `excluded []string`; `PendingRun` gains `CreatedAt time.Time`, SELECT adds `created_at`)
- Modify: `internal/controller/scheduler.go:263-314` (`resolveGitPendingRuns`) and its caller `RunGitResolver`
- Modify: `cmd/controller/main.go` (deadline resolver + pass-through)
- Test: extend the resolver's existing test file (find it: `grep -rn "resolveGitPendingRuns" internal/ --include=*_test.go`); `internal/store/postgres_excluded_test.go` (append a `ListPendingRuns` case)

**Interfaces:**
- Consumes: `failureBackoff` (Task 4); `AppendLog(ctx, runID, -1, "stderr", time.Now(), msg)` (the `-1` step index is the system-message sentinel; see `tryQueueRun`'s usage); `MarkRunFinished`.
- Produces: `ListPendingRuns(ctx, limit int, excluded []string) ([]PendingRun, error)`; `PendingRun.CreatedAt`; `resolveGitPendingRuns(ctx, st, resolver, km, bo *failureBackoff, deadline time.Duration)`; `RunGitResolver(..., deadline time.Duration)` (append the param); `gitResolveDeadlineDefault() time.Duration` in `cmd/controller/main.go` (env `UNIFIED_GIT_RESOLVE_DEADLINE`, default `time.Hour`; unset/invalid/<=0 → default with a warn for invalid).

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/postgres_excluded_test.go`:

```go
func TestListPendingRuns_ExcludedAndCreatedAt(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	a, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	b, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)

	runs, err := pg.ListPendingRuns(ctx, 10, []string{a.ID})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, b.ID, runs[0].ID)
	assert.WithinDuration(t, time.Now(), runs[0].CreatedAt, time.Minute, "CreatedAt must be populated")
}
```

Resolver deadline test (in the resolver's existing test file, following its established fake/stub pattern for `gittemplate.Resolver` — if the existing tests drive `resolveGitPendingRuns` against real Postgres with an unreachable git URL, reuse that): seed a Pending run with a `git://` spec whose resolution fails transiently; call with `deadline = time.Hour` → run stays Pending and the backoff records a failure; backdate `created_at` by 2 hours (`UPDATE runs SET created_at = ...` via the test's store handle if in package store, else via a seeded old run) and call again with a FRESH backoff (so it isn't excluded) → run becomes Failed and its logs contain `git template resolution failed for more than`.

- [ ] **Step 2: Run to verify failures**

Run: `go test ./internal/store/ -run TestListPendingRuns_Excluded -v`
Expected: compile error (signature).

- [ ] **Step 3: Implement**

`postgres.go`:

```go
func (p *Postgres) ListPendingRuns(ctx context.Context, limit int, excluded []string) ([]PendingRun, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, spec, created_at FROM runs
		 WHERE status = 'Pending' AND id != ALL($2::uuid[])
		 ORDER BY created_at LIMIT $1`,
		limit, excluded)
	// ... scan &r.ID, &r.Spec, &r.CreatedAt ...
```

Add `CreatedAt time.Time` to `PendingRun` (in store.go where the type lives) and update the interface signature + all callers/fakes.

`scheduler.go` — `resolveGitPendingRuns(ctx, st, resolver, km, bo, deadline)`:

```go
	runs, err := st.ListPendingRuns(ctx, 50, bo.Excluded(time.Now()))
	...
	// transient-failure branch (the existing `slog.Warn("git resolver: resolve spec failed", ...)` case):
		bo.Failure(r.ID, time.Now())
		if time.Since(r.CreatedAt) > deadline {
			msg := fmt.Sprintf("git template resolution failed for more than %s: %v", deadline, err)
			slog.Error("git resolver: resolve deadline exceeded, failing run", "runID", r.ID, "age", time.Since(r.CreatedAt))
			if _, lerr := st.AppendLog(ctx, r.ID, -1, "stderr", time.Now(), msg); lerr != nil {
				slog.Warn("git resolver: append system log", "runID", r.ID, "error", lerr)
			}
			if ferr := st.MarkRunFinished(ctx, r.ID, api.RunFailed); ferr != nil {
				slog.Warn("git resolver: mark run failed failed", "runID", r.ID, "error", ferr)
			}
			bo.Success(r.ID) // run left the Pending set; forget it
		}
		continue
	// deterministic-failure branch: add bo.Success(r.ID) after MarkRunFinished
	// success branch (UpdateRunSpec): add bo.Success(r.ID)
```

`RunGitResolver` gains `deadline time.Duration`, creates `bo := newFailureBackoff(time.Minute, time.Hour, 10_000)`, and passes both down. `cmd/controller/main.go`:

```go
// gitResolveDeadlineDefault resolves UNIFIED_GIT_RESOLVE_DEADLINE (a Go
// duration). Runs whose git-template resolution keeps failing longer than
// this are Failed instead of staying Pending forever. Unset, invalid, or
// non-positive values fall back to 1h — 0 does NOT disable the deadline
// (disabling would reintroduce immortal-Pending runs).
func gitResolveDeadlineDefault() time.Duration {
	const def = time.Hour
	v := os.Getenv("UNIFIED_GIT_RESOLVE_DEADLINE")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid UNIFIED_GIT_RESOLVE_DEADLINE, using default", "value", v, "default", def)
		return def
	}
	return d
}
```

and pass `gitResolveDeadlineDefault()` at the `controller.RunGitResolver(...)` call site.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/store/ ./internal/controller/ -count=1 && go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_excluded_test.go internal/controller/scheduler.go cmd/controller/main.go
git add -A internal/controller  # resolver test file
git commit -m "fix(controller): git resolver backoff + resolve deadline fails immortal-Pending runs"
```

---

### Task 7: F7 — cache compensating delete

**Files:**
- Modify: `internal/cache/cache.go:41-65` (`Save`)
- Test: extend `internal/cache/` tests (find the existing test file; create `cache_save_test.go` if none fits)

**Interfaces:**
- Consumes: `objectstore.ObjectStore` (`Put`, `Delete` — Delete is nil for missing keys).
- Produces: no signature change; a failed meta Put no longer leaves an orphan archive object.

- [ ] **Step 1: Write the failing test**

```go
package cache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// putFailingStore fails the Nth Put (1-based) and delegates everything else.
type putFailingStore struct {
	objectstore.ObjectStore
	puts    int
	failPut int
}

func (f *putFailingStore) Put(ctx context.Context, key string, content io.Reader, size int64) error {
	f.puts++
	if f.puts == f.failPut {
		return errors.New("meta put failed")
	}
	return f.ObjectStore.Put(ctx, key, content, size)
}

// TestSave_MetaFailureDeletesArchive: a failed .meta Put must not leave an
// orphaned .tar.zst (GC and lookup only ever iterate .meta objects).
func TestSave_MetaFailureDeletesArchive(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644))
	inner := objectstore.NewLocalObjectStore(t.TempDir())
	st := &putFailingStore{ObjectStore: inner, failPut: 2} // archive Put ok, meta Put fails

	err := Save(context.Background(), st, dir, "key1", 7)
	require.Error(t, err)

	keys, err := inner.List(context.Background(), "")
	require.NoError(t, err)
	assert.Empty(t, keys, "no orphaned object may survive a failed save")
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/cache/ -run TestSave_MetaFailure -v`
Expected: FAIL — the `.tar.zst` object survives today.

- [ ] **Step 3: Implement**

In `Save`, wrap the meta Put:

```go
	if err := store.Put(ctx, oKey+".meta", bytes.NewReader(metaData), int64(len(metaData))); err != nil {
		// The archive object was already written; without its .meta it is
		// invisible to both lookup and GC (which iterate .meta only), so it
		// would leak forever. Compensate best-effort, like the log archiver
		// does on CreateLogArchive failure.
		if derr := store.Delete(ctx, oKey+".tar.zst"); derr != nil {
			slog.Warn("cache save: cleanup of orphaned archive failed", "key", oKey, "error", derr)
		}
		return fmt.Errorf("put meta: %w", err)
	}
```

Add `"log/slog"` to the imports if absent.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cache/ -count=1 -v && go build ./...`
Expected: PASS including pre-existing cache tests.

- [ ] **Step 5: Commit**

```bash
git add internal/cache/
git commit -m "fix(cache): delete orphaned archive object when the meta write fails"
```

---

### Task 8: Documentation + final sweep

**Files:**
- Modify: `docs/configuration.md` (env-only `UNIFIED_GIT_RESOLVE_DEADLINE` — put it in the env table; note there is deliberately no flag)
- Modify: `docs/troubleshooting.md` (two new entries)
- Modify: `docs/operations.md` (sweep-backoff paragraph)

**Interfaces:**
- Consumes: exact strings from Tasks 3 and 6.

- [ ] **Step 1: `docs/configuration.md`**

Env-table entry (mirror existing rows):

> `UNIFIED_GIT_RESOLVE_DEADLINE` — how long a run's `git://` template resolution may keep failing (network/credentials) before the run is Failed instead of waiting as Pending. Go duration, default `1h`. Deterministic resolution errors (e.g. a nonexistent ref) still fail the run immediately. There is no CLI flag; unset/invalid/non-positive values use the default.

- [ ] **Step 2: `docs/troubleshooting.md`** (mirror the file's Symptom/Cause/Fix format)

Entry 1:

> **Symptom:** agent requests fail with 403 `run <id> is claimed by another agent`.
> **Cause:** the run is owned by a different agent ID (`runs.claimed_by`). Common cause: a stale agent process from before a restart still flushing reports, or two agents configured with conflicting IDs.
> **Fix:** restart/retire the stale agent; ensure every agent has a unique ID. The rejected write was not applied.

Entry 2:

> **Symptom:** a run fails with log line `git template resolution failed for more than 1h0m0s: ...`.
> **Cause:** the job references a `git://` template whose host stayed unreachable (or credentials stayed invalid) past `UNIFIED_GIT_RESOLVE_DEADLINE`.
> **Fix:** fix the repository URL/credentials and re-trigger the run.

- [ ] **Step 3: `docs/operations.md`**

Add after the retention/tiered-log paragraphs:

> **Sweep failure backoff.** The log archiver, run-retention sweeper, and git resolver retry a persistently failing candidate with exponential backoff (1 min doubling to 1 h) instead of letting it occupy the head of every oldest-first batch — a handful of broken runs can no longer starve archival, deletion, or resolution for everything newer. The backoff state is held by the current leader only and resets on failover (each problem candidate is retried once, then re-excluded).

- [ ] **Step 4: Sweep and full test run**

```bash
grep -rn "GIT_RESOLVE_DEADLINE\|claimed by another agent\|resolution failed for more than" docs/ internal/ cmd/ | grep -v _test.go
go build ./... && go test ./internal/... -count=1
git status   # only the three doc files in this task's diff
```

Expected: strings consistent between code and docs; all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add docs/configuration.md docs/troubleshooting.md docs/operations.md
git commit -m "docs: hardening env var, 403/deadline troubleshooting, sweep backoff"
```
