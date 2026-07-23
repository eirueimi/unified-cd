# PR F — `call:`-step child-run auth for enrolled agents — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an enrolled (`uca_`) agent execute a `call:` step end-to-end — create the child run and poll it — using its own agent credential, authorized by parent-run ownership, with no human PAT.

**Architecture:** Add a dedicated agent-authenticated endpoint `POST /api/v1/agents/{agentId}/runs/{runId}/children`, gated by `s.agentAuth` + `requireAgentPathIdentity` + `agentRunGuard` (the agent may spawn a child only for a run it actually claimed). Extract the job→run creation logic shared with the human trigger route into a helper. Give agents read access to the two run GETs the poll uses (`/runs/{id}`, `/runs/{id}/outputs`) via the existing `agentOrServerAuth` pattern (mirrors the artifact routes). Repoint the agent client at the new create endpoint. Finally, remove the test-only PAT double-registration that masked the bug.

**Tech Stack:** Go 1.26.2, chi v5, Postgres (pgx), cobra. Design source: `.superpowers/sdd/callstep-auth-investigation.md` (Option 1, recommended).

## Global Constraints

- Module `github.com/eirueimi/unified-cd`. No cgo/gcc → run tests WITHOUT `-race`.
- Repo root is the working dir; prefix Bash with `cd /c/Users/arimax/unified-cd-project/unified-cd && …`.
- Commit trailer on every commit: `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`.
- Authorization basis for an agent creating a child run is the parent run's existing, immutable `claimed_by` ownership (via `agentRunGuard`) — never a capability, role, or self-declared field. Labels/capabilities are untouched here.
- Controller unit tests are Postgres-backed; a transient `[setup failed]` on the first run is a known flake — rerun once.
- This is a clean cutover: after it lands, `POST /api/v1/runs` stays human-only (ServerAuth + developer). Do not add an agent branch there.

---

### Task 1: Controller — agent-authenticated child-run endpoint (parent-owned)

**Files:**
- Modify: `internal/controller/api_runs.go` (extract `createRunFromJob` helper from `handleTriggerRun`)
- Modify: `internal/controller/api_agent.go` (add `handleAgentCreateChildRun`)
- Modify: `internal/controller/server.go:234-252` (register the route in `agentRouteIdentityMatrix`)
- Test: `internal/controller/api_agent_test.go` (new `TestAgentAPI_CreateChildRun_*`)

**Interfaces:**
- Consumes: `s.agentRunGuard(ctx, agentID, runID, rejectTerminal) (runWriteVerdict, error)` (`agent_guard.go:98`); `respondRunWriteVerdict(w, v, runID) bool` (`agent_guard.go:137`); `api.TriggerRunRequest{JobName, Params}`; test helpers `newTestServer`, `claimRunForTest`, `issueAgentAccessForTest` (all in `api_agent_test.go`).
- Produces: `func (s *Server) createRunFromJob(ctx context.Context, jobName string, reqParams map[string]string, triggeredBy string) (*api.Run, int, string)` — returns `(run, 0, "")` on success, or `(nil, httpStatus, message)` on failure. `func (s *Server) handleAgentCreateChildRun(w http.ResponseWriter, r *http.Request)`. Route `POST /api/v1/agents/{agentId}/runs/{runId}/children`.

- [ ] **Step 1: Write the failing test (RED)**

Add to `internal/controller/api_agent_test.go`:

```go
// TestAgentAPI_CreateChildRun_OwnedParent verifies an enrolled agent can create
// a child run for a run it has claimed (the call: step path), authorized purely
// by parent-run ownership.
func TestAgentAPI_CreateChildRun_OwnedParent(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "parent-job", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(t.Context(), "child-job", "unified-cd/v1", []byte(`{"native":true}`))
	parent, _ := pg.CreateRun(t.Context(), "parent-job", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a1", parent.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "child-job"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+parent.ID+"/children", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var child api.Run
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &child))
	assert.NotEmpty(t, child.ID)
	assert.Equal(t, "child-job", child.JobName)
	assert.NotEqual(t, parent.ID, child.ID)
}

// TestAgentAPI_CreateChildRun_NotOwnedParent verifies an agent cannot spawn a
// child for a run claimed by a DIFFERENT agent (403, no run created).
func TestAgentAPI_CreateChildRun_NotOwnedParent(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(t.Context(), "child-job", "unified-cd/v1", []byte(`{"native":true}`))
	parent, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	claimRunForTest(t, pg, "a2", parent.ID) // owned by a2, not a1
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	body, _ := json.Marshal(api.TriggerRunRequest{JobName: "child-job"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+parent.ID+"/children", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}
```

- [ ] **Step 2: Run it — expect FAIL (RED)**

Run: `go test ./internal/controller/ -run 'TestAgentAPI_CreateChildRun' -count=1 -v`
Expected: FAIL — the route does not exist yet, so the router returns 404/405 (not 200/403). (Postgres; rerun once on `[setup failed]`.)

- [ ] **Step 3: Extract `createRunFromJob` from `handleTriggerRun` (refactor, no behavior change)**

In `internal/controller/api_runs.go`, replace the body of `handleTriggerRun` from the `GetJob` call through the `CreateRun` call with a call to a new helper. The current `handleTriggerRun` (after the `req` decode + `JobName == ""` check at lines 22-30) does GetJob→unmarshal spec→resolveParams→ExpandAgentSelector→RequiredCaps→CreateRun. Extract exactly that into:

```go
// createRunFromJob resolves a job's spec, validates params, derives routing
// (agentSelector + required capability), and creates the run. Shared by the
// human trigger route and the agent child-run route. Returns (run, 0, "") on
// success or (nil, httpStatus, message) describing the failure.
func (s *Server) createRunFromJob(ctx context.Context, jobName string, reqParams map[string]string, triggeredBy string) (*api.Run, int, string) {
	job, err := s.store.GetJob(ctx, jobName)
	if err != nil {
		return nil, http.StatusNotFound, "job not found: " + jobName
	}
	var spec dsl.Spec
	agentSelector := []string{}
	if err := json.Unmarshal(job.Spec, &spec); err == nil {
		agentSelector = spec.AgentSelector
	}
	params, err := resolveParams(spec.Params.Inputs, reqParams)
	if err != nil {
		return nil, http.StatusBadRequest, err.Error()
	}
	agentSelector, err = dsl.ExpandAgentSelector(agentSelector, params)
	if err != nil {
		return nil, http.StatusBadRequest, "agentSelector: " + err.Error()
	}
	requiredCaps := dsl.RequiredCaps(spec)
	run, err := s.store.CreateRun(ctx, job.Name, params, job.Spec, agentSelector, requiredCaps, triggeredBy)
	if err != nil {
		return nil, http.StatusInternalServerError, "create run: " + err.Error()
	}
	return run, 0, ""
}
```

Then rewrite the tail of `handleTriggerRun` (keep the decode + `JobName == ""` guards unchanged) to:

```go
	triggeredBy := "api"
	if p, ok := principalFromContext(r.Context()); ok && p.Name != "" {
		triggeredBy = p.Name
	}
	run, status, msg := s.createRunFromJob(r.Context(), req.JobName, req.Params, triggeredBy)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
```

- [ ] **Step 4: Add `handleAgentCreateChildRun`**

In `internal/controller/api_agent.go` (it already imports `chi`, `json`, `net/http`, `api`), add:

```go
// handleAgentCreateChildRun lets an agent executing a call: step create the
// child run. Authorized purely by parent-run ownership: the agent may spawn a
// child only for a run it currently holds (claimed_by == agentID, non-terminal).
// This is the agent-authenticated counterpart to the human-only
// POST /api/v1/runs — an enrolled uca_ credential is never accepted there.
func (s *Server) handleAgentCreateChildRun(w http.ResponseWriter, r *http.Request) {
	agentID := chi.URLParam(r, "agentId")
	parentRunID := chi.URLParam(r, "runId")
	v, gerr := s.agentRunGuard(r.Context(), agentID, parentRunID, true)
	if gerr != nil {
		http.Error(w, gerr.Error(), http.StatusInternalServerError)
		return
	}
	if respondRunWriteVerdict(w, v, parentRunID) {
		return
	}
	var req api.TriggerRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.JobName == "" {
		http.Error(w, "jobName is required", http.StatusBadRequest)
		return
	}
	run, status, msg := s.createRunFromJob(r.Context(), req.JobName, req.Params, "agent:"+agentID)
	if status != 0 {
		http.Error(w, msg, status)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
```

- [ ] **Step 5: Register the route**

In `internal/controller/server.go`, add to `agentRouteIdentityMatrix` (after the `.../runs/{runId}/finish` entry, ~line 243), keeping `bindPath: true` so it gets `s.agentAuth` + `requireAgentPathIdentity`:

```go
	{method: http.MethodPost, path: "/api/v1/agents/{agentId}/runs/{runId}/children", bindPath: true, handler: (*Server).handleAgentCreateChildRun},
```

- [ ] **Step 6: Run it — expect PASS (GREEN)**

Run: `go test ./internal/controller/ -run 'TestAgentAPI_CreateChildRun|TestHandleTriggerRun|TriggerRun' -count=1 -v`
Expected: PASS — both new tests pass AND the pre-existing trigger-run tests still pass (the refactor is behavior-preserving). If any trigger test asserts an exact error string, keep the messages identical to the originals above.

- [ ] **Step 7: Commit** (`feat(controller): agent-authenticated child-run endpoint scoped by parent ownership`).

---

### Task 2: Controller — agent read access to run + outputs GETs

**Files:**
- Modify: `internal/controller/agent_auth.go` (add `viewerOrAgent` middleware)
- Modify: `internal/controller/server.go:349,355` (move the two GETs out of the ServerAuth group)
- Test: `internal/controller/api_agent_test.go` (new `TestAgentAPI_ReadRunAndOutputs`)

**Interfaces:**
- Consumes: `s.agentOrServerAuth` (`agent_auth.go:124`), `requireMinRole` (`rbac.go:97`), `agentPrincipalFromContext`.
- Produces: `func (s *Server) viewerOrAgent(next http.Handler) http.Handler`. Routes `GET /api/v1/runs/{id}` and `GET /api/v1/runs/{id}/outputs` now accept an agent `uca_` credential in addition to human viewer+.

Rationale: the poll (`GetRun`) and outputs fetch (`GetRunOutputs`) hit these two GETs, which are ServerAuth-only today. Enrolled agents are already trusted to read any run's ARTIFACTS via the same `agentOrServerAuth` pattern (`server.go:475-478`); extending that trust to run status/outputs is strictly not-larger. The child run is claimed by a *different* agent (or is Queued), so parent-ownership scoping cannot apply to these reads — `agentOrServerAuth` is the intended mechanism. GET requests are not audited (`audit.go`: only POST/PUT/DELETE), so moving them out of the `/api/v1` group changes no audit behavior.

- [ ] **Step 1: Write the failing test (RED)**

Add to `internal/controller/api_agent_test.go`:

```go
// TestAgentAPI_ReadRunAndOutputs verifies an enrolled agent may read a run and
// its outputs (the call: poll path), even a run it did not claim.
func TestAgentAPI_ReadRunAndOutputs(t *testing.T) {
	s, pg := newTestServer(t)
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, nil, "")
	// Claimed by a different agent; the reader (a1) never claimed it.
	claimRunForTest(t, pg, "a2", run.ID)
	token := issueAgentAccessForTest(t, pg, "a1", nil, nil)

	for _, path := range []string{"/api/v1/runs/" + run.ID, "/api/v1/runs/" + run.ID + "/outputs"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Router().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "GET %s: %s", path, rec.Body.String())
	}
}
```

- [ ] **Step 2: Run it — expect FAIL (RED)**

Run: `go test ./internal/controller/ -run 'TestAgentAPI_ReadRunAndOutputs' -count=1 -v`
Expected: FAIL with 401 — the `uca_` bearer is rejected by the group-wide `ServerAuth`.

- [ ] **Step 3: Add the `viewerOrAgent` middleware**

In `internal/controller/agent_auth.go`, add:

```go
// viewerOrAgent admits an authenticated agent principal, otherwise enforces the
// human viewer role. Pair it with agentOrServerAuth on read routes that both an
// enrolled agent and a human viewer may call.
func (s *Server) viewerOrAgent(next http.Handler) http.Handler {
	viewer := requireMinRole("viewer")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := agentPrincipalFromContext(r.Context()); ok {
			next.ServeHTTP(w, r)
			return
		}
		viewer(next).ServeHTTP(w, r)
	})
}
```

- [ ] **Step 4: Move the two GETs out of the ServerAuth group**

In `internal/controller/server.go`, DELETE these two lines from inside the `s.r.Route("/api/v1", …)` group (currently lines 349 and 355):

```go
		r.With(view).Get("/runs/{id}", s.handleGetRun)
```
```go
		r.With(view).Get("/runs/{id}/outputs", s.handleGetRunOutputs)
```

Then register them individually just above the `s.r.Route("/api/v1", …)` block, next to the existing individually-registered SSE route (`server.go:327-328`), so they use `agentOrServerAuth` instead of the group's `ServerAuth`:

```go
	// Run + outputs reads are also reachable by an enrolled agent (the call:
	// step polls the child run it created), so they use agentOrServerAuth like
	// the artifact routes rather than the human-only /api/v1 group. viewerOrAgent
	// keeps the human viewer floor while letting agent principals through.
	s.r.With(s.agentOrServerAuth, s.viewerOrAgent).Get("/api/v1/runs/{id}", s.handleGetRun)
	s.r.With(s.agentOrServerAuth, s.viewerOrAgent).Get("/api/v1/runs/{id}/outputs", s.handleGetRunOutputs)
```

- [ ] **Step 5: Run it — expect PASS (GREEN)**

Run: `go test ./internal/controller/ -run 'TestAgentAPI_ReadRunAndOutputs|GetRun|RunOutputs' -count=1 -v`
Expected: PASS. Then confirm the human path is intact:
Run: `go test ./internal/controller/ -run 'TestAgentImpersonation|Matrix|ServerAuth' -count=1`
Expected: PASS (the impersonation matrix and human auth still hold; `/runs/{id}` is no longer in the group but the individual registration preserves the human viewer gate).

- [ ] **Step 6: Commit** (`feat(controller): allow enrolled agents to read run + outputs for call: polling`).

---

### Task 3: Agent client + callstep — use the child-run endpoint

**Files:**
- Modify: `internal/agent/client.go:214-220` (replace `CreateRun` with `CreateChildRun`)
- Modify: `internal/agent/callstep.go:44` (pass agentID + parent runID)
- Test: `internal/agent/callstep_test.go` (adjust the fake server path; or `client_test.go` if that's where CreateRun is tested)

**Interfaces:**
- Consumes: the new route from Task 1.
- Produces: `func (c *Client) CreateChildRun(ctx context.Context, agentID, parentRunID, jobName string, params map[string]string) (api.Run, error)` hitting `POST /api/v1/agents/{agentId}/runs/{parentRunID}/children`. `ExecuteCallStep` already receives `agentID` and the parent `runID` (`callstep.go:27`), so no signature change there.

- [ ] **Step 1: Find and update the CreateRun test expectation (RED)**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd && grep -rn 'CreateRun\|/api/v1/runs"' internal/agent/*_test.go`
In whichever test asserts the request path/shape for the call-step create (a fake `httptest` server matching `POST /api/v1/runs`), change the expected path to `POST /api/v1/agents/<agentID>/runs/<parentRunID>/children` and, if the test calls `client.CreateRun` directly, switch it to `client.CreateChildRun(ctx, agentID, parentRunID, job, params)`. If no direct unit test exists for this path, add one to `internal/agent/callstep_test.go` asserting the fake server receives the `.../children` path.

- [ ] **Step 2: Run it — expect FAIL (RED)**

Run: `go test ./internal/agent/ -run 'CallStep|CreateRun|CreateChildRun' -count=1 -v`
Expected: FAIL — `CreateChildRun` is undefined / the fake server sees the old path.

- [ ] **Step 3: Replace `CreateRun` with `CreateChildRun`**

In `internal/agent/client.go`, replace the `CreateRun` method (lines 214-220) with:

```go
// CreateChildRun creates a child Run for a call: step, authorized by the
// parent run the agent has claimed. agentID/parentRunID scope the request to
// the agent-authenticated child-run endpoint (the human POST /api/v1/runs is
// not reachable with a uca_ credential).
func (c *Client) CreateChildRun(ctx context.Context, agentID, parentRunID, jobName string, params map[string]string) (api.Run, error) {
	body := api.TriggerRunRequest{JobName: jobName, Params: params}
	var run api.Run
	_, err := c.do(ctx, http.MethodPost,
		fmt.Sprintf("/api/v1/agents/%s/runs/%s/children", agentID, parentRunID),
		body, &run)
	return run, err
}
```

- [ ] **Step 4: Update the callstep call site**

In `internal/agent/callstep.go`, change the create call (line 44) from:

```go
	childRun, err := client.CreateRun(ctx, step.Call.Job, expandedParams)
```
to:

```go
	childRun, err := client.CreateChildRun(ctx, agentID, runID, step.Call.Job, expandedParams)
```

(`agentID` and `runID` are already parameters of `ExecuteCallStep`.)

- [ ] **Step 5: Run it — expect PASS (GREEN)**

Run: `go test ./internal/agent/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit** (`feat(agent): create call: child runs via the agent child-run endpoint`).

---

### Task 4: e2e — clean cutover (remove the PAT masking hack) + prove enrolled `call:`

**Files:**
- Modify: `test/e2e/phase5_test.go:94-113` (remove `issueCallStepAgentToken`)
- Modify: `test/e2e/phase2_test.go:260` (use the plain agent token)

**Interfaces:**
- Consumes: existing `issueAgentAccessToken(t, pg, agentID)` (already used elsewhere in the e2e package).

Rationale: `issueCallStepAgentToken` minted a `uca_` token AND registered it as a developer PAT — the only reason a `call:` step ever worked for an "agent" in CI. With Tasks 1-3, the agent creates children via its own credential, so this double-life is gone. The existing `call:` e2e test then becomes the real regression guard: a plain enrolled agent doing `call:` end-to-end.

- [ ] **Step 1: Point the call: e2e test at the plain agent token (RED)**

In `test/e2e/phase2_test.go`, change line 260 from:

```go
		ag := agent.New(agentID, agent.NewClient(httpSrv.URL, issueCallStepAgentToken(t, pg, agentID)))
```
to:

```go
		ag := agent.New(agentID, agent.NewClient(httpSrv.URL, issueAgentAccessToken(t, pg, agentID)))
```

Also update the comment at `phase2_test.go:253-256` to drop the reference to `issueCallStepAgentToken` (state that each agent uses its own enrolled credential and creates children via the agent child-run endpoint).

- [ ] **Step 2: Run the call: e2e — expect PASS with the real path (still GREEN if on Linux/mac; skipped on Windows)**

Run: `go test ./test/e2e/ -run 'Phase2.*[Cc]all|CallJob|Phase2' -count=1 -v`
Expected on Linux/mac: PASS — the child run is created through `/api/v1/agents/{id}/runs/{parent}/children` with the agent's own credential, no PAT. On Windows the phase tests `t.Skip` (see `phase5_test.go:117`); run this task's verification on WSL/Linux.

- [ ] **Step 3: Remove the now-unused helper**

Delete `issueCallStepAgentToken` and its doc comment (`test/e2e/phase5_test.go:94-113`). Confirm no other references remain:
Run: `cd /c/Users/arimax/unified-cd-project/unified-cd && grep -rn 'issueCallStepAgentToken' test/`
Expected: no output.

- [ ] **Step 4: Build + vet + full short suite**

Run: `go build ./... && go vet ./... && go test ./... -short -count=1`
Expected: green. (e2e phase tests are Linux/mac; on Windows they skip — run the e2e verification on WSL as in the prior PRs.)

- [ ] **Step 5: Commit** (`test(e2e): enrolled agent runs call: via child-run endpoint; drop PAT masking hack`).

---

## Self-Review

**Spec coverage (`.superpowers/sdd/callstep-auth-investigation.md` §4 Option 1):**
- Dedicated `agentAuth` child-run endpoint scoped by `claimed_by` via `agentRunGuard` → Task 1 (`handleAgentCreateChildRun` + matrix route with `bindPath` → `agentAuth`+`requireAgentPathIdentity`; `agentRunGuard(...,true)`).
- Reuse `store.CreateRun` + record parent link → Task 1 (`createRunFromJob` → `CreateRun`; parent link is still established by the parent's step report `child_run_id`, `callstep.go:56-60`, unchanged).
- Point `Client.CreateRun` at the new path (parent runID + agentID) → Task 3.
- Agent read access to `/runs/{id}` + `/runs/{id}/outputs` via `agentOrServerAuth` → Task 2.
- Clean cutover (remove the test double-registration) → Task 4. ✓

**Placeholder scan:** every code step shows complete code; every run step gives an exact command + expected result. No TBD/TODO.

**Type consistency:** `createRunFromJob` returns `(*api.Run, int, string)` and is called identically in `handleTriggerRun` (Task 1 Step 3) and `handleAgentCreateChildRun` (Task 1 Step 4). `CreateChildRun(ctx, agentID, parentRunID, jobName, params)` defined in Task 3 Step 3 is called with exactly those args in Task 4 via `ExecuteCallStep`'s existing `agentID`/`runID` (Task 3 Step 4). Route path string `"/api/v1/agents/{agentId}/runs/{runId}/children"` (matrix, Task 1 Step 5) matches the client's `fmt.Sprintf("/api/v1/agents/%s/runs/%s/children", agentID, parentRunID)` (Task 3 Step 3) — chi params `agentId`/`runId` are what `requireAgentPathIdentity` (`{agentId}`) and `handleAgentCreateChildRun` (`chi.URLParam(r, "runId")`) read.

**Ownership guard correctness:** the child run is created against `child-job`; the guard authorizes on the PARENT run (`parentRunID` from the path), which the agent claimed — matching `handleAgentSetRunOutputs`'s guard usage (`api_agent.go:639`). `rejectTerminal=true`: a parent mid-`call:` is Running, so a terminal parent (already finished) correctly cannot spawn children.

**Ordering:** Task 1 (endpoint) → Task 2 (reads) → Task 3 (client uses endpoint) → Task 4 (e2e proves the whole path + removes the mask). Tasks 1 and 2 are independent; 3 depends on 1; 4 depends on 1+2+3.
