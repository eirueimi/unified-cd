# Concurrent Step Execution on the Kubernetes Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run `matrix:`/`foreach:` combinations and `parallel:` groups concurrently on the Kubernetes agent, as they already run on the standard agent, removing the first entry from the documented list of intentional backend differences.

**Architecture:** `ConcurrencyMode` returns `Sequential` on the Kubernetes backend because its scope-pod map is not concurrency-safe. An audit found that map is the entire unguarded surface — the hook stack the code also blames lives in the shared orchestrator and was guarded there long ago. Replace the map's check-then-act with a per-key in-flight entry, then flip the mode.

**Tech Stack:** Go, `sync.Once`/`sync.Mutex`, testify, the build-tagged Kubernetes integration suite (kind).

**Spec:** [`docs/superpowers/specs/2026-08-25-k8s-concurrent-step-execution-design.md`](../specs/2026-08-25-k8s-concurrent-step-execution-design.md)

## Global Constraints

- Do not touch the standard agent. It is already concurrent; this aligns Kubernetes to it.
- Do not touch the shared orchestrator's concurrency handling — `runParallel`, `postHooksMu`, and the hook-stack drain already support concurrent mode.
- A failed scope-pod creation is **not** cached: the entry is removed so a later step makes its own attempt, while the waiters on the failed attempt still receive its error.
- Members share the run Pod and its workspace, exactly as they do on the standard agent. Giving each member its own Pod is out of scope.
- `go build ./...` and `go test ./... -short -count=1` pass at the end of every task.
- Commit messages follow Conventional Commits.

---

## Task 1: Make scope-pod creation concurrency-safe

Behaviour does not change in this task — `ConcurrencyMode` still returns `Sequential`. This is the safety work, landed and provable on its own.

**Files:**
- Modify: `internal/k8sagent/backend.go` (the `k8sBackend` struct, `newK8sBackend`, `CloseScopes`, `ensureScopePod`)
- Modify: `internal/k8sagent/fakepm_test.go` (make the shared test double concurrency-safe and count creations)
- Create: `internal/k8sagent/backend_scope_race_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `scopeEntry` and `k8sBackend.scopes map[string]*scopeEntry` replacing `scopePods map[string]string`; `createScopePod(ctx, step, env) (string, error)` holding the old create-and-wait body. Task 2 relies on this being safe but calls nothing here directly.

- [ ] **Step 1: Make the shared fake pod manager concurrency-safe**

`internal/k8sagent/fakepm_test.go`'s `fakePM` is used across the package's suites and writes its fields without synchronization (`f.created = pod`, `f.waitHadDeadline = hasDeadline`). A test that drives it concurrently would report a race in the double rather than in the code under test.

Add to the struct, keeping every existing field:

```go
	mu          sync.Mutex
	createCount int
```

Guard the writes in `CreatePod`:

```go
func (f *fakePM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.mu.Lock()
	f.created = pod
	f.createCount++
	out := pod.DeepCopy()
	out.Name = "ucd-img-generated-xyz" // simulate server-assigned name from GenerateName
	f.createdNm = out.Name
	f.mu.Unlock()
	return out, nil
}
```

and the writes at the top of `WaitForPodRunning`:

```go
func (f *fakePM) WaitForPodRunning(ctx context.Context, _ string) error {
	deadline, hasDeadline := ctx.Deadline()
	f.mu.Lock()
	f.waitCtxSeen = true
	f.waitHadDeadline = hasDeadline
	f.waitDeadline = deadline
	waitBlock := f.waitBlock
	waitErr := f.waitErr
	f.mu.Unlock()

	if waitBlock != nil {
		select {
		case <-waitBlock:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return waitErr
}
```

Note the block happens **after** the unlock — holding the mutex across a blocking wait would serialise the very concurrency the race test needs to create.

Add a reader for the counter:

```go
func (f *fakePM) creations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createCount
}
```

Leave `DeletePod` and every other method as they are; existing tests read `pm.created` directly after a sequential call and continue to work.

- [ ] **Step 2: Write the failing race test**

Create `internal/k8sagent/backend_scope_race_test.go`:

```go
package k8sagent

import (
	"context"
	"sync"
	"testing"

	agentlib "github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestK8sBackend_EnsureScope_SameKeyCreatesOnePod drives EnsureScope
// concurrently for one scope key, which is what a parallel: group or a matrix
// whose members share a uses-scope does once the backend reports Concurrent.
//
// Two properties are asserted, and they fail for different reasons:
//
//   - Under -race, the old map[string]string version reports a data race on
//     b.scopePods — concurrent read and write of a plain map.
//   - Even without -race, the old check-then-act let both goroutines miss the
//     cache and each create a pod. One won the map entry; the other's pod was
//     orphaned, because CloseScopes only deletes pods that made it into the
//     map. The creation count is what catches that.
func TestK8sBackend_EnsureScope_SameKeyCreatesOnePod(t *testing.T) {
	pm := &fakePM{}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	const goroutines = 8
	var wg sync.WaitGroup
	payloads := make([]any, goroutines)
	errs := make([]error, goroutines)
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			h, err := b.EnsureScope(context.Background(), step, nil)
			payloads[i], _ = agentlib.ScopeHandlePayload(h)
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range goroutines {
		require.NoError(t, errs[i], "goroutine %d", i)
	}
	assert.Equal(t, 1, pm.creations(), "one scope key must produce exactly one pod, however many steps ask for it at once")
	for i := range goroutines {
		assert.Equal(t, payloads[0], payloads[i], "every caller for one key must get the same scope pod")
	}
}

// TestK8sBackend_EnsureScope_DifferentKeysDoNotSerialize proves the in-flight
// entry is per key rather than one lock around the whole function. The fake
// blocks the first WaitForPodRunning until released; if scope creation were
// serialised under a single mutex, the second key's request would block behind
// it and this test would deadlock until the test timeout.
func TestK8sBackend_EnsureScope_DifferentKeysDoNotSerialize(t *testing.T) {
	pm := &fakePM{waitBlock: make(chan struct{})}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = b.EnsureScope(context.Background(), api.ClaimStep{ScopeID: "scope:a", ScopeImage: "golang:1.22"}, nil)
	}()

	second := make(chan error, 1)
	go func() {
		_, err := b.EnsureScope(context.Background(), api.ClaimStep{ScopeID: "scope:b", ScopeImage: "node:22"}, nil)
		second <- err
	}()

	// The second key must complete while the first is still blocked in its
	// pod wait. Releasing the block only afterwards proves it was not queued
	// behind the first.
	require.NoError(t, <-second, "a second scope key must not wait on the first key's pod")

	close(pm.waitBlock)
	<-first
}

// TestK8sBackend_EnsureScope_FailureIsNotCached proves a failed creation does
// not poison the key for the rest of the claim: the entry is dropped so a
// later step retries, rather than inheriting an error it did not cause. Scope
// pods fail for reasons that are frequently transient — image pull, quota, a
// node filling up.
func TestK8sBackend_EnsureScope_FailureIsNotCached(t *testing.T) {
	pm := &fakePM{waitErr: assert.AnError}
	a := &K8sAgent{cfg: Config{Namespace: "default"}, pm: pm}
	b := newK8sBackend(a, "run-1", "test-job", "pod-default", "/workspace", nil, metav1.Time{})

	step := api.ClaimStep{ScopeID: "scope:build", ScopeImage: "golang:1.22"}

	_, err := b.EnsureScope(context.Background(), step, nil)
	require.Error(t, err, "the fake's wait error must surface")

	pm.mu.Lock()
	pm.waitErr = nil
	pm.mu.Unlock()

	_, err = b.EnsureScope(context.Background(), step, nil)
	require.NoError(t, err, "a later step must get its own attempt, not the cached failure")
	assert.Equal(t, 2, pm.creations(), "the retry must actually create a pod rather than return a cached entry")
}
```

`ScopeHandlePayload` is a package function, not a method: `func ScopeHandlePayload(h ScopeHandle) (v any, ok bool)` (`internal/agent/backend.go:120`). The package is imported in this package as `agentlib` (see `internal/k8sagent/backend.go:12`). The payload is the scope pod's name as an `any`; comparing the values directly is enough here, so the test does not assert on its dynamic type.

- [ ] **Step 3: Run the tests and watch them fail**

Run: `go test ./internal/k8sagent/ -run 'EnsureScope_(SameKey|DifferentKeys|FailureIsNotCached)' -race -count=1 -v`

Expected: FAIL. `SameKeyCreatesOnePod` reports a data race on `b.scopePods` and a creation count of more than 1. `FailureIsNotCached` fails on the creation count or the second error, depending on ordering. `DifferentKeysDoNotSerialize` passes today — there is no lock yet — and is there to stop the fix from introducing one.

- [ ] **Step 4: Replace the map with per-key in-flight entries**

In `internal/k8sagent/backend.go`, add above `k8sBackend`:

```go
// scopeEntry is one scope key's in-flight or completed pod creation. The Once
// makes concurrent callers for the same key share a single attempt instead of
// each creating a pod — the old check-then-act let two steps both miss the
// cache, and the loser's pod was orphaned, since CloseScopes only deletes
// pods that made it into the map.
//
// name and err are written inside the Once and read by CloseScopes, which
// never calls Do and so has no happens-before edge from it. Both accesses are
// therefore taken under k8sBackend.scopesMu.
type scopeEntry struct {
	once sync.Once
	name string
	err  error
}
```

Replace the struct field:

```go
	scopesMu sync.Mutex
	scopes   map[string]*scopeEntry
```

and its initialiser in `newK8sBackend` (`scopePods: map[string]string{}` becomes `scopes: map[string]*scopeEntry{}`).

Split `ensureScopePod` in two. The existing body from `envMap := imageStepEnv(step)` through the `WaitForPodRunning` error handling moves verbatim into:

```go
// createScopePod creates one scope pod and waits for it to be Running. It is
// called at most once per scope key, from inside scopeEntry.once.
func (b *k8sBackend) createScopePod(ctx context.Context, step api.ClaimStep, env []string) (string, error) {
```

returning `name, nil` at the end instead of writing to the map. Keep every existing comment in it — the timeout rationale and the expanded-env note are still true and are the only record of why.

`ensureScopePod` becomes the coordination:

```go
func (b *k8sBackend) ensureScopePod(ctx context.Context, step api.ClaimStep, env []string) (string, error) {
	key := scopeKey(step)

	b.scopesMu.Lock()
	e, ok := b.scopes[key]
	if !ok {
		e = &scopeEntry{}
		b.scopes[key] = e
	}
	b.scopesMu.Unlock()

	e.once.Do(func() {
		name, err := b.createScopePod(ctx, step, env)
		b.scopesMu.Lock()
		e.name, e.err = name, err
		if err != nil {
			// Do not cache a failure. A later step needing this scope makes
			// its own attempt rather than inheriting an error it did not
			// cause; the callers waiting on THIS attempt still receive err
			// below, because they hold the entry pointer.
			delete(b.scopes, key)
		}
		b.scopesMu.Unlock()
	})

	b.scopesMu.Lock()
	name, err := e.name, e.err
	b.scopesMu.Unlock()
	return name, err
}
```

And `CloseScopes` iterates entries instead of names, skipping the ones that never produced a pod:

```go
	b.scopesMu.Lock()
	entries := make(map[string]string, len(b.scopes))
	for key, e := range b.scopes {
		if e.err == nil && e.name != "" {
			entries[key] = e.name
		}
	}
	b.scopesMu.Unlock()

	for key, name := range entries {
		if err := b.a.pm.DeletePod(context.WithoutCancel(ctx), name); err != nil {
			slog.Warn("k8s: failed to delete scope pod", "scopeKey", key, "pod", name, "error", err)
		}
	}
```

Keep the existing sidecar-pump stop at the top of `CloseScopes` unchanged, and its comment.

Add `"sync"` to the file's imports.

- [ ] **Step 5: Run the tests and watch them pass**

Run: `go test ./internal/k8sagent/ -race -count=1`

Expected: PASS, the whole package. The three new tests pass and every existing scope test still passes — `backend_ensurescope_test.go` drives the same path and must be unaffected.

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/backend.go internal/k8sagent/fakepm_test.go internal/k8sagent/backend_scope_race_test.go
git commit -m "fix(k8sagent): make scope-pod creation concurrency-safe

The scope map was a plain map with a check-then-act: two steps missing the
cache for one key each created a pod, and the loser's pod was orphaned
because CloseScopes only deletes what reached the map.

A per-key in-flight entry replaces it. Callers for one key share a single
attempt; different keys still proceed in parallel, so scope startup is not
serialised behind one lock held across a pod's Ready wait. A failed attempt
is not cached, so a later step retries rather than inheriting an error it
did not cause.

Behaviour is unchanged: ConcurrencyMode still reports Sequential."
```

---

## Task 2: Report Concurrent, and prove parity

**Files:**
- Modify: `internal/k8sagent/backend.go` (`ConcurrencyMode` and its comment)
- Modify: `internal/paritycases/scenarios.go` (add one case)

**Interfaces:**
- Consumes: the concurrency-safe scope map from Task 1.
- Produces: a parity `Case` both backends must satisfy.

- [ ] **Step 1: Add the parity scenario**

`internal/paritycases/scenarios.go` holds one function per scenario returning a `Case` (`internal/paritycases/paritycases.go:66-76`: `Name`, `Claim func() api.ClaimResponse`, `Secrets`, `Expect`). Read `ifSkipsStep` at the top of the file for the house shape, then add:

```go
// parallelGroupMembersAllSucceed pins the behaviour that must not diverge once
// the k8s backend reports Concurrent: every member of a parallel: group runs
// and reports its own terminal status, on both backends.
//
// It deliberately asserts nothing about ORDER or timing. Both backends now run
// members concurrently, so any assertion about which finished first would be a
// flaky test wearing a parity check's clothes. Each member writes to a distinct
// path so the case says nothing about workspace contention either — that is a
// property of the DSL contract, not of backend parity.
func parallelGroupMembersAllSucceed() Case {
	return Case{
		Name: "parallel-group-members-all-succeed",
		Claim: func() api.ClaimResponse {
			return api.ClaimResponse{
				RunID:   "run-parallel-group-members",
				JobName: "parallel-group-members",
				Native:  true,
				Stages: []api.ClaimStage{
					{Parallel: []api.ClaimStep{
						{Index: 0, StageIndex: 0, Name: "alpha", Run: "echo alpha > alpha.txt"},
						{Index: 1, StageIndex: 0, Name: "beta", Run: "echo beta > beta.txt"},
						{Index: 2, StageIndex: 0, Name: "gamma", Run: "echo gamma > gamma.txt"},
					}},
				},
			}
		},
		Expect: Expectation{
			StepStatus: map[string]string{
				"alpha": "Succeeded",
				"beta":  "Succeeded",
				"gamma": "Succeeded",
			},
			RunFinished: "Succeeded",
		},
	}
}
```

The stage field is `Parallel []ClaimStep` (`internal/api/types.go:112-115`), so the literal above is correct as written. Register the new function wherever the file collects its cases, alongside the others.

- [ ] **Step 2: Run the parity suite on both backends and watch it pass under Sequential**

Run: `go test ./internal/agent/ ./internal/k8sagent/ -run Parity -count=1`

Expected: PASS on both. The case must pass *before* the mode flips — a parity case that only passes under concurrency would be testing the flip rather than the contract.

- [ ] **Step 3: Flip the mode and correct the comment**

In `internal/k8sagent/backend.go`, replace `ConcurrencyMode` and the comment above it:

```go
// ConcurrencyMode reports how the k8s agent runs parallel-group / matrix
// members: concurrently, matching the standard agent.
//
// This returned Sequential until scope-pod creation became concurrency-safe.
// The old comment also blamed the hook stack, which was already wrong: the
// hook stack and postHooks live in the shared orchestrator
// (internal/agent/orchestrator.go) and have been guarded by postHooksMu since
// the standard agent went concurrent. Everything else the backend holds is
// either written once before the step loop (masker, sidecarPump, via
// SetMasker) or allocated per call (StepLogWriters), and the pod pool carries
// its own mutex.
func (b *k8sBackend) ConcurrencyMode() agentlib.ConcurrencyMode {
	return agentlib.Concurrent
}
```

- [ ] **Step 4: Run the whole suite under the race detector**

Run: `go test ./internal/agent/ ./internal/k8sagent/ ./internal/paritycases/ -race -count=1`

Expected: PASS. This is the first run where the k8s backend's step loop is actually concurrent, so a race anywhere in the backend surfaces here rather than in production.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/backend.go internal/paritycases/scenarios.go
git commit -m "feat(k8sagent): run parallel and matrix members concurrently

Matches the standard agent. The Sequential mode was an implementation
concession to an unsafe scope map, not a property of the Kubernetes
execution model — concurrent execs into one Pod are ordinary.

Its comment also blamed the hook stack, which had moved into the shared
orchestrator and been guarded there long before."
```

---

## Task 3: Exercise it against a real Pod

The unit tests prove the scope map is safe and the parity case pins the contract. Neither runs steps concurrently against a real Kubernetes Pod, which is the thing this change is for.

**Files:**
- Modify: `internal/k8sagent/agent_integration_test.go` (carries `//go:build k8s`; its helpers live in `internal/k8sagent/testutil_k8s_test.go`)

**Interfaces:**
- Consumes: the Concurrent mode from Task 2.
- Produces: nothing later depends on.

- [ ] **Step 1: Read the build-tagged suite and its fixtures**

Six files carry `//go:build k8s`: `agent_integration_test.go`, `artifact_k8s_test.go`, `cache_k8s_test.go`, `executor_integration_test.go`, `podmanager_integration_test.go`, and `testutil_k8s_test.go`, which holds the shared helpers. The new test belongs in `agent_integration_test.go`, beside the other claim-level tests.

Read one existing test there end to end before writing. It builds real Pods against a kind cluster (CI starts one — see `.github/workflows/ci.yml`'s `k8s` job), so it has setup helpers you must reuse rather than reinvent.

- [ ] **Step 2: Add a concurrent-members test**

Add a test to that suite that runs a claim with a `parallel:` group of three steps against a real Pod, and asserts all three report `Succeeded` and the run finishes `Succeeded`.

Assert nothing about ordering. What this test is for is the machinery underneath: three concurrent execs into one Pod, three sets of step log writers with their own auto-flush timers, and one sidecar log pump beneath all of it. Those were read but never observed under concurrency, and this is the only place they run for real.

If the suite has a helper that asserts a claim's step statuses, use it; the point is the execution, not a new assertion style.

- [ ] **Step 3: Run it against a cluster**

Run: `go test -tags k8s ./internal/k8sagent/... -run Concurrent -timeout 10m -v`

Expected: PASS with a reachable cluster. If no cluster is available in your environment, say so plainly in your report — do not claim a result you did not observe. CI runs this suite on every pull request, so the check is not lost either way.

- [ ] **Step 4: Commit**

```bash
git add internal/k8sagent/
git commit -m "test(k8sagent): run a parallel group against a real Pod

The unit tests cover the scope map and the parity case pins the contract,
but neither runs steps concurrently against a real Pod. This exercises the
machinery that was audited by reading and never observed: concurrent execs
into one Pod, per-step log writers with their own flush timers, and the
single sidecar pump beneath them."
```

---

## Task 4: Documentation and migration guide

**Files:**
- Modify: `docs/operator-manual/kubernetes-integration.md` (remove the execution-order difference)
- Modify: `docs/user-guide/writing-jobs/isolation-and-containers.md` (the podTemplate parity notes, if they repeat the claim)
- Create: `docs/operator-manual/migrations/k8s-concurrent-step-execution.md`
- Modify: `mkdocs.yml` (nav entry under Operator Manual → Migrations)

**Interfaces:**
- Consumes: the behaviour from Task 2.
- Produces: nothing.

- [ ] **Step 1: Remove the difference from the canonical list**

`docs/operator-manual/kubernetes-integration.md` opens its intentional-differences list with the execution-order entry — `matrix:`/`foreach:` combinations and `parallel:` groups run "**sequentially** inside the Pod (the standard agent runs them in parallel goroutines)". Delete that entry; it is no longer true.

Check the list's introduction still reads correctly with one fewer item, and search the file for any other sentence that repeats the claim.

- [ ] **Step 2: Check the parity notes**

`docs/user-guide/writing-jobs/isolation-and-containers.md` carries the podTemplate parity notes. Search it for execution-order or sequential claims and correct any that survive. If it says nothing about ordering, change nothing and say so in your report — an unnecessary edit to a user-guide page is worth avoiding.

- [ ] **Step 3: Write the migration guide**

Create `docs/operator-manual/migrations/k8s-concurrent-step-execution.md`, matching the structure of `docs/operator-manual/migrations/agent-id-scoped-credentials.md`.

It must carry:

1. **What changed.** Matrix and parallel members on the Kubernetes agent now run concurrently and share the run Pod's workspace, as they already do on the standard agent.

2. **What can break**, concretely rather than abstractly: several members appending to one file, or writing the same path, in the shared workspace. On the standard agent such a job was already racy; on Kubernetes it was accidentally safe, and that accident is what is going away.

3. **Why it was done anyway.** The DSL's contract is that parallel steps share a workspace; the Kubernetes serialisation was an implementation artefact, not a promise. State it plainly rather than defensively.

4. **The fix for an affected job:** `needs:` is the existing way to order steps that must not overlap. Give a short before/after job snippet showing two members that wrote the same path becoming ordered by `needs:`.

- [ ] **Step 4: Add it to the nav and verify the site**

Add the page to `mkdocs.yml` under `Operator Manual` → `Migrations`, then run:

`python -m mkdocs build --strict`

Expected: PASS with zero warnings.

- [ ] **Step 5: Commit**

```bash
git add docs/ mkdocs.yml
git commit -m "docs(k8sagent): concurrent step execution is no longer a backend difference

Removes execution order from the intentional-differences list and adds a
migration guide: a job whose matrix members wrote the same workspace path
was accidentally safe on Kubernetes and is not any more."
```

---

## Final acceptance

Against spec sections 5 and 6:

- [ ] `go test ./internal/k8sagent/ -race -count=1` passes, including the same-key, different-keys, and failure-not-cached tests.
- [ ] The parity case passes on both backends.
- [ ] The build-tagged Kubernetes suite passes with a concurrent-members test present, or its absence is reported with the reason.
- [ ] `go build ./...` and `go test ./... -short -count=1` pass.
- [ ] Execution order no longer appears in the intentional-differences list, and the migration guide is in the nav with `mkdocs build --strict` clean.
