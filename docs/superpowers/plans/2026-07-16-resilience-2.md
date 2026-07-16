# Agent Resilience Wave 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Panic in a step fails only its run (not the process); a lost claim is reconciled via heartbeat-carried active-run IDs; host disk exhaustion is prevented (preflight) and mitigable (opt-in age-based workspace GC); dropped log lines surface a marker and the write path no longer stalls 60s under partition.

**Architecture:** `internal/agent` (runOne recover, active-run set, disk preflight/GC, LogPusher), `internal/k8sagent` (outer recover, active-run set), `internal/agent/heartbeat.go` + `client.go` + `internal/controller/api_agent.go` (heartbeat body + reconcile), `internal/config/agent.go` + `cmd/agent/main.go` (config).

**Tech Stack:** Go, `golang.org/x/sys` (disk-free; confirm vendored, else build-tagged syscalls), existing agent/controller test harnesses.

## Global Constraints

- **Backward compatible heartbeat:** a heartbeat request with NO body (old agent) triggers NO reconcile. Only a body carrying `activeRunIDs` enables it.
- **Reconcile grace:** the controller fails a Running-assigned run absent from the reported active set ONLY if its `claimed_at` is older than a grace window (default 60s) — the claim→first-heartbeat race must never fail a healthy run.
- **GC safety:** `workspaceRetentionDays` default 0 (disabled). GC never touches `wsBase` itself, dot-prefixed siblings (`.ucd-tools`), or a dir for a currently-active run (cross-check the active set). Age = dir mtime.
- **Preflight is host-only** (k8s workspaces are pod volumes). `minFreeDisk` unset/0 = disabled.
- **Every recover logs at Error with `debug.Stack()`** — never silently swallow.
- All new config: yaml + flag + env, following the existing `AgentConfig` plumbing.
- Full suite before push: `go test ./... -count=1` (known unrelated `internal/cli` flake — isolate-rerun). `go generate ./...` drift-free. No `-race` (CGO disabled).

---

### Task 1: Panic recovery — `runOne` + outer goroutine guards

**Files:**
- Modify: `internal/agent/pipeline.go` (`runOne`)
- Modify: `internal/agent/agent.go` (host slot goroutine / executeRun guard), `internal/k8sagent/agent.go` (dispatch goroutine guard)
- Test: `internal/agent/pipeline_test.go` (or new `panic_test.go`), a k8s dispatch guard test

**Interfaces:**
- `runOne` converts a panic in `run(ctx, step)` into an `error` (respecting `ContinueOnError` the same as a normal error).

- [ ] **Step 1: Write the failing test**

Add to `internal/agent/pipeline_test.go` (match its package + how it builds a `run` func):

```go
func TestRunOne_RecoversPanic(t *testing.T) {
	panicRun := func(ctx context.Context, s api.ClaimStep) error { panic("boom in step") }
	err := runOne(context.Background(), api.ClaimStep{Index: 0, Name: "s"}, panicRun)
	if err == nil || !strings.Contains(err.Error(), "panic") || !strings.Contains(err.Error(), "boom in step") {
		t.Fatalf("panic must become an error naming the panic value, got %v", err)
	}
}

func TestRunOne_PanicRespectsContinueOnError(t *testing.T) {
	panicRun := func(ctx context.Context, s api.ClaimStep) error { panic("boom") }
	err := runOne(context.Background(), api.ClaimStep{Index: 0, Name: "s", ContinueOnError: true}, panicRun)
	if err != nil {
		t.Fatalf("a recovered panic on a continueOnError step must be swallowed like a normal error, got %v", err)
	}
}

func TestRunParallel_OnePanicFailsRunNotProcess(t *testing.T) {
	var okRan atomic.Bool
	run := func(ctx context.Context, s api.ClaimStep) error {
		if s.Name == "boom" {
			panic("kaboom")
		}
		okRan.Store(true)
		return nil
	}
	steps := []api.ClaimStep{{Index: 0, Name: "boom"}, {Index: 1, Name: "ok"}}
	err := runParallel(context.Background(), steps, run)
	if err == nil {
		t.Fatal("a panicking parallel step must fail the group")
	}
	if !okRan.Load() {
		t.Fatal("the sibling parallel step must still have run")
	}
}
```

(Confirm `runParallel`'s signature — `pipeline.go:141` — and that a panicking goroutine now returns rather than aborting `wg.Wait()`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'RunOne_Recovers|RunOne_PanicRespects|RunParallel_OnePanic' -count=1`
Expected: FAIL / the process aborts (panic propagates).

- [ ] **Step 3: Implement `runOne`**

Rewrite `runOne` (pipeline.go:172):

```go
func runOne(ctx context.Context, step api.ClaimStep, run func(context.Context, api.ClaimStep) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := debug.Stack()
			slog.Error("step panicked", "step", step.Name, "index", step.Index, "panic", r)
			perr := fmt.Errorf("step %q panicked: %v\n%s", step.Name, r, stack)
			if step.ContinueOnError {
				err = nil
			} else {
				err = perr
			}
		}
	}()
	e := run(ctx, step)
	if e != nil && step.ContinueOnError {
		return nil
	}
	return e
}
```

Add imports `runtime/debug`, `fmt`, `log/slog` if missing. Because `runParallel` (pipeline.go:148) calls `runOne` inside each goroutine, this recover alone stops a parallel-step panic from aborting the group — no change needed there.

Note on the panic message reaching the run log: the panic error propagates as the step's failure, and the orchestrator's step-failure path already ships the error text to the step's stderr log (`orchestrator.go` step-report). Confirm the panic message appears in the reported step failure; if the orchestrator only logs `err` at agent level, add a System log line at the recover site is unnecessary — the step failure carries it.

- [ ] **Step 4: Implement outer guards (defense in depth)**

Host `internal/agent/agent.go`: in the slot goroutine that calls `executeRun` (the `go func(slot int){ ... a.runLoop(...) }` body), and/or inside `executeRun`, wrap with a recover that, on panic, calls `a.failRun(runCtx, resp.RunID, fmt.Sprintf("agent panic: %v", r))` (host `failRun` exists) so a panic ABOVE the step level still marks the run Failed instead of leaving it Running. Prefer wrapping `executeRun`'s body (it has the runID).

k8s `internal/k8sagent/agent.go`: in the dispatch goroutine (`go func(c api.ClaimResponse){ ...; a.dispatch(runCtx, c) }`), wrap `a.dispatch` with a recover → `a.failRun(runCtx, c.RunID, fmt.Sprintf("agent panic: %v", r))`.

Both recovers log Error with `debug.Stack()`.

- [ ] **Step 5: Test the outer guard (k8s dispatch)**

Reuse the k8s `dispatch` seam from PR #51's drain tests: set `a.dispatch` to a stub that panics; drive one claim; assert the run is reported Failed (fake controller) and `Run` does not crash. (If the drain test harness `newK8sAgentForTest`/`claimController` exists, model on it.)

- [ ] **Step 6: Run + commit**

Run: `go test ./internal/agent/ ./internal/k8sagent/ -count=1`
Expected: PASS.

```bash
git add internal/agent/ internal/k8sagent/
git commit -m "feat(agent): recover step panics into run failures; outer goroutine guards (audit item 4)"
```

---

### Task 2: LogPusher drop marker + bounded write-flush context

**Files:**
- Modify: `internal/agent/runner.go` (`LogPusher`)
- Test: `internal/agent/runner_test.go` (or the file where LogPusher is tested)

- [ ] **Step 1: Write the failing test**

Model on the existing LogPusher tests (find them: `grep -rln LogPusher internal/agent/*_test.go`). Add:

```go
// After N dropped batches, the next successful flush emits a single marker line
// with the dropped count.
func TestLogPusher_DropMarker(t *testing.T) {
	// Use a fake client that fails the first K flushes (forcing drops), then
	// succeeds; assert a "[... dropped ...]" marker with the count is sent on
	// the first success. Reuse whatever fake-client seam the existing LogPusher
	// tests use (an AppendLogBulk recorder). Shorten maxPendingBytes via the
	// struct if the tests already do, or push enough lines to exceed 1MiB.
}
```

Write it concretely against the existing harness (inspect `runner_test.go` for the fake client + how `LogPusher` is constructed and driven; the assertion is: the recorded AppendLogBulk requests, after recovery, include exactly one line matching `dropped` with the right N).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'LogPusher_DropMarker' -count=1`
Expected: FAIL (no marker today).

- [ ] **Step 3: Implement**

In `runner.go`:
- Add a `droppedLines int` field to the `LogPusher` struct (guarded by the existing `p.mu`).
- In `appendPendingLocked` (runner.go ~355, the drop-oldest loop), count discarded lines: before `p.pending = p.pending[1:]`, add `p.droppedLines += len(p.pending[0])`-worth (count LINES in the dropped batch, not bytes — inspect the pending element type; if a batch is `[]api.LogAppendRequest`, add its len).
- In `flushLocked`, after a SUCCESSFUL `AppendLogBulk` that cleared `pending`, if `p.droppedLines > 0`, send one more `AppendLogBulk` (or prepend to the next) with a synthetic line: `fmt.Sprintf("[%d log line(s) dropped: controller unreachable]", p.droppedLines)` at the pusher's stepIndex, stream `stderr`, then reset `p.droppedLines = 0`. Route it through the same masker the pusher already uses.
- **Write-stall:** where `Write` calls `flushLocked(context.Background())` synchronously (runner.go ~278), change to a bounded context: `fctx, cancel := context.WithTimeout(context.Background(), logPusherWriteFlushTimeout); defer cancel()` with `logPusherWriteFlushTimeout = 5 * time.Second` (new package var, overridable in tests). This caps how long a partitioned write holds `p.mu`; the 2s auto-flush ticker remains the steady drain.

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/agent/ -run 'LogPusher' -count=1` then `go test ./internal/agent/ -count=1`
Expected: PASS.

```bash
git add internal/agent/runner.go internal/agent/runner_test.go
git commit -m "feat(agent): LogPusher surfaces a dropped-lines marker; bound write-path flush (F9)"
```

---

### Task 3: Active-run set (both agents) + heartbeat body

**Files:**
- Modify: `internal/agent/agent.go` + `internal/k8sagent/agent.go` (active-run set enroll/retire)
- Modify: `internal/agent/heartbeat.go` (send active IDs), `internal/agent/client.go` (`Heartbeat` body)
- Modify: `internal/api/types.go` (heartbeat request type)
- Test: `internal/agent/heartbeat_test.go` (or new), agent active-set unit tests

**Interfaces:**
- Produces: an `activeRuns` set with `add(id)`/`remove(id)`/`snapshot() []string` on both agents; `api.HeartbeatRequest{ ActiveRunIDs []string }`; `Client.Heartbeat(ctx, agentID, activeRunIDs []string)` (body when non-nil); `StartHeartbeat` takes a `func() []string` provider.

- [ ] **Step 1: Write the failing tests**

- Agent active-set unit test: add/remove/snapshot is race-safe and returns the current set (small, in the agent package).
- `client.Heartbeat` with a non-empty slice POSTs a body containing the IDs (httptest recorder asserts the decoded `HeartbeatRequest.ActiveRunIDs`); with `nil` posts no body (or an empty body — decide: send body only when the provider returns non-nil, to stay bodyless for the legacy path — assert the bodyless case sends Content-Length 0 / nil body).

Write these against the existing client test harness (`grep -rln 'func.*Heartbeat' internal/agent/*_test.go`).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'Heartbeat|ActiveRuns' -count=1`
Expected: FAIL (signature/type absent).

- [ ] **Step 3: Implement**

- `internal/api/types.go`: `type HeartbeatRequest struct { ActiveRunIDs []string \`json:"activeRunIds,omitempty"\` }`.
- Active set: a small type (in `internal/agent`, reused by k8s via import or duplicated — prefer a shared `agentlib` helper) `type runSet struct { mu sync.Mutex; m map[string]struct{} }` with `Add`/`Remove`/`Snapshot`.
- Host `agent.go`: construct a `runSet`; in `runLoop`, `Add(resp.RunID)` before `executeRun`, `Remove` after (defer). Pass a snapshot provider to `StartHeartbeat`.
- k8s `agent.go`: same, in the dispatch goroutine.
- `heartbeat.go`: `StartHeartbeat(ctx, client, agentID, interval, activeProvider func() []string)`; each beat calls `client.Heartbeat(hbCtx, agentID, activeProvider())`. All `StartHeartbeat` callers updated (grep — host + k8s).
- `client.go`: `Heartbeat(ctx, agentID string, activeRunIDs []string) error` — if `activeRunIDs == nil` send `nil` body (bodyless, legacy); else marshal `api.HeartbeatRequest{ActiveRunIDs: activeRunIDs}`. Note: a live agent always has a (possibly empty) set — send an EMPTY-slice body (not nil) so the controller can distinguish "new agent, zero active runs" (reconcile: fail all its Running runs older than grace) from "old agent, no body" (skip). Decide: provider returns `[]string{}` (non-nil) for a live new agent → always sends a body. Only pre-this-change binaries send no body. Encode `HeartbeatRequest` even for an empty slice.

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/agent/ ./internal/k8sagent/ -count=1`
Expected: PASS.

```bash
git add internal/agent/ internal/k8sagent/ internal/api/types.go
git commit -m "feat(agent): track active run IDs; carry them in the heartbeat body (audit item 6 agent side)"
```

---

### Task 4: Controller heartbeat reconcile + store method

**Files:**
- Modify: `internal/controller/api_agent.go` (`handleAgentHeartbeat`)
- Modify: `internal/store/store.go` + `internal/store/postgres.go` (new `ListReconcilableRunIDsByAgent`)
- Test: `internal/controller/api_agent_test.go` (or wherever heartbeat/reconcile is tested)

**Interfaces:**
- Produces: `store.ListReconcilableRunIDsByAgent(ctx, agentID string, grace time.Duration) ([]string, error)` — Running runs assigned to `agentID` with `claimed_at` older than `grace`.

- [ ] **Step 1: Write the failing test**

Model on `TestResolveGitPendingRuns_*` / existing reconcile tests using `store.NewTestPostgres`. Seed: agent A with two Running runs r1 (claimed_at now-5m) and r2 (claimed_at now-2s); POST a heartbeat with body `activeRunIDs:[r1]`. Assert: r2 is NOT failed (within grace) — wait, r2 is young so within grace → NOT failed; r1 is reported active → NOT failed. Then seed r3 (claimed_at now-5m, NOT in the reported set) → after the heartbeat, r3 IS Failed. And a bodyless heartbeat → nothing failed. Construct the three cases.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/controller/ -run 'Heartbeat.*econcile|econcile.*Heartbeat' -count=1`
Expected: FAIL (handler ignores body).

- [ ] **Step 3: Implement**

- Store: `ListReconcilableRunIDsByAgent(ctx, agentID, grace)` — `SELECT id FROM runs WHERE agent_id = $1 AND status = 'Running' AND claimed_at < now() - $2` (adapt to the schema's claimed_at column + Running enum; mirror `ListRunningRunIDsByAgent`'s query, add the claimed_at predicate). Add to the `store.Store` interface + the postgres impl + any fake/mock store used in controller tests.
- `handleAgentHeartbeat`: after `TouchAgent`, decode the optional body:

```go
	if r.ContentLength != 0 {
		var req api.HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			reported := map[string]struct{}{}
			for _, id := range req.ActiveRunIDs {
				reported[id] = struct{}{}
			}
			ids, err := s.store.ListReconcilableRunIDsByAgent(r.Context(), agentID, heartbeatReconcileGrace)
			if err == nil {
				for _, id := range ids {
					if _, ok := reported[id]; ok {
						continue
					}
					if ferr := failOrphanedRun(r.Context(), s.store, id); ferr != nil {
						slog.Warn("heartbeat reconcile: fail orphaned run", "runID", id, "error", ferr)
					}
				}
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
```

`heartbeatReconcileGrace` = a package const (e.g. 60s). A body decode error or absent body → no reconcile (backward compatible). Add `encoding/json`/`api`/`slog` imports if needed.

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/controller/ -count=1`
Expected: PASS.

```bash
git add internal/controller/ internal/store/
git commit -m "feat(controller): heartbeat reconcile fails Running runs absent from the agent's active set past grace (audit item 6)"
```

---

### Task 5: Disk preflight (host agent)

**Files:**
- Create: `internal/agent/diskfree.go` (+ `diskfree_windows.go` / `diskfree_unix.go` if `x/sys` isn't vendored — check first)
- Modify: `internal/agent/agent.go` (claim gate), `internal/config/agent.go` + `cmd/agent/main.go` (config)
- Test: `internal/agent/diskfree_test.go`, claim-gate test with an injected free-space fn

**Interfaces:**
- Produces: `func freeBytes(path string) (uint64, error)`; `AgentConfig.MinFreeDisk` (bytes; yaml `minFreeDisk`, flag `--min-free-disk`, env); a claim-loop gate calling an injectable `a.freeBytesFn`.

- [ ] **Step 1: Write the failing test**

```go
func TestClaimLoop_SkipsBelowMinFreeDisk(t *testing.T) {
	// Construct an Agent with MinFreeDisk=1GiB and a.freeBytesFn returning 100MiB;
	// assert the claim loop does NOT call Claim (or backs off) while below, and
	// DOES once freeBytesFn returns above. Use the dispatch/claim seam; if the
	// host claim loop is hard to unit-drive, test the gate predicate function
	// directly: shouldClaim(free, min) and assert the loop consults it.
}
```

Prefer extracting a tiny predicate `func belowMinFreeDisk(free, min uint64) bool` and unit-testing that + asserting the loop calls it. Keep the disk syscall itself behind `a.freeBytesFn` (default = `freeBytes`) so tests inject.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'MinFreeDisk' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `diskfree.go`: `freeBytes(path)` — if `golang.org/x/sys` is vendored (check `go list -m golang.org/x/sys` / `grep -r x/sys/unix vendor/`), use `unix.Statfs`/`windows.GetDiskFreeSpaceEx`; else two build-tagged files. Return available bytes for `path`'s filesystem.
- `AgentConfig.MinFreeDisk uint64` (bytes; accept a human suffix like `10GiB` if the config already parses such — check other size fields; else plain bytes). Plumb flag/env in `cmd/agent/main.go`.
- Host `agent.go` claim loop: before `a.Client.Claim(...)`, if `a.cfg.MinFreeDisk > 0`, `free, err := a.freeBytesFn(wsBase)`; on `err == nil && free < MinFreeDisk` → log a warning and `continue` after a short sleep (mirror the claim-error backoff path), skipping the claim this tick. Add `freeBytesFn func(string)(uint64,error)` field defaulted to `freeBytes` in the Agent constructor.

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/agent/ ./internal/config/ -count=1 && go build ./cmd/agent/`
Expected: PASS.

```bash
git add internal/agent/ internal/config/agent.go cmd/agent/main.go
git commit -m "feat(agent): min-free-disk preflight before claiming (audit item 7)"
```

---

### Task 6: Opt-in age-based workspace GC (host agent)

**Files:**
- Modify: `internal/agent/workspace.go` (or a new `workspace_gc.go`), `internal/agent/agent.go` (periodic sweep wiring), `internal/config/agent.go` + `cmd/agent/main.go`
- Test: `internal/agent/workspace_gc_test.go`

**Interfaces:**
- Produces: `AgentConfig.WorkspaceRetentionDays int` (yaml/flag/env; default 0 = disabled); `func gcWorkspaces(wsBase string, retention time.Duration, active map[string]struct{}, now time.Time) (removed []string, err error)`.

- [ ] **Step 1: Write the failing test**

```go
func TestGCWorkspaces(t *testing.T) {
	base := t.TempDir()
	mk := func(rel string, age time.Duration) string {
		p := filepath.Join(base, rel)
		os.MkdirAll(p, 0o755)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
		return p
	}
	old := mk("working0/oldjob", 10*24*time.Hour)
	fresh := mk("working0/freshjob", 1*time.Hour)
	activeDir := mk("working1/activejob", 30*24*time.Hour)
	tools := mk(".ucd-tools", 30*24*time.Hour)

	active := map[string]struct{}{} // by dir path or job name — match the impl's key
	// mark activeDir's job as active:
	// (impl decides key; the test must construct `active` the same way gcWorkspaces reads it)
	removed, err := gcWorkspaces(base, 7*24*time.Hour, active /*must protect activeDir*/, time.Now())
	require.NoError(t, err)
	require.DirExists(t, fresh, "fresh dir kept")
	require.DirExists(t, tools, "dot-prefixed shim dir never touched")
	require.NoDirExists(t, old, "aged dir removed")
	_ = activeDir; _ = removed
	// Assert an active-run dir is protected (construct `active` to include it and assert DirExists).
}
```

The implementer decides the active-set key (job name vs full dir path) and MUST make the test construct `active` consistently — write the test after choosing the key. Include an explicit active-protection assertion.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/agent/ -run 'GCWorkspaces' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

- `gcWorkspaces(wsBase, retention, active, now)`: walk `wsBase/working*/` (depth 2: `working<slot>/<job>`). For each `<job>` dir: skip if the segment is dot-prefixed; skip if its job (key) is in `active`; if `now.Sub(mtime) > retention` → `os.RemoveAll`, collect. NEVER remove `wsBase`, `working<slot>` themselves, or any dot-prefixed entry. Use the dir mtime (`os.Stat`).
- Wire: `AgentConfig.WorkspaceRetentionDays` (flag/env). In `agent.go`, when `> 0`, run `gcWorkspaces` at startup (after ReconcileRuns) and on a periodic ticker (mirror `runPodGC`'s cadence), passing the current active-set snapshot (from Task 3) as `active`. Log removed dirs.

- [ ] **Step 4: Run + commit**

Run: `go test ./internal/agent/ ./internal/config/ -count=1`
Expected: PASS.

```bash
git add internal/agent/ internal/config/agent.go cmd/agent/main.go
git commit -m "feat(agent): opt-in age-based workspace GC (audit item 7)"
```

---

### Task 7: Docs + full sweep

**Files:**
- Modify: `docs/configuration.md` (new agent knobs: `minFreeDisk`, `workspaceRetentionDays` + env/flags), `docs/operations.md` (disk hygiene: preflight + GC operator guidance; the new heartbeat-reconcile behavior), `docs/troubleshooting.md` (dropped-log marker meaning; a run failed by heartbeat-reconcile after a lost claim).

- [ ] **Step 1: Write the docs** covering: panic-in-step now fails just that run (with the panic in the step log); `minFreeDisk` preflight (agent stops claiming below threshold — an operational lever, not an error); `workspaceRetentionDays` opt-in GC (default off; what it deletes and what it protects); the `[N log line(s) dropped: controller unreachable]` marker; heartbeat-carried active-run reconcile (a lost claim is now failed within ~grace instead of hanging Running).

- [ ] **Step 2: Full sweep**

`go build ./... && go generate ./...` (no drift), `go vet ./internal/... ./cmd/...`, full `go test ./... -count=1` (known internal/cli flake — isolate-rerun 3x if hit).

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs: min-free-disk, workspace GC, heartbeat reconcile, log-drop marker"
```

---

## Notes for the executor
- Order 1,2 (independent), 3→4 (agent active set feeds controller reconcile), 5,6 (config-heavy; 6 consumes Task 3's active set), 7.
- **Verify every guessed signature** (`runOne`, `runParallel`, `StartHeartbeat` callers, `LogPusher` internals, `ListRunningRunIDsByAgent`, `AgentConfig` plumbing, `NewK8sAgent`/host `Agent` constructors for the injectable fn fields) before writing tests — adjust to reality, keep assertions.
- The heartbeat body decision (empty-slice body for a live new agent vs nil for legacy) is load-bearing for backward compat — implement exactly as Task 3 Step 3 states and assert both cases.
- Reuse PR #51's k8s drain test seam (`dispatch` field, `newK8sAgentForTest`, `claimController`) for the k8s outer-guard and active-set tests if present.
- Full-suite gate before finishing (merge discipline).
