# k8s Agent Resilience (G-A1 / G-A2 / G-A3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the k8s agent survive stuck run pods, controller-side reaps, and shutdown/overload — by porting the host agent's already-proven drain / bounded-concurrency / retried-fail-report patterns to k8s and bounding + cancel-arming the run-pod wait.

**Architecture:** Five fix sites across `internal/k8sagent/{config,agent,podmanager}.go` and `internal/agent/orchestrator.go`. The host agent (`internal/agent/agent.go`) is the reference implementation for G-A3 — mirror it. No controller, DSL, DB, or manifest changes.

**Tech Stack:** Go, `context`, `sync`, k8s client-go, `httptest`-based fake controller + in-package fakes (`fakePM`, `fakeExec`, `fakeK8sBackend`).

## Global Constraints

- Reuse shared helpers, do not reinvent: `agentlib.RetryUntilSuccess` (`internal/agent/retry.go:60`), `isTerminalRunStatus` (`internal/k8sagent/podgc.go:27`), `agentlib.StartHeartbeat` / `agentlib.DefaultHeartbeatInterval` / `agentlib.CancelPollInterval`.
- `PodStartTimeout` default **5m**; non-positive → 5m. Env `UNIFIED_K8S_POD_START_TIMEOUT`, yaml `podStartTimeout`.
- `DrainTimeout` default **0 = wait indefinitely**. Env `UNIFIED_K8S_DRAIN_TIMEOUT`, yaml `drainTimeout`. **No CLI flag** (k8s agent configures via file+env only).
- `MaxConcurrent`: **default 100**; unset/`0` → 100; **negative → unlimited** (no semaphore); positive → that bound. `Validate` maps `0→100`, leaves negatives untouched.
- Config duration fields are stored as **strings** and parsed via helper methods (follow the existing `PoolIdleTimeout` / `PoolIdleTimeoutDuration()` pattern in `config.go`).
- Every duration/env override is read in `Config.Validate` (follow the existing `UNIFIED_K8S_AGENT_ID` override there).
- Match existing k8s log style: `slog` with `"runId"` / `"pod"` keys, messages prefixed `k8s: `.
- Tests must run WITHOUT a real cluster (no `//go:build k8s` tag) — use `httptest` fake controllers and the in-package fakes. Shorten `agentlib.RetryInitialWait/RetryMaxWait` and `agentlib.CancelPollInterval` in tests (save/restore in `t.Cleanup`).

---

### Task 1: Config — `PodStartTimeout` + `DrainTimeout` fields, duration helpers, env overrides, `MaxConcurrent` default 100 / unlimited

**Files:**
- Modify: `internal/k8sagent/config.go`
- Test: `internal/k8sagent/config_test.go`

**Interfaces:**
- Produces:
  - `Config.PodStartTimeout string` (yaml `podStartTimeout`), `Config.DrainTimeout string` (yaml `drainTimeout`)
  - `func (c *Config) PodStartTimeoutDuration() time.Duration` — returns the parsed value, or **5m** when unset/invalid/non-positive
  - `func (c *Config) DrainTimeoutDuration() time.Duration` — returns the parsed value, or **0** when unset/invalid
  - `DefaultConfig().MaxConcurrent == 100`
  - `Validate`: sets `MaxConcurrent = 100` iff it is `0`; leaves negative values unchanged; applies env overrides `UNIFIED_K8S_POD_START_TIMEOUT` / `UNIFIED_K8S_DRAIN_TIMEOUT`; rejects an unparseable `podStartTimeout` / `drainTimeout` with an error.

- [ ] **Step 1: Write the failing tests**

Add to `internal/k8sagent/config_test.go`:

```go
func TestPodStartTimeoutDuration_DefaultAndParse(t *testing.T) {
	c := DefaultConfig()
	if got := c.PodStartTimeoutDuration(); got != 5*time.Minute {
		t.Fatalf("unset podStartTimeout: want 5m, got %v", got)
	}
	c.PodStartTimeout = "90s"
	if got := c.PodStartTimeoutDuration(); got != 90*time.Second {
		t.Fatalf("parsed podStartTimeout: want 90s, got %v", got)
	}
	c.PodStartTimeout = "0s" // non-positive -> default
	if got := c.PodStartTimeoutDuration(); got != 5*time.Minute {
		t.Fatalf("non-positive podStartTimeout: want 5m, got %v", got)
	}
}

func TestDrainTimeoutDuration_DefaultAndParse(t *testing.T) {
	c := DefaultConfig()
	if got := c.DrainTimeoutDuration(); got != 0 {
		t.Fatalf("unset drainTimeout: want 0, got %v", got)
	}
	c.DrainTimeout = "30s"
	if got := c.DrainTimeoutDuration(); got != 30*time.Second {
		t.Fatalf("parsed drainTimeout: want 30s, got %v", got)
	}
}

func TestValidate_MaxConcurrentDefaultAndUnlimited(t *testing.T) {
	base := func() Config {
		return Config{Server: "s", Token: "t", AgentID: "a"}
	}
	// 0 -> 100
	c := base()
	c.MaxConcurrent = 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxConcurrent != 100 {
		t.Fatalf("zero maxConcurrent: want 100, got %d", c.MaxConcurrent)
	}
	// negative -> unchanged (unlimited sentinel)
	c = base()
	c.MaxConcurrent = -1
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxConcurrent != -1 {
		t.Fatalf("negative maxConcurrent must be preserved as unlimited, got %d", c.MaxConcurrent)
	}
	// positive -> unchanged
	c = base()
	c.MaxConcurrent = 3
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxConcurrent != 3 {
		t.Fatalf("positive maxConcurrent: want 3, got %d", c.MaxConcurrent)
	}
}

func TestValidate_DurationEnvOverridesAndParseError(t *testing.T) {
	t.Setenv("UNIFIED_K8S_POD_START_TIMEOUT", "42s")
	t.Setenv("UNIFIED_K8S_DRAIN_TIMEOUT", "7s")
	c := Config{Server: "s", Token: "t", AgentID: "a"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PodStartTimeoutDuration() != 42*time.Second {
		t.Fatalf("env override podStartTimeout: got %v", c.PodStartTimeoutDuration())
	}
	if c.DrainTimeoutDuration() != 7*time.Second {
		t.Fatalf("env override drainTimeout: got %v", c.DrainTimeoutDuration())
	}

	bad := Config{Server: "s", Token: "t", AgentID: "a", PodStartTimeout: "not-a-duration"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected Validate to reject an unparseable podStartTimeout")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/k8sagent/ -run 'PodStartTimeoutDuration|DrainTimeoutDuration|MaxConcurrentDefaultAndUnlimited|DurationEnvOverrides' -count=1`
Expected: FAIL (compile error — `PodStartTimeout` field / helpers do not exist yet).

- [ ] **Step 3: Add the fields, helpers, defaults, and validation**

In `internal/k8sagent/config.go`, add the two fields to the `Config` struct (next to `PoolIdleTimeout`):

```go
	PoolIdleTimeout     string                      `yaml:"poolIdleTimeout,omitempty"`
	PodStartTimeout     string                      `yaml:"podStartTimeout,omitempty"`
	DrainTimeout        string                      `yaml:"drainTimeout,omitempty"`
	PodTemplates        map[string]AgentPodTemplate `yaml:"podTemplates,omitempty"`
```

Change the default in `DefaultConfig()`:

```go
		MaxConcurrent: 100,
```

Add the helper methods after `PoolIdleTimeoutDuration`:

```go
// defaultPodStartTimeout bounds how long executeRun waits for a run Pod to
// reach Running before failing the run (see agent.go). Matches the throwaway
// scope-pod bound (imagePodStartTimeout).
const defaultPodStartTimeout = 5 * time.Minute

// PodStartTimeoutDuration parses PodStartTimeout, returning defaultPodStartTimeout
// when unset, unparseable, or non-positive.
func (c *Config) PodStartTimeoutDuration() time.Duration {
	if c.PodStartTimeout == "" {
		return defaultPodStartTimeout
	}
	d, err := time.ParseDuration(c.PodStartTimeout)
	if err != nil || d <= 0 {
		return defaultPodStartTimeout
	}
	return d
}

// DrainTimeoutDuration parses DrainTimeout, returning 0 (wait indefinitely)
// when unset or unparseable.
func (c *Config) DrainTimeoutDuration() time.Duration {
	if c.DrainTimeout == "" {
		return 0
	}
	d, err := time.ParseDuration(c.DrainTimeout)
	if err != nil {
		return 0
	}
	return d
}
```

In `Validate()`, replace the `MaxConcurrent` clamp and add env overrides + parse validation. Change:

```go
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 5
	}
```

to:

```go
	// maxConcurrent: 0/unset -> default 100; negative -> unlimited (preserved
	// as a sentinel; the run loop skips its semaphore); positive -> that bound.
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 100
	}
```

And, next to the existing `UNIFIED_K8S_AGENT_ID` override at the top of `Validate`, add env overrides (env wins over file):

```go
	if v := os.Getenv("UNIFIED_K8S_POD_START_TIMEOUT"); v != "" {
		c.PodStartTimeout = v
	}
	if v := os.Getenv("UNIFIED_K8S_DRAIN_TIMEOUT"); v != "" {
		c.DrainTimeout = v
	}
```

And, next to the existing `poolIdleTimeout` parse check near the end of `Validate`, add:

```go
	if c.PodStartTimeout != "" {
		if _, err := time.ParseDuration(c.PodStartTimeout); err != nil {
			return fmt.Errorf("podStartTimeout %q: %w", c.PodStartTimeout, err)
		}
	}
	if c.DrainTimeout != "" {
		if _, err := time.ParseDuration(c.DrainTimeout); err != nil {
			return fmt.Errorf("drainTimeout %q: %w", c.DrainTimeout, err)
		}
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/k8sagent/ -run 'PodStartTimeoutDuration|DrainTimeoutDuration|MaxConcurrentDefaultAndUnlimited|DurationEnvOverrides' -count=1`
Expected: PASS.

Also run the existing config tests to catch the changed default (a test may assert `MaxConcurrent == 5`):
Run: `go test ./internal/k8sagent/ -run Config -count=1`
Expected: PASS — if an existing test asserts the old default of 5, update it to 100 (the default changed intentionally per the spec).

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/config.go internal/k8sagent/config_test.go
git commit -m "feat(k8s): podStartTimeout/drainTimeout config + maxConcurrent default 100/unlimited"
```

---

### Task 2: k8s `failRun` helper + retried failure reporting at pod-acquisition sites (G-A3b)

**Files:**
- Modify: `internal/k8sagent/agent.go`
- Test: `internal/k8sagent/failrun_test.go` (create)

**Interfaces:**
- Produces: `func (a *K8sAgent) failRun(ctx context.Context, runID, reason string)` — appends a System (`stepIndex -1`) stderr log line with `reason` (best-effort) then `FinishRun(RunFailed)` wrapped in `agentlib.RetryUntilSuccess`. Mirrors the host `Agent.failRun` (`internal/agent/agent.go:367-379`).
- Consumes: `agentlib.RetryUntilSuccess`, `a.client.AppendLogBulk`, `a.client.FinishRun`.

- [ ] **Step 1: Write the failing test**

Create `internal/k8sagent/failrun_test.go`:

```go
package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
)

// TestFailRun_RetriesFinishAndLogsReason verifies failRun surfaces its reason
// as a System log line and retries FinishRun past a transient 500.
func TestFailRun_RetriesFinishAndLogsReason(t *testing.T) {
	prevInitial, prevMax := agentlib.RetryInitialWait, agentlib.RetryMaxWait
	agentlib.RetryInitialWait, agentlib.RetryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { agentlib.RetryInitialWait, agentlib.RetryMaxWait = prevInitial, prevMax })

	var finishCalls atomic.Int32
	var mu sync.Mutex
	var gotLogLine string
	var gotFinishStatus string

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		var reqs []api.LogAppendRequest
		_ = orchestrateDecodeJSON(r, &reqs)
		mu.Lock()
		if len(reqs) > 0 {
			gotLogLine = reqs[0].Line
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, r *http.Request) {
		if finishCalls.Add(1) <= 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var req struct {
			Status string `json:"status"`
		}
		_ = orchestrateDecodeJSON(r, &req)
		mu.Lock()
		gotFinishStatus = req.Status
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := agentlib.NewClient(srv.URL, "tok")
	a := &K8sAgent{cfg: Config{AgentID: "k8s-1"}, client: client}

	a.failRun(context.Background(), "r1", "boom: pod did not start")

	mu.Lock()
	defer mu.Unlock()
	if gotLogLine != "boom: pod did not start" {
		t.Fatalf("system log line: got %q", gotLogLine)
	}
	if gotFinishStatus != string(api.RunFailed) {
		t.Fatalf("finish status: got %q, want Failed", gotFinishStatus)
	}
	if finishCalls.Load() < 2 {
		t.Fatalf("expected FinishRun to be retried (>=2 calls), got %d", finishCalls.Load())
	}
}
```

Note: `orchestrateDecodeJSON` is the existing test helper used by `report_retry_test.go`; reuse it.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestFailRun_RetriesFinishAndLogsReason -count=1`
Expected: FAIL (compile error — `a.failRun` undefined).

- [ ] **Step 3: Add `failRun` and route the acquisition sites through it**

In `internal/k8sagent/agent.go`, add the helper (place it right after `executeRun`):

```go
// failRun fails a claim that could not begin executing (pod build/create/acquire
// or the run pod never becoming ready). reason is surfaced into the run's own
// logs (stepIndex -1, rendered "System" in the UI) before FinishRun(Failed).
// The log line is best-effort; FinishRun is retried until it lands so the run
// never sits stuck as Running. Mirrors the host agent's Agent.failRun.
func (a *K8sAgent) failRun(ctx context.Context, runID, reason string) {
	slog.Error(reason, "runId", runID)
	_ = a.client.AppendLogBulk(ctx, a.cfg.AgentID, runID, -1, []api.LogAppendRequest{{
		RunID:     runID,
		StepIndex: -1,
		Stream:    "stderr",
		Timestamp: time.Now().UTC(),
		Line:      reason,
	}})
	agentlib.RetryUntilSuccess(ctx, func(cc context.Context) error {
		return a.client.FinishRun(cc, a.cfg.AgentID, runID, api.RunFailed)
	})
}
```

Replace the Native-branch block (currently `agent.go:150-164`) — it already does this inline — with a `failRun` call:

```go
	if c.Native {
		a.failRun(ctx, c.RunID, "native: true jobs are host-only; the k8s agent cannot run them")
		return
	}
```

Replace the three single-shot pod-acquisition failure sites (the `WaitForPodRunning` site is handled in Task 3):

At the pool `ClaimPod` failure (currently `agent.go:184-188`):
```go
		if err != nil {
			a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: failed to acquire Pod: %v", err))
			return
		}
```

At the `BuildPod` failure (currently `agent.go:199-203`):
```go
		if err != nil {
			a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: failed to build Pod spec: %v", err))
			return
		}
```

At the `CreatePod` failure (currently `agent.go:205-209`):
```go
		created, err := a.pm.CreatePod(ctx, pod)
		if err != nil {
			a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: failed to create Pod: %v", err))
			return
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/k8sagent/ -run 'TestFailRun|TestOrchestrate_ReportRetries' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/failrun_test.go
git commit -m "feat(k8s): retried failRun helper for pod-acquisition failures (G-A3b)"
```

---

### Task 3: Bound the run-pod wait + cancel-aware watcher (G-A1)

**Files:**
- Modify: `internal/k8sagent/agent.go`
- Modify: `internal/k8sagent/fakepm_test.go` (add optional blocking to the fake)
- Test: `internal/k8sagent/podwait_test.go` (create)

**Interfaces:**
- Consumes: `failRun` (Task 2), `a.cfg.PodStartTimeoutDuration()` (Task 1), `isTerminalRunStatus`, `agentlib.CancelPollInterval`, `a.client.GetRun`.
- Produces: `func (a *K8sAgent) awaitPodRunning(ctx context.Context, podName, runID string) (masterTerminal bool, err error)` — waits for the pod to be Running bounded by `PodStartTimeoutDuration()`, aborting early (returning `masterTerminal=true, err!=nil`) if the controller marks the run terminal before it is ready.

- [ ] **Step 1: Make the fake pod manager optionally block, and write the failing tests**

In `internal/k8sagent/fakepm_test.go`, add a `waitBlock` field and honor it (backward-compatible: nil = current immediate behavior):

```go
type fakePM struct {
	created         *corev1.Pod
	createdNm       string
	waitErr         error
	waitBlock       chan struct{} // if non-nil, WaitForPodRunning blocks until closed or ctx done
	deleted         []string
	waitHadDeadline bool
	waitCtxSeen     bool
}
```

```go
func (f *fakePM) WaitForPodRunning(ctx context.Context, _ string) error {
	f.waitCtxSeen = true
	_, hasDeadline := ctx.Deadline()
	f.waitHadDeadline = hasDeadline
	if f.waitBlock != nil {
		select {
		case <-f.waitBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return f.waitErr
}
```

Create `internal/k8sagent/podwait_test.go`:

```go
package k8sagent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
)

// controllerForWait returns a fake controller whose GET /runs/{id} reports the
// status currently stored in *status, and records whether finish was called.
func controllerForWait(t *testing.T, status *atomic.Value, finishCalled *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/agents/{id}/logs", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /api/v1/runs/{id}", func(w http.ResponseWriter, _ *http.Request) {
		s, _ := status.Load().(string)
		if s == "" {
			s = "Running"
		}
		orchestrateWriteJSON(w, api.Run{ID: "r1", Status: api.RunStatus(s)})
	})
	mux.HandleFunc("POST /api/v1/agents/{id}/runs/{runId}/finish", func(w http.ResponseWriter, _ *http.Request) {
		finishCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestExecuteRun_WaitFailureFailsRunWithDeadline: a fresh (non-pooled) run whose
// pod never becomes ready is failed (retried) and its pod deleted, and the wait
// carried a deadline.
func TestExecuteRun_WaitFailureFailsRunWithDeadline(t *testing.T) {
	prevInitial, prevMax := agentlib.RetryInitialWait, agentlib.RetryMaxWait
	agentlib.RetryInitialWait, agentlib.RetryMaxWait = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { agentlib.RetryInitialWait, agentlib.RetryMaxWait = prevInitial, prevMax })

	var status atomic.Value
	var finishCalled atomic.Int32
	srv := controllerForWait(t, &status, &finishCalled)
	client := agentlib.NewClient(srv.URL, "tok")

	pm := &fakePM{waitErr: errors.New("pod stuck pending")}
	a := &K8sAgent{
		cfg:    Config{AgentID: "k8s-1", Namespace: "ns", PodImage: "img", ShimImage: "shim", PodStartTimeout: "50ms"},
		client: client,
		pm:     pm,
	}
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "s", Run: "echo ok"}},
	}}

	a.executeRun(context.Background(), c)

	if !pm.waitHadDeadline {
		t.Fatal("run-pod wait must carry a deadline (PodStartTimeout)")
	}
	if finishCalled.Load() == 0 {
		t.Fatal("a wait failure must fail the run (FinishRun) via failRun")
	}
	if len(pm.deleted) == 0 {
		t.Fatal("the created pod must be deleted on wait failure (no leak)")
	}
}

// TestExecuteRun_MasterTerminalDuringWaitAbandonsWithoutOverride: the run is
// flipped Cancelled by the controller while the pod is still not ready; the wait
// aborts early and executeRun returns WITHOUT overriding the controller status.
func TestExecuteRun_MasterTerminalDuringWaitAbandonsWithoutOverride(t *testing.T) {
	prevPoll := agentlib.CancelPollInterval
	agentlib.CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { agentlib.CancelPollInterval = prevPoll })

	var status atomic.Value
	status.Store("Running")
	var finishCalled atomic.Int32
	srv := controllerForWait(t, &status, &finishCalled)
	client := agentlib.NewClient(srv.URL, "tok")

	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	pm := &fakePM{waitBlock: block} // blocks until ctx is cancelled by the watcher
	a := &K8sAgent{
		cfg:    Config{AgentID: "k8s-1", Namespace: "ns", PodImage: "img", ShimImage: "shim", PodStartTimeout: "10s"},
		client: client,
		pm:     pm,
	}
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "s", Run: "echo ok"}},
	}}

	// Flip the run terminal shortly after executeRun starts waiting.
	go func() {
		time.Sleep(20 * time.Millisecond)
		status.Store("Cancelled")
	}()

	done := make(chan struct{})
	go func() { a.executeRun(context.Background(), c); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("executeRun did not return after the run went terminal during the wait")
	}

	if finishCalled.Load() != 0 {
		t.Fatal("must NOT override controller status when the run is already terminal")
	}
	if len(pm.deleted) == 0 {
		t.Fatal("the created pod must still be deleted when abandoning a terminal run")
	}
}

var _ = &sync.Mutex{} // keep sync import if unused after edits
```

(Remove the trailing `var _` line if `sync` ends up used/unused — it is only a guard so the file compiles regardless.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/k8sagent/ -run 'TestExecuteRun_WaitFailure|TestExecuteRun_MasterTerminalDuringWait' -count=1`
Expected: FAIL — currently the wait has no deadline (`waitHadDeadline` false) and no cancel-aware abort.

- [ ] **Step 3: Add `awaitPodRunning`, track pod readiness, and rewire the wait site**

In `internal/k8sagent/agent.go`, add the imports `"sync/atomic"` (if not present) and keep `"fmt"`, `"time"`. Add the helper after `failRun`:

```go
// awaitPodRunning waits for podName to reach Running, bounded by
// cfg.PodStartTimeoutDuration(), and abortable early if the controller marks the
// run terminal (user cancel or reap) before the pod is ready. Under
// RestartPolicy: Never a Pending/ImagePullBackOff pod never transitions to
// Failed, so without this bound the wait would hang until full agent shutdown.
//
// It returns masterTerminal=true (with a non-nil err) when the wait was aborted
// because the run is already terminal at the controller — the caller must clean
// up the pod but must NOT override the controller's authoritative status.
func (a *K8sAgent) awaitPodRunning(ctx context.Context, podName, runID string) (masterTerminal bool, err error) {
	waitCtx, cancel := context.WithTimeout(ctx, a.cfg.PodStartTimeoutDuration())
	defer cancel()

	var terminal atomic.Bool
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(agentlib.CancelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-ticker.C:
				run, gerr := a.client.GetRun(waitCtx, runID)
				if gerr != nil {
					continue
				}
				if isTerminalRunStatus(run.Status) {
					terminal.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	werr := a.pm.WaitForPodRunning(waitCtx, podName)
	cancel()
	<-watchDone

	if terminal.Load() {
		return true, fmt.Errorf("run %s reached terminal status before pod %s became ready", runID, podName)
	}
	return false, werr
}
```

Track pod readiness so a pooled pod that never started is **deleted** (not returned to the idle pool). In the pooled branch, change the deferred release (currently `agent.go:191-195`) to:

```go
		pooledPod = pp
		podName = pp.PodName
		defer func() {
			if !podReady {
				// The pod never reached Running; do not return a possibly-wedged
				// pod to the idle pool — delete it so the pool re-creates next time.
				if err := a.pm.DeletePod(context.Background(), podName); err != nil {
					slog.Warn("k8s: failed to delete not-ready pooled Pod", "pod", podName, "error", err)
				}
				return
			}
			if err := a.pool.ReleasePod(context.Background(), pooledPod, true); err != nil {
				slog.Warn("k8s: failed to release Pod", "pod", podName, "error", err)
			}
		}()
```

Declare `podReady` just before the `if usePool {` block:

```go
	var pooledPod *PooledPod
	var podName string
	podReady := false
```

(The fresh-pod branch's existing `defer ... DeletePod` already deletes regardless of readiness — leave it as-is.)

Replace the wait site (currently `agent.go:218-222`) with:

```go
	masterTerminal, err := a.awaitPodRunning(ctx, podName, c.RunID)
	if err != nil {
		if masterTerminal {
			slog.Info("k8s: run became terminal before pod ready; abandoning", "runId", c.RunID, "pod", podName)
			return
		}
		a.failRun(ctx, c.RunID, fmt.Sprintf("k8s: run pod did not become ready: %v", err))
		return
	}
	podReady = true
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/k8sagent/ -run 'TestExecuteRun_WaitFailure|TestExecuteRun_MasterTerminalDuringWait|TestFailRun' -count=1`
Expected: PASS.

- [ ] **Step 5: Run the full k8s package tests (non-tagged) to check nothing regressed**

Run: `go test ./internal/k8sagent/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/fakepm_test.go internal/k8sagent/podwait_test.go
git commit -m "feat(k8s): bound run-pod wait with timeout + cancel-aware abort (G-A1)"
```

---

### Task 4: Cancel poller reacts to any terminal status; skip terminal report when reaped (G-A2)

**Files:**
- Modify: `internal/agent/orchestrator.go`
- Test: `internal/agent/orchestrator_reap_test.go` (create)

**Interfaces:**
- Consumes: `isTerminalRunStatus` — **note:** this helper currently lives in package `k8sagent` (`podgc.go:27`), NOT package `agent`. `orchestrator.go` is in package `agent`, so it cannot import it (would be a cycle). Add a local unexported copy in package `agent` (the terminal set is also duplicated in `cli/wait.go` and `controller/sse.go`, so a local copy here is consistent with the codebase).
- Produces: broadened poller behavior + `reapedByMaster` guard around the terminal `SetRunOutputs`/`FinishRun`.

- [ ] **Step 1: Write the failing test**

**Note:** `orchestrateWriteJSON`/`orchestrateDecodeJSON` exist only in package `k8sagent`. This test is in package `agent`; use stdlib `encoding/json`. The test is modeled directly on `newRetryServer`/`retryHarness` in `orchestrator_retry_test.go` (same package) — a **real host `Agent`, native claim, real bash** driven via `Agent.executeRun`, with a fake controller. No fake `ExecBackend` is needed. A blocking `sleep 2` step gives the fast cancel-poller time to observe the `Failed` status and cancel the run before the step finishes.

Create `internal/agent/orchestrator_reap_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
)

// reapServer is a fake controller (modeled on newRetryServer) whose GET run
// endpoint reports the status currently in *status, and which counts FinishRun
// calls. It registers the routes a native RunClaim actually hits.
func reapServer(t *testing.T, agentID string, status *atomic.Value, finishCalls *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	noContent := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	mux.HandleFunc("POST /api/v1/agents/register", noContent)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/steps", noContent)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/logs/bulk", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/steps/{idx}/outputs", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/outputs", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/logs", ok) // system (stepIndex -1) log line, if any
	mux.HandleFunc("GET /api/v1/runs/{runId}", func(w http.ResponseWriter, r *http.Request) {
		s, _ := status.Load().(string)
		if s == "" {
			s = string(api.RunRunning)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("runId"), Status: api.RunStatus(s)})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/{runId}/finish", func(w http.ResponseWriter, _ *http.Request) {
		finishCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestRunClaim_ReapedByMaster_SkipsTerminalFinish: the controller reports the run
// Failed out-of-band while a step is still running; the poller cancels the run and
// RunClaim does NOT send its own terminal FinishRun (the controller is authoritative).
func TestRunClaim_ReapedByMaster_SkipsTerminalFinish(t *testing.T) {
	prevPoll := CancelPollInterval
	CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prevPoll })

	const agentID = "reap-agent"
	var status atomic.Value
	status.Store(string(api.RunFailed)) // reaped from the first poll
	var finishCalls atomic.Int32

	srv := reapServer(t, agentID, &status, &finishCalls)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native:  true,
		RunID:   "run-reap",
		JobName: "reap-job",
		Stages: []api.ClaimStage{
			{Step: &api.ClaimStep{Index: 0, Name: "blocker", Run: "sleep 2"}},
		},
	}

	done := make(chan struct{})
	go func() { a.executeRun(context.Background(), claim, t.TempDir()); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("executeRun did not return after the run was reaped")
	}

	if finishCalls.Load() != 0 {
		t.Fatalf("a master-reaped run must not send its own terminal FinishRun, got %d calls", finishCalls.Load())
	}
}

// TestRunClaim_Cancelled_StillFinishes is the contrast: an out-of-band Cancelled
// status still results in a normal terminal FinishRun(Cancelled) — the reaped-skip
// applies only to non-Cancelled terminal statuses.
func TestRunClaim_Cancelled_StillFinishes(t *testing.T) {
	prevPoll := CancelPollInterval
	CancelPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { CancelPollInterval = prevPoll })

	const agentID = "reap-agent"
	var status atomic.Value
	status.Store(string(api.RunCancelled))
	var finishCalls atomic.Int32

	srv := reapServer(t, agentID, &status, &finishCalls)
	a := &Agent{ID: agentID, Client: NewClient(srv.URL, "tok")}

	claim := api.ClaimResponse{
		Native: true, RunID: "run-cancel", JobName: "cancel-job",
		Stages: []api.ClaimStage{{Step: &api.ClaimStep{Index: 0, Name: "blocker", Run: "sleep 2"}}},
	}
	a.executeRun(context.Background(), claim, t.TempDir())

	if finishCalls.Load() == 0 {
		t.Fatal("a cancelled run must still send FinishRun(Cancelled)")
	}
}

var _ = sync.Mutex{} // guard: keep sync import valid regardless of final edits
```

If the trailing `var _ = sync.Mutex{}` (and the `sync` import) is unused, delete both — it is only there so the snippet compiles standalone.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent/ -run TestRunClaim_ReapedByMaster -count=1`
Expected: FAIL — today the poller ignores `Failed`, the step completes normally, and RunClaim sends `FinishRun` (finishCalls ≥ 1).

- [ ] **Step 3: Add the local terminal helper, `reapedByMaster`, broaden the poller, and guard the terminal report**

In `internal/agent/orchestrator.go`:

Add a local terminal-status helper (near the top of the file, package-private):

```go
// isTerminalRunStatus reports whether a run has finished at the controller.
// (Local copy: the k8s agent has its own in package k8sagent; controller/sse.go
// and cli/wait.go likewise keep their own — there is no shared exported form.)
func isTerminalRunStatus(s api.RunStatus) bool {
	switch s {
	case api.RunSucceeded, api.RunFailed, api.RunCancelled:
		return true
	default:
		return false
	}
}
```

Add the flag next to `cancelledByMaster` (currently `orchestrator.go:72`):

```go
	var cancelledByMaster atomic.Bool
	var reapedByMaster atomic.Bool // controller marked the run terminal (Failed/other) out-of-band
```

Broaden the poller check (currently `orchestrator.go:122-127`):

```go
				if isTerminalRunStatus(run.Status) {
					if run.Status == api.RunCancelled {
						slog.Info("received cancellation signal from master; interrupting run", "runID", c.RunID)
						cancelledByMaster.Store(true)
					} else {
						slog.Info("master reported run terminal; interrupting run", "runID", c.RunID, "status", run.Status)
						reapedByMaster.Store(true)
					}
					cancelRun()
					return
				}
```

Guard the terminal report. Immediately after `finishCtx := context.WithoutCancel(ctx)` (currently `orchestrator.go:667`) and BEFORE the job-outputs promotion block, add:

```go
	// If the controller already marked this run terminal out-of-band (e.g. the
	// stuck-run reaper tripped during a partition), it holds the authoritative
	// status. Do not promote outputs or send our own FinishRun — that would race
	// or overwrite the controller's decision. The pipeline was already stopped by
	// the poller's cancelRun(). (Cancelled is handled by the normal path so the
	// run is still reported Cancelled.)
	if reapedByMaster.Load() {
		slog.Info("run already terminal at master; skipping local outputs/finish", "runId", c.RunID)
		return
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agent/ -run 'TestRunClaim_ReapedByMaster|Cancel' -count=1`
Expected: PASS — the reaped run sends no FinishRun; existing cancellation tests still pass (Cancelled path unchanged).

- [ ] **Step 5: Run the full agent package with race detector**

Run: `go test ./internal/agent/ -race -count=1`
Expected: PASS (the new atomic + poller change must be race-clean).

- [ ] **Step 6: Commit**

```bash
git add internal/agent/orchestrator.go internal/agent/orchestrator_reap_test.go
git commit -m "feat(agent): cancel poller reacts to any terminal status; skip terminal report when reaped (G-A2)"
```

---

### Task 5: Graceful drain + bounded/unlimited concurrency in `K8sAgent.Run` (G-A3a + G-A3c)

**Files:**
- Modify: `internal/k8sagent/agent.go`
- Test: `internal/k8sagent/drain_test.go` (create)

**Interfaces:**
- Consumes: `a.cfg.DrainTimeoutDuration()`, `a.cfg.MaxConcurrent`, `agentlib.StartHeartbeat` (returns `<-chan struct{}`), `a.client.Deregister`.
- Produces:
  - A test seam field on `K8sAgent`: `dispatch func(ctx context.Context, c api.ClaimResponse)` — defaults to `a.executeRun`, set in `NewK8sAgent`; the claim loop calls `a.dispatch(runCtx, resp)`. Lets drain/concurrency be tested without a pod backend.
  - Restructured `Run`: `claimCtx`(=incoming ctx)/`runCtx` split, `DrainTimeout` goroutine, heartbeat bound to `runCtx` and joined, in-flight `sync.WaitGroup`, `MaxConcurrent` semaphore (nil when unlimited), `Deregister` on exit.

- [ ] **Step 1: Add the dispatch seam and write the failing tests**

In `internal/k8sagent/agent.go`, add the field to `K8sAgent`:

```go
type K8sAgent struct {
	cfg    Config
	client *agentlib.Client
	pm     podManager
	exec   stepExecutor
	pool   *PodPool
	// dispatch executes one claimed run. Defaults to executeRun; overridable in
	// tests to exercise the claim loop's drain/concurrency without a pod backend.
	dispatch func(ctx context.Context, c api.ClaimResponse)
}
```

Set the default in `NewK8sAgent` (before `return`):

```go
func NewK8sAgent(cfg Config, agentClient *agentlib.Client, pm *PodManager, exec *Executor, pool *PodPool) *K8sAgent {
	a := &K8sAgent{cfg: cfg, client: agentClient, pm: pm, exec: exec, pool: pool}
	a.dispatch = a.executeRun
	return a
}
```

Create `internal/k8sagent/drain_test.go`:

```go
package k8sagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
)

// claimController hands out `total` claims, then empty responses, and accepts
// register/heartbeat/deregister/reconcile. Routes verified against
// internal/agent/client.go: register POST /api/v1/agents/register; reconcile
// POST /api/v1/agents/{id}/runs/reconcile returning {"failedRuns":N}; claim POST
// /api/v1/agents/{id}/claim; heartbeat POST /api/v1/agents/{id}/heartbeat;
// deregister DELETE /api/v1/agents/{id}. Uses stdlib encoding/json (this is
// package k8sagent, so orchestrateWriteJSON is available too — either works).
func claimController(t *testing.T, agentID string, total *atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	mux.HandleFunc("POST /api/v1/agents/register", ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/heartbeat", ok)
	mux.HandleFunc("DELETE /api/v1/agents/"+agentID, ok)
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/runs/reconcile", func(w http.ResponseWriter, _ *http.Request) {
		orchestrateWriteJSON(w, map[string]int{"failedRuns": 0})
	})
	mux.HandleFunc("POST /api/v1/agents/"+agentID+"/claim", func(w http.ResponseWriter, _ *http.Request) {
		if total.Add(-1) >= 0 {
			orchestrateWriteJSON(w, api.ClaimResponse{RunID: "r"})
			return
		}
		orchestrateWriteJSON(w, api.ClaimResponse{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
```

Both tests pass the agent ID to `claimController` (`claimController(t, "k8s-1", &remaining)`) so the path-scoped routes match the configured `AgentID`.

```go
// TestRun_DrainWaitsForInflight: an in-flight dispatch keeps running under runCtx
// after claimCtx is cancelled, and Run returns only once it completes.
func TestRun_DrainWaitsForInflight(t *testing.T) {
	var remaining atomic.Int32
	remaining.Store(1) // exactly one claim
	srv := claimController(t, "k8s-1", &remaining)
	client := agentlib.NewClient(srv.URL, "tok")

	// Build with a real pm/pool over a fake clientset so Restore/GC no-op cleanly.
	// (If constructing PodPool/PodManager without a cluster is impractical here,
	// use k8s.io/client-go/kubernetes/fake to build the clientset.)
	a := newK8sAgentForTest(t, Config{AgentID: "k8s-1", Namespace: "ns", MaxConcurrent: 5}, client)

	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	a.dispatch = func(ctx context.Context, c api.ClaimResponse) {
		close(started)
		<-release // hold the run "in flight"
		finished.Store(true)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = a.Run(ctx); close(runDone) }()

	<-started       // a run is in flight
	cancel()        // begin drain (stop claiming)
	time.Sleep(50 * time.Millisecond)
	if finished.Load() {
		t.Fatal("in-flight run should still be running during drain")
	}
	select {
	case <-runDone:
		t.Fatal("Run returned before the in-flight run finished draining")
	default:
	}
	close(release) // let the run complete
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the in-flight run drained")
	}
	if !finished.Load() {
		t.Fatal("in-flight run should have completed under runCtx")
	}
}

// TestRun_SemaphoreBoundsConcurrency: with MaxConcurrent=2, no more than 2
// dispatches run at once.
func TestRun_SemaphoreBoundsConcurrency(t *testing.T) {
	var remaining atomic.Int32
	remaining.Store(6)
	srv := claimController(t, "k8s-1", &remaining)
	client := agentlib.NewClient(srv.URL, "tok")
	a := newK8sAgentForTest(t, Config{AgentID: "k8s-1", Namespace: "ns", MaxConcurrent: 2}, client)

	var cur, max atomic.Int32
	var mu sync.Mutex
	a.dispatch = func(ctx context.Context, c api.ClaimResponse) {
		n := cur.Add(1)
		mu.Lock()
		if n > max.Load() {
			max.Store(n)
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		cur.Add(-1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { _ = a.Run(ctx); close(runDone) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-runDone

	if max.Load() > 2 {
		t.Fatalf("MaxConcurrent=2 must bound concurrency, observed %d", max.Load())
	}
	if max.Load() == 0 {
		t.Fatal("expected some dispatches to run")
	}
}
```

Add a `newK8sAgentForTest` helper in `drain_test.go` that constructs a `*K8sAgent` whose `pool`/`pm` are backed by `k8s.io/client-go/kubernetes/fake.NewSimpleClientset()` so `pool.Restore` and `runPodGC` are safe no-ops without a cluster:

```go
func newK8sAgentForTest(t *testing.T, cfg Config, client *agentlib.Client) *K8sAgent {
	t.Helper()
	fakeCS := fake.NewSimpleClientset()
	pm := NewPodManager(fakeCS, cfg.Namespace, "img")
	pool := NewPodPool(fakeCS, cfg.Namespace, pm)
	a := NewK8sAgent(cfg, client, pm, nil, pool)
	return a
}
```

Import `"k8s.io/client-go/kubernetes/fake"`. `NewPodManager`, `NewPodPool`, and `NewExecutor` all already accept `kubernetes.Interface` (verified), which `fake.NewSimpleClientset()` implements — so this compiles as written with no constructor changes. Over the fake clientset, `pool.Restore` lists zero pods and `runPodGC` finds none, so both are safe no-ops.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/k8sagent/ -run 'TestRun_DrainWaitsForInflight|TestRun_SemaphoreBoundsConcurrency' -count=1`
Expected: FAIL — today `Run` has no drain (returns as soon as ctx cancels, abandoning in-flight) and no semaphore.

- [ ] **Step 3: Restructure `Run`**

In `internal/k8sagent/agent.go`, add imports `"sync"` (and keep `"context"`, `"time"`). Replace the body of `Run` from the heartbeat/GC/claim-loop section onward (currently `agent.go:111-137`) with:

```go
	// ctx is the claim context: cancelled on shutdown to stop new claims. runCtx
	// outlives it so in-flight runs can drain; DrainTimeout (0 = wait forever)
	// bounds the drain window. Mirrors the host agent (internal/agent/agent.go).
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	if d := a.cfg.DrainTimeoutDuration(); d > 0 {
		go func() {
			<-ctx.Done()
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
				runCancel()
			case <-runCtx.Done():
			}
		}()
	}

	// Heartbeat bound to runCtx (not ctx): a drain must not stop heartbeats, or
	// the stuck-run reaper would fail a healthy draining run after staleAfter.
	// Joined before Run returns so no beat outlives Run.
	hbDone := agentlib.StartHeartbeat(runCtx, a.client, a.cfg.AgentID, agentlib.DefaultHeartbeatInterval)
	go a.runPodGC(runCtx, time.Minute)

	// Concurrency gate: positive MaxConcurrent -> semaphore of that size;
	// negative -> unlimited (nil sem, dispatch ungated). Validate mapped 0->100.
	var sem chan struct{}
	if a.cfg.MaxConcurrent > 0 {
		sem = make(chan struct{}, a.cfg.MaxConcurrent)
	}

	var wg sync.WaitGroup
claimLoop:
	for {
		if ctx.Err() != nil {
			break
		}
		if sem != nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				break claimLoop
			}
		}
		resp, err := a.client.Claim(ctx, a.cfg.AgentID, "30s", labels)
		if err != nil {
			if sem != nil {
				<-sem
			}
			slog.Error("claim error", "error", err)
			select {
			case <-ctx.Done():
				break claimLoop
			case <-time.After(2 * time.Second):
			}
			continue
		}
		if resp.RunID == "" {
			if sem != nil {
				<-sem
			}
			continue
		}
		wg.Add(1)
		go func(c api.ClaimResponse) {
			defer wg.Done()
			if sem != nil {
				defer func() { <-sem }()
			}
			a.dispatch(runCtx, c)
		}(resp)
	}

	// Stop claiming; wait for in-flight runs to drain (bounded by DrainTimeout),
	// then stop and join the heartbeat before returning.
	wg.Wait()
	runCancel()
	<-hbDone

	// ctx is cancelled; deregister on a fresh context so the master drops us
	// immediately instead of waiting for heartbeat staleness.
	deregCtx, deregCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer deregCancel()
	if err := a.client.Deregister(deregCtx, a.cfg.AgentID); err != nil {
		slog.Warn("k8s: deregister failed", "agentId", a.cfg.AgentID, "error", err)
	} else {
		slog.Info("k8s agent deregistered", "agentId", a.cfg.AgentID)
	}
	return ctx.Err()
```

Delete the now-obsolete comment block that claimed "The k8s agent has no drain/cordon" (currently `agent.go:111-114`) and the old single-`ctx` heartbeat/GC/loop it described. Keep the `Register` → `pool.Restore` → reconcile-orphans prologue (currently `agent.go:74-109`) unchanged — it runs on `ctx` (claim-time) as before.

- [ ] **Step 4: Run the drain/concurrency tests**

Run: `go test ./internal/k8sagent/ -run 'TestRun_DrainWaitsForInflight|TestRun_SemaphoreBoundsConcurrency' -race -count=1`
Expected: PASS.

- [ ] **Step 5: Run the whole k8s package with the race detector**

Run: `go test ./internal/k8sagent/ -race -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/drain_test.go
git commit -m "feat(k8s): graceful drain + bounded/unlimited maxConcurrent in Run (G-A3a/c)"
```

---

### Task 6: Docs + final build/vet/test sweep

**Files:**
- Modify: the k8s agent config/ops doc (find it: `grep -rl "maxConcurrent\|k8s-agent\|UNIFIED_K8S" docs/`; likely under `docs/` — e.g. a k8s agent deployment/config page). If no dedicated page exists, add a short "k8s agent resilience & concurrency" section to the most relevant existing k8s doc.

- [ ] **Step 1: Locate the doc**

Run: `grep -rln "maxConcurrent\|UNIFIED_K8S\|k8s-agent" docs/`
Pick the k8s-agent config/ops page. Read it to match its heading style and table format.

- [ ] **Step 2: Document the new knobs and behavior**

Add or update a section covering:
- `podStartTimeout` (yaml) / `UNIFIED_K8S_POD_START_TIMEOUT` (env) — default `5m`; bounds how long the agent waits for a run pod to become Running before failing the run. Prevents an unschedulable / `ImagePullBackOff` pod from wedging a run forever.
- `drainTimeout` (yaml) / `UNIFIED_K8S_DRAIN_TIMEOUT` (env) — default `0` (wait indefinitely). On SIGTERM/rollout the agent stops claiming but lets in-flight runs finish for up to this long (heartbeats continue during drain so runs aren't reaped). Parity with the host agent's drain.
- `maxConcurrent` — **default now `100`** (was 5). `0`/unset → `100`; a **negative value → unlimited** (no cap; bounded only by cluster scheduling); positive → that many concurrent runs/pods. Previously parsed but not enforced — it is now enforced.

- [ ] **Step 3: Full sweep**

Run: `gofmt -l internal/k8sagent/ internal/agent/`
Expected: no files listed (all formatted).

Run: `go build ./... && go vet ./internal/k8sagent/ ./internal/agent/ ./cmd/k8s-agent/`
Expected: clean.

Run: `go test ./internal/k8sagent/ ./internal/agent/ -race -count=1`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add docs/
git commit -m "docs: k8s agent podStartTimeout/drainTimeout/maxConcurrent resilience knobs"
```

---

## Notes for the executor

- **Verify route patterns before running Task 5 tests.** The fake controller handlers in `drain_test.go` MUST match the exact HTTP method+path the `agentlib.Client` calls (`Register`, `Claim`, heartbeat, `ReconcileRuns`, `Deregister`) and the JSON shapes it decodes. Read `internal/agent/client.go` and copy the real routes; the illustrative handlers in the plan are a starting point, not gospel.
- **`isTerminalRunStatus` lives in package `k8sagent`, not `agent`.** Task 4 must add a local copy in package `agent` (do NOT import k8sagent from agent — that is a dependency inversion / cycle risk). This duplication is consistent with the existing copies in `cli/wait.go` and `controller/sse.go`.
- **Do not touch the host agent** (`internal/agent/agent.go`) except the shared `orchestrator.go` poller in Task 4 — that file is shared by both agents, and the G-A2 change is intended for both.
- **Task ordering matters:** Task 2 (`failRun`) is consumed by Task 3 (wait failure path). Task 1 (config helpers) is consumed by Tasks 3 and 5. Execute in order.
