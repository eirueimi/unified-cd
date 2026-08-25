# Concurrent step execution on the Kubernetes agent — Design

Date: 2026-08-25
Status: Approved (design); implementation plan to follow

## 1. Purpose

`matrix:` and `foreach:` combinations and `parallel:` groups run as concurrent
goroutines on the standard agent and one at a time on the Kubernetes agent. It
is the first entry in the documented list of intentional differences
(`docs/operator-manual/kubernetes-integration.md`).

It is not a consequence of the Kubernetes execution model. Multiple concurrent
execs into one Pod are ordinary. It is an implementation concession, recorded as
one in the code: `ConcurrencyMode` returns `Sequential` because the "scope-pod
map and hook stack are not concurrency-safe"
(`internal/k8sagent/backend.go:424-429`).

Remove the concession and the difference disappears.

## 2. What the audit found

The concern going in was that making the backend concurrent would mean auditing
every piece of state inside it. The audit found the opposite: the surface is one
map.

**Already safe, and not to be touched:**

- **The hook stack is not in the backend.** `hookStack` and `postHooks` live in
  the shared orchestrator (`internal/agent/orchestrator.go:241-243`) and are
  already guarded by `postHooksMu`, added for the standard agent's concurrent
  mode. The comment there states it directly: "Under Sequential mode (k8s) the
  lock is uncontended but still correct." The `ConcurrencyMode` comment naming
  the hook stack is stale.
- **`masker` and `sidecarPump`** are written once by `SetMasker`
  (`internal/k8sagent/backend.go:383,392`), which the orchestrator calls at
  `orchestrator.go:213`, before `RunPipeline`. After that they are read-only for
  the claim's lifetime.
- **`CloseScopes`** is deferred at `orchestrator.go:209` and runs after
  `RunPipeline` returns, so its iteration over the scope map cannot overlap the
  step loop.
- **`StepLogWriters`** (`internal/k8sagent/backend.go:406-422`) allocates a fresh
  pusher and writer per call and shares only the read-only `masker`.
- **`PodPool`** carries its own mutex (`internal/k8sagent/pool.go:47`).

**The whole remaining surface:** `scopePods map[string]string`, read at
`internal/k8sagent/backend.go:153` and written at `:176`, both inside
`ensureScopePod`.

## 3. The change

### 3.1 Make scope-pod creation concurrency-safe

`ensureScopePod` is a check-then-act:

```go
if name, ok := b.scopePods[key]; ok {
	return name, nil
}
// ... create pod, wait for Ready ...
b.scopePods[key] = name
```

Under concurrency this has two failure modes, and a plain mutex fixes only one
of them well:

- **Concurrent map access.** Two goroutines touching the map at once.
- **Duplicate creation.** Two goroutines missing the cache for the same key both
  create a scope pod. One wins the map entry; the other's pod is orphaned —
  `CloseScopes` only deletes pods that made it into the map, so it leaks until
  the namespace is cleaned up.

Wrapping the whole function in one mutex fixes both but serialises scope
startup: the lock would be held across `CreatePod` and `WaitForPodRunning`,
which is bounded by `PodStartTimeout` (`backend.go:167-172`). Two parallel steps
needing *different* scope images would start their pods one after the other,
which is most of the latency the concurrency was meant to remove.

Use a per-key inflight entry instead. The backend mutex guards only the map
lookup and insert; the create-and-wait happens outside it, once per key, with
other goroutines for the same key waiting on that single attempt and receiving
its result. Two different keys proceed in parallel; two goroutines on the same
key produce one pod.

**A failed creation is not cached.** The entry is removed when the attempt
fails, so a later step needing the same scope makes its own attempt rather than
inheriting a failure it did not cause. The waiters on that attempt still receive
its error — they asked for the same thing and it did not work — but the claim is
not poisoned for whatever comes after. Scope-pod creation fails for reasons that
are frequently transient (image pull, quota, a node filling up), and the
alternative caches a wrong answer for the rest of the run. This must be stated
in a comment; a caching decision reads as arbitrary six months later.

### 3.2 Switch the mode and correct the comment

`ConcurrencyMode` returns `Concurrent`. Its comment currently justifies
`Sequential` by naming the hook stack, which has since been made safe elsewhere.
The replacement should say what is now true and where the safety comes from, so
the next reader does not re-derive the audit in §2.

## 4. What changes for users

Matrix and parallel members on the Kubernetes agent begin running concurrently,
sharing the run Pod and its workspace — exactly as they already do on the
standard agent.

**A job that implicitly relied on Kubernetes serialisation can break.** The
clearest case is several matrix members appending to one file in the workspace,
or writing the same path. On the standard agent that job is already racy; on
Kubernetes it has been accidentally safe.

This was decided deliberately: the DSL's contract is that parallel steps share a
workspace, and the Kubernetes serialisation was an implementation artefact, not
a promise. Aligning to the host is the direction of the whole `ExecBackend`
seam. The alternative — introducing a way to request serialisation before
changing the default — was rejected as a larger change that adds DSL surface to
protect against a contract nobody documented.

A migration guide records it: what changes, which job shapes are at risk, and
how to order steps that must not overlap.

**Correction (Task 4).** An earlier revision of this section named `needs:` as
that mechanism. It is not one: `needs:` was removed from the DSL and `apply`
rejects it outright at any nesting level, with "needs: is no longer supported —
use parallel: blocks for concurrent execution" (`internal/dsl/parse.go:136-147`).
The claim was wrong when written and would have sent operators to a keyword that
fails at apply time.

The DSL's only step-ordering primitive is **declaration order**: steps under
`steps:` run one at a time in the order listed, and `parallel:` is what opts a
group into running concurrently. Ordering two writes that must not overlap means
taking them out of concurrency, not annotating a dependency between them. For
`matrix:`/`foreach:` there is no equivalent move — the combinations are
expansions of one step and run as a single concurrent set — so the fix there is
to give each combination its own path, parameterized by the dimension.

## 5. Testing

Two layers, because they catch different things.

**Race coverage in `internal/k8sagent`.** A test that drives `EnsureScope`
concurrently and runs under `-race`:

- Several goroutines requesting the *same* scope key must produce exactly one
  pod — assert on the fake pod manager's creation count, not just on the
  returned handles being equal.
- Goroutines requesting *different* keys must each get their own pod.
- The map itself must survive `-race`, which is the part that fails today.

**Parity coverage in `internal/paritycases`.** The suite runs one `Case`
(`internal/paritycases/paritycases.go:66-76`) against both backends and asserts
the same `Expectation`. A concurrency scenario must assert something true of
both backends without being timing-dependent — step statuses and the run's
terminal status, not wall-clock ordering. A `parallel:` group whose members each
touch a distinct workspace path and all report `Succeeded` is the shape that
works; anything asserting *which* member finished first would be a flaky test
dressed up as a parity check.

Note what parity cannot prove here: both backends running members concurrently
is not observable from `StepStatus` alone. The parity case pins the behaviour
that must not diverge; the race test is what shows the k8s side is actually
concurrent.

## 6. Unverified risk

Concurrent exec streams into one Pod are fine at the Kubernetes API level, and
`PodPool` is already mutex-guarded. Two things were read but not exercised:

- The **sidecar log pump** (`k8sSidecarPump`, started in `SetMasker`) is a single
  per-claim pump, so it is structurally unaffected by step concurrency — but it
  has never run beneath concurrent steps.
- The **stderr auto-flush** started per step in `StepLogWriters` allocates its
  own context and pusher per call, so several running at once should be
  independent — again, read rather than observed.

The race test covers scope creation, not these. Exercising them means running
the build-tagged Kubernetes integration suite
(`go test -tags k8s ./internal/k8sagent/...`, which CI runs against a kind
cluster) with a job whose parallel members actually execute at once.

**That is in scope.** A change whose entire purpose is to run steps
concurrently, verified only by tests that never run steps concurrently against
a real Pod, would be verified in name. The suite already builds real Pods and
runs in CI, so the cost is one scenario, not new infrastructure.

**Outcome (Task 3).** `TestK8sAgent_ExecuteRun_ParallelMembersRunConcurrently_Integration`
(`internal/k8sagent/agent_integration_test.go`) shipped one of the two. The
**stderr auto-flush** is exercised for real: the test shrinks
`stderrAutoFlushInterval` and gives each member's script a runtime floor, so
the ticker is guaranteed to fire multiple times mid-step, under concurrent
load, across three independent `StepLogWriters` instances — not just via the
unconditional final flush every step does at its own end regardless.

The **sidecar log pump** is not exercised, and on inspection that is fine to
leave as is rather than a gap to close later. `k8sBackend.SetMasker` builds
the pump once per claim, before the step loop runs at all; each user
sidecar's `LogPusher` writes under `dsl.SidecarLogIndex`, an index space
distinct from any step's index. Concurrent parallel members share no
per-step state with it — the earlier "structurally unaffected by step
concurrency" guess above holds. A scenario built to reach the pump would need
a claim-level test with a real sidecar container (and the image-pull
dependency that brings), not a parallel-group test; nothing about running
steps concurrently gives that test more reason to exist than it already had.

## 7. Out of scope

- **Any change to the standard agent.** It is already concurrent; this aligns
  Kubernetes to it.
- **The orchestrator's concurrency handling.** `runParallel`, `postHooksMu`, and
  the hook-stack drain are shared code that already supports concurrent mode.
- **Pod-level parallelism.** Members share the run Pod, as they do today; giving
  each member its own Pod is a different design with different cost.
- **Removing `ConcurrencyMode` from the `ExecBackend` interface.** Once both
  backends return `Concurrent` the seam looks redundant, but it is the
  documented place where a future backend declares this, and deleting it is a
  separate decision.
