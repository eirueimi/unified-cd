# Orphaned-run Reaper + Agent Heartbeat + k8s Pod GC — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Detect a dead agent via a dedicated heartbeat and fail its stranded `Running` runs (reason "agent lost"), plus GC leaked k8s pods — without false-positives against healthy saturated agents and without re-running steps.

**Architecture:** Agents send a periodic heartbeat (independent of claim polling) to a new controller endpoint that touches `last_seen_at`. A leader-elected controller reaper marks `Running` runs `Failed` when their claiming agent's heartbeat is stale (or the agent row is gone), via `MarkRunFinished` (per-run, so mutex/semaphore locks are released). The k8s-agent GCs `ucd-run-*` pods whose run is terminal/absent.

**Tech Stack:** Go, chi, PostgreSQL (advisory locks), client-go.

## Global Constraints

- Module path: `github.com/eirueimi/unified-cd`.
- `agentlib` is an alias for `internal/agent`; both the standard agent and k8s-agent use `*agent.Client`. A single `Client.Heartbeat` and a single `StartHeartbeat` helper serve both.
- The reaper MUST finalize each stranded run via `store.MarkRunFinished(id, api.RunFailed)` (NOT a bulk `UPDATE runs`), because `MarkRunFinished` also releases `mutex_holders` and `named_lock_slots` for the run — a bulk update would leak those locks and block other runs.
- The reaper Fails stranded runs; it MUST NOT re-queue them.
- Reaper is leader-elected via a PG advisory lock with a NEW key distinct from the existing ones (scheduler `0x65786364`, approval `0x61707276`, cache `0x63616368`, logArchiver `0x6C6F6761`, appSource `0x61707073`). Use `stuckRunReaperLockKey = int64(0x7374756B)` ('stuk').
- Thresholds (defaults; keep as named constants): heartbeat interval `15s`, reaper interval `30s`, `staleAfter = 90s`, `grace = 60s`. `staleAfter` MUST be < `DeleteStaleAgents`'s `5m` so the agent row is usually still present when the reaper acts (the LEFT JOIN covers the case where it isn't).
- Heartbeat is best-effort: a failed heartbeat logs and retries next tick; it never crashes the agent.
- `go build ./...` and each task's tests pass after every task.

---

### Task 1: Heartbeat endpoint + client method

**Files:**
- Modify: `internal/agent/client.go` (add `Heartbeat`)
- Modify: `internal/controller/api_agent.go` (add `handleAgentHeartbeat`)
- Modify: `internal/controller/server.go` (register the route)
- Test: `internal/controller/api_agent_test.go`

**Interfaces:**
- Produces: `func (c *Client) Heartbeat(ctx context.Context, agentID string) error` (POST `/api/v1/agents/{agentID}/heartbeat`, bearer agent token, expects 204).
- Produces: `func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request)` → `store.TouchAgent(agentID)` → 204.
- Consumes: existing `store.TouchAgent(ctx, agentID) error`.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/api_agent_test.go` (match the file's existing harness — `NewServer` + `store.NewTestPostgres`; read a neighbor test for the exact setup, including how an agent is registered and how `last_seen_at`/`GetAgent` is read):

```go
func TestAgentHeartbeat_TouchesLastSeen(t *testing.T) {
	s, st := newAgentTestServer(t) // reuse the package's helper; adapt name to the real one
	// register an agent so a row exists
	reg := api.AgentRegisterRequest{AgentID: "agent-hb", Hostname: "h", OS: "linux", Labels: []string{"kind:linux"}}
	body, _ := json.Marshal(reg)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/register", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent && rr.Code != http.StatusOK {
		t.Fatalf("register: %d", rr.Code)
	}
	before, _ := st.GetAgent(context.Background(), "agent-hb")

	time.Sleep(10 * time.Millisecond)
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-hb/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer agent-secret")
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("heartbeat = %d, want 204", rr.Code)
	}
	after, _ := st.GetAgent(context.Background(), "agent-hb")
	if !after.LastSeenAt.After(before.LastSeenAt) {
		t.Fatalf("last_seen_at not advanced: before=%v after=%v", before.LastSeenAt, after.LastSeenAt)
	}
}

func TestAgentHeartbeat_RejectsNonAgentToken(t *testing.T) {
	s, _ := newAgentTestServer(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/x/heartbeat", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	s.r.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad-token heartbeat = %d, want 401", rr.Code)
	}
}
```

(If the controller test harness for agents requires Postgres, this test is Postgres-backed like its neighbors and will skip when unavailable — match that pattern.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestAgentHeartbeat`
Expected: FAIL — route/handler missing.

- [ ] **Step 3: Add the handler**

In `internal/controller/api_agent.go`:

```go
// handleAgentHeartbeat handles POST /api/v1/agents/{agentId}/heartbeat.
// Refreshes the agent's last_seen_at so a busy (non-polling) agent is not
// considered dead by the stuck-run reaper / stale-agent cleanup.
func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	if err := s.store.TouchAgent(r.Context(), agentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Register the route**

In `internal/controller/server.go`, inside the `s.r.Route("/api/v1/agents", ...)` block, alongside the other `BearerAuth(s.cfg.AgentToken)` agent routes:

```go
		r.With(BearerAuth(s.cfg.AgentToken)).Post("/{agentId}/heartbeat", s.handleAgentHeartbeat)
```

- [ ] **Step 5: Add the client method**

In `internal/agent/client.go` (match the existing `do`/request helper style — e.g. `FinishRun` at line ~118):

```go
// Heartbeat refreshes the agent's last_seen_at on the controller.
func (c *Client) Heartbeat(ctx context.Context, agentID string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/agents/"+agentID+"/heartbeat", nil, nil)
	return err
}
```

(Confirm `c.do`'s signature/return from the file; adapt if it returns `(status, err)` and treat non-2xx as an error consistent with sibling methods.)

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/controller/ -run TestAgentHeartbeat && go build ./...`
Expected: PASS, clean.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/client.go internal/controller/api_agent.go internal/controller/server.go internal/controller/api_agent_test.go
git commit -m "feat(agent): heartbeat endpoint + client method (touch last_seen_at)"
```

---

### Task 2: Heartbeat goroutine wired into both agents

**Files:**
- Create: `internal/agent/heartbeat.go`
- Create: `internal/agent/heartbeat_test.go`
- Modify: `internal/agent/agent.go` (start it in `Run` after registration)
- Modify: `internal/k8sagent/agent.go` (start it in `Run` after registration)

**Interfaces:**
- Consumes: `Client.Heartbeat` (Task 1).
- Produces: `func StartHeartbeat(ctx context.Context, client *Client, agentID string, interval time.Duration)` — spawns a goroutine that ticks every `interval` and calls `client.Heartbeat`, returning when `ctx` is done. Also a package const `DefaultHeartbeatInterval = 15 * time.Second`.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/heartbeat_test.go`. Use a tiny fake or the real `Client` against an `httptest.Server` counting hits:

```go
package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestStartHeartbeat_TicksUntilCtxDone(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/a1/heartbeat" {
			atomic.AddInt32(&hits, 1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	ctx, cancel := context.WithCancel(context.Background())
	StartHeartbeat(ctx, c, "a1", 20*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	cancel()
	got := atomic.LoadInt32(&hits)
	if got < 3 {
		t.Fatalf("expected several heartbeats, got %d", got)
	}
	// after cancel, hits should stop growing
	time.Sleep(60 * time.Millisecond)
	if atomic.LoadInt32(&hits) != got && atomic.LoadInt32(&hits) < got {
		t.Fatalf("heartbeats continued after cancel")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestStartHeartbeat`
Expected: FAIL — `StartHeartbeat` undefined.

- [ ] **Step 3: Implement**

Create `internal/agent/heartbeat.go`:

```go
package agent

import (
	"context"
	"log/slog"
	"time"
)

// DefaultHeartbeatInterval is how often an agent refreshes its liveness.
const DefaultHeartbeatInterval = 15 * time.Second

// StartHeartbeat spawns a goroutine that periodically calls client.Heartbeat so
// the controller's last_seen_at stays fresh even when all execution slots are
// busy (and claim polling has paused). Best-effort: a failed heartbeat is logged
// and retried on the next tick. Returns immediately; the goroutine exits when ctx
// is done.
func StartHeartbeat(ctx context.Context, client *Client, agentID string, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultHeartbeatInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := client.Heartbeat(ctx, agentID); err != nil && ctx.Err() == nil {
					slog.Warn("agent heartbeat failed", "agentId", agentID, "error", err)
				}
			}
		}
	}()
}
```

- [ ] **Step 4: Wire into the standard agent**

In `internal/agent/agent.go` `Run`, right after `slog.Info("agent registered", ...)` (~line 92):

```go
	StartHeartbeat(ctx, a.Client, a.ID, DefaultHeartbeatInterval)
```

- [ ] **Step 5: Wire into the k8s-agent**

In `internal/k8sagent/agent.go` `Run`, after the agent registers (find the registration call near the top of `Run`, before the `for {` claim loop at ~line 68) add:

```go
	agentlib.StartHeartbeat(ctx, a.client, a.cfg.AgentID, agentlib.DefaultHeartbeatInterval)
```

(`a.client` is `*agentlib.Client`. If the k8s-agent's `Run` does not currently register, add the heartbeat right before the claim `for` loop.)

- [ ] **Step 6: Run tests + build**

Run: `go test ./internal/agent/ -run TestStartHeartbeat && go build ./...`
Expected: PASS, clean.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/heartbeat.go internal/agent/heartbeat_test.go internal/agent/agent.go internal/k8sagent/agent.go
git commit -m "feat(agent): periodic heartbeat goroutine in both agents"
```

---

### Task 3: `ListStuckRunIDs` store method

**Files:**
- Modify: `internal/store/postgres.go`
- Modify: `internal/store/store.go` (interface)
- Test: `internal/store/postgres_runs_test.go` (or a new `postgres_stuckrun_test.go`)

**Interfaces:**
- Produces: `func (p *Postgres) ListStuckRunIDs(ctx context.Context, staleAfter, grace time.Duration) ([]string, error)` and the same signature on the `Store` interface.
- Consumes: existing `runs` (`status`, `claimed_by`, `claimed_at`) and `agents` (`id`, `last_seen_at`).

- [ ] **Step 1: Write the failing test**

Add a Postgres-backed test (match the store test harness — `store.NewTestPostgres(t)`; read a neighbor to see how runs/agents are seeded, and how to set `claimed_by`/`claimed_at`/`last_seen_at` — you may need raw `pool.Exec` to backdate timestamps):

```go
func TestListStuckRunIDs(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	// Seed: an agent with STALE last_seen, a run it claimed long ago (Running).
	// (Use whatever seeding helpers exist; backdate via raw UPDATEs on agents.last_seen_at
	//  and runs.claimed_at/claimed_by/status as needed.)
	// ... seed stuckRunID (stale agent, claimed_at old, Running)
	// ... seed freshRunID  (fresh agent, Running)  -> must NOT be returned
	// ... seed recentRunID (stale agent, claimed_at just now) -> must NOT be returned (grace)
	// ... seed pendingRunID (Pending) -> must NOT be returned

	ids, err := pg.ListStuckRunIDs(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	assert.Contains(t, ids, stuckRunID)
	assert.NotContains(t, ids, freshRunID)
	assert.NotContains(t, ids, recentRunID)
	assert.NotContains(t, ids, pendingRunID)
}

func TestListStuckRunIDs_MissingAgentCountsAsLost(t *testing.T) {
	pg := store.NewTestPostgres(t)
	// A Running run whose claimed_by references an agent row that does not exist
	// (e.g. deleted by DeleteStaleAgents), claimed_at old -> returned.
	// ...
	ids, err := pg.ListStuckRunIDs(context.Background(), 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	assert.Contains(t, ids, orphanRunID)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestListStuckRunIDs`
Expected: FAIL — method undefined.

- [ ] **Step 3: Implement**

In `internal/store/postgres.go`:

```go
// ListStuckRunIDs returns IDs of Running runs whose claiming agent is gone or has
// not sent a heartbeat within staleAfter, excluding runs claimed within the grace
// window (to avoid reaping a just-claimed run before its first heartbeat).
func (p *Postgres) ListStuckRunIDs(ctx context.Context, staleAfter, grace time.Duration) ([]string, error) {
	const q = `
		SELECT r.id
		FROM runs r
		LEFT JOIN agents a ON r.claimed_by = a.id
		WHERE r.status = 'Running'
		  AND r.claimed_at IS NOT NULL
		  AND r.claimed_at < NOW() - make_interval(secs => $2)
		  AND (a.id IS NULL OR a.last_seen_at < NOW() - make_interval(secs => $1))
	`
	rows, err := p.pool.Query(ctx, q, staleAfter.Seconds(), grace.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list stuck runs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
```

Add to the `Store` interface in `internal/store/store.go`:

```go
	ListStuckRunIDs(ctx context.Context, staleAfter, grace time.Duration) ([]string, error)
```

(If there is an in-memory/mock Store implementation in the codebase, add a minimal implementation there too so it still satisfies the interface — grep for other `Store` implementers.)

- [ ] **Step 4: Run tests + build**

Run: `go test ./internal/store/ -run TestListStuckRunIDs && go build ./...`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add internal/store/postgres.go internal/store/store.go internal/store/*_test.go
git commit -m "feat(store): ListStuckRunIDs (Running runs with a dead/stale claiming agent)"
```

---

### Task 4: Stuck-run reaper + wiring

**Files:**
- Create: `internal/controller/stuckrun_reaper.go`
- Create: `internal/controller/stuckrun_reaper_test.go`
- Modify: `cmd/controller/main.go` (start the goroutine)

**Interfaces:**
- Consumes: `store.AcquireAdvisoryLock`, `store.ListStuckRunIDs` (Task 3), `store.MarkRunFinished`.
- Produces: `func RunStuckRunReaper(ctx context.Context, st store.Store, interval, staleAfter, grace time.Duration)`.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/stuckrun_reaper_test.go`. Mirror `approval_reaper_test.go` (read it for the fake-store / leader-follower harness). Assert: as leader, `ListStuckRunIDs` results are each passed to `MarkRunFinished(id, Failed)`; when the advisory lock returns nil (follower), no runs are finished.

```go
func TestStuckRunReaper_FailsStuckRunsAsLeader(t *testing.T) {
	st := &fakeReaperStore{ // implement the few methods used: AcquireAdvisoryLock, ListStuckRunIDs, MarkRunFinished
		lockAcquired: true,
		stuck:        []string{"r1", "r2"},
	}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second)
	assert.ElementsMatch(t, []string{"r1", "r2"}, st.finishedFailed)
}

func TestStuckRunReaper_FollowerDoesNothing(t *testing.T) {
	st := &fakeReaperStore{lockAcquired: false, stuck: []string{"r1"}}
	runStuckRunReaperOnce(context.Background(), st, 90*time.Second, 60*time.Second)
	assert.Empty(t, st.finishedFailed)
}
```

(Extract the per-tick work into `runStuckRunReaperOnce(ctx, st, staleAfter, grace)` so it is unit-testable, exactly as the approval reaper splits `runApprovalReaperAsLeader`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestStuckRunReaper`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

Create `internal/controller/stuckrun_reaper.go`:

```go
package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
)

// stuckRunReaperLockKey is the advisory lock key for the stuck-run reaper.
// Distinct from scheduler(0x65786364), approval(0x61707276), cache(0x63616368),
// logArchiver(0x6C6F6761), appSource(0x61707073).
const stuckRunReaperLockKey = int64(0x7374756B) // 'stuk'

// RunStuckRunReaper periodically fails Running runs whose claiming agent has
// died (no heartbeat within staleAfter, or the agent row is gone), so a run
// never hangs forever on agent loss. Leader-elected via an advisory lock so only
// one replica acts. Fails (never re-queues) — re-running partially-executed steps
// could duplicate side effects. Returns immediately if st is nil.
func RunStuckRunReaper(ctx context.Context, st store.Store, interval, staleAfter, grace time.Duration) {
	if st == nil {
		return
	}
	if interval == 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runStuckRunReaperOnce(ctx, st, staleAfter, grace)
	}
}

func runStuckRunReaperOnce(ctx context.Context, st store.Store, staleAfter, grace time.Duration) {
	release, err := st.AcquireAdvisoryLock(ctx, stuckRunReaperLockKey)
	if err != nil {
		slog.Warn("stuck-run reaper lock", "error", err)
		return
	}
	if release == nil {
		return // follower
	}
	defer release()

	ids, err := st.ListStuckRunIDs(ctx, staleAfter, grace)
	if err != nil {
		slog.Error("stuck-run reaper list error", "error", err)
		return
	}
	for _, id := range ids {
		// MarkRunFinished also releases the run's mutex/semaphore locks.
		if err := st.MarkRunFinished(ctx, id, api.RunFailed); err != nil {
			slog.Error("stuck-run reaper: mark failed", "runId", id, "error", err)
			continue
		}
		slog.Warn("stuck-run reaper: failed orphaned run (agent lost)", "runId", id)
	}
	if len(ids) > 0 {
		slog.Info("stuck-run reaper: failed orphaned runs", "count", len(ids))
	}
}
```

- [ ] **Step 4: Wire into main**

In `cmd/controller/main.go`, next to `go controller.RunApprovalReaper(ctx, pg, time.Minute)` (~line 200):

```go
	go controller.RunStuckRunReaper(ctx, pg, 30*time.Second, 90*time.Second, 60*time.Second)
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/controller/ -run TestStuckRunReaper && go build ./...`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/stuckrun_reaper.go internal/controller/stuckrun_reaper_test.go cmd/controller/main.go
git commit -m "feat(controller): stuck-run reaper fails runs of dead agents (agent lost)"
```

---

### Task 5: k8s orphan-pod GC

**Files:**
- Modify: `internal/k8sagent/podmanager.go` (a pod lister, if not present)
- Create: `internal/k8sagent/podgc.go`
- Create: `internal/k8sagent/podgc_test.go`
- Modify: `internal/k8sagent/agent.go` (start the GC goroutine in `Run`)

**Interfaces:**
- Produces:
  - A pure decision function `func podGCDecision(runStatus api.RunStatus, found bool, pooledInUse bool) bool` (delete iff `!pooledInUse && (!found || isTerminal(runStatus))`).
  - `func (a *K8sAgent) runPodGC(ctx context.Context, interval time.Duration)` — periodic loop listing run pods and deleting orphaned ones.
- Consumes: a pod lister returning `(podName, runID, pooledInUse)` per run pod (label `app=unified-cd-agent`, `unified-cd/runId`, pool annotations), `a.client.GetRun(ctx, runID)`, `a.pm.DeletePod`.

- [ ] **Step 1: Write the failing test**

Create `internal/k8sagent/podgc_test.go`, testing the pure decision + a fake-lister/fake-client loop (mirror the cluster-free style of `orchestrate_test.go`):

```go
func TestPodGCDecision(t *testing.T) {
	cases := []struct {
		status      api.RunStatus
		found       bool
		pooledInUse bool
		wantDelete  bool
	}{
		{api.RunSucceeded, true, false, true},
		{api.RunFailed, true, false, true},
		{api.RunCancelled, true, false, true},
		{api.RunRunning, true, false, false},
		{api.RunRunning, false, false, true},  // run gone -> orphan
		{api.RunSucceeded, true, true, false}, // pooled in-use -> keep
	}
	for _, c := range cases {
		if got := podGCDecision(c.status, c.found, c.pooledInUse); got != c.wantDelete {
			t.Fatalf("podGCDecision(%v,found=%v,pooled=%v)=%v want %v", c.status, c.found, c.pooledInUse, got, c.wantDelete)
		}
	}
}
```

Plus a loop test with a fake lister returning one terminal-run pod and one active-run pod, asserting only the terminal one is deleted. (Extract `runPodGCOnce(ctx, lister, getRun, deletePod)` as the testable seam.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestPodGC`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

Create `internal/k8sagent/podgc.go` with `podGCDecision`, a terminal-status helper, `runPodGCOnce(ctx, lister, getRun, deletePod)`, and `runPodGC(ctx, interval)` that wires the real pod lister (`app=unified-cd-agent` selector as in `pool.go:194`, reading `unified-cd/runId` label + the pool status annotation), `a.client.GetRun`, and `a.pm.DeletePod`. `GetRun` returning a not-found error ⇒ `found=false`. Skip pods whose pool annotation marks them in-use/idle-pooled (reconcile with the constants `annoPoolTemplate`/`annoPoolStatus`/`poolStatusInUse` in podbuilder.go so the GC never removes a healthy pooled pod).

- [ ] **Step 4: Start the GC loop**

In `internal/k8sagent/agent.go` `Run`, alongside the heartbeat start (Task 2 Step 5):

```go
	go a.runPodGC(ctx, time.Minute)
```

- [ ] **Step 5: Run tests + build**

Run: `go test ./internal/k8sagent/ -run TestPodGC && go build ./... && go vet -tags k8s ./internal/k8sagent/`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/podgc.go internal/k8sagent/podgc_test.go internal/k8sagent/podmanager.go internal/k8sagent/agent.go
git commit -m "feat(k8sagent): GC orphaned ucd-run pods for terminal/absent runs"
```

---

### Task 6: Docs + TODO update

**Files:**
- Modify: `docs/high-availability.md`
- Modify: `TODO.md`

- [ ] **Step 1: Update docs**

In `docs/high-availability.md`, add a section documenting: agents heartbeat every `~15s` (independent of claim polling, so a saturated agent stays live and isn't falsely stale-deleted); the leader-elected stuck-run reaper fails `Running` runs whose agent's heartbeat is stale (or gone) within `~staleAfter`, releasing their mutex/semaphore locks; the reaper Fails (does not re-queue) and why; k8s orphaned pods are GC'd by the k8s-agent. State the default thresholds.

- [ ] **Step 2: Update TODO.md**

Mark the "可用性 / フェイルオーバー" item A as implemented (reference this plan + the reaper/heartbeat/GC), leaving a one-line note that re-queue was intentionally not chosen.

- [ ] **Step 3: Commit**

```bash
git add docs/high-availability.md TODO.md
git commit -m "docs: document agent heartbeat + stuck-run reaper + k8s pod GC"
```

---

## Self-Review

**Spec coverage:** heartbeat endpoint+client → Task 1; heartbeat goroutine (both agents) → Task 2; `ListStuckRunIDs` → Task 3; reaper + wiring → Task 4; k8s pod GC → Task 5; docs+TODO → Task 6. All spec sections covered.

**Placeholder scan:** No TBD/TODO. Task 3/Task 5 test steps describe seeding/lister setup rather than pasting full DB-backdating SQL because the exact seeding helpers are package-local — the implementer is told to read the neighbor harness; the assertions and method signatures are fully specified. All new production code is given in full.

**Type consistency:** `Client.Heartbeat(ctx, agentID) error` (Task 1) consumed by `StartHeartbeat` (Task 2). `ListStuckRunIDs(ctx, staleAfter, grace) ([]string, error)` (Task 3) consumed by `runStuckRunReaperOnce` (Task 4). `MarkRunFinished(id, api.RunFailed)` used per-run in Task 4 (matches the constraint). `stuckRunReaperLockKey = 0x7374756B` is distinct from all listed keys. Thresholds (heartbeat 15s / interval 30s / staleAfter 90s / grace 60s) are consistent between the reaper wiring (Task 4) and the constraints. `podGCDecision` signature consistent between Task 5's test and implementation.
