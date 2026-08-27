# How an agent executes a run

`internal/agent` is where a run stops being data and starts being processes. It
is also the package where the most invariants live, and the one whose layout
confuses people first.

This page is a **map, not a specification**. This codebase carries unusually
thorough reasoning in its comments — `DefaultFinallyBudget` in
`orchestrator.go` is sixty lines explaining one number, and it is the right
place for that explanation to live. Where the reasoning is already at the code,
this page points at it rather than copying it, because a copy drifts.

## First, the layout confusion

`internal/agent` contains **two different things**:

- The **backend-independent orchestrator** — `orchestrator.go`, `pipeline.go`,
  `runner.go` — which both agents use.
- The **host backend** — `backend_host.go` — which only the host agent uses.

`internal/k8sagent` contains only the Kubernetes backend, and imports
`internal/agent` for the shared half.

So "where is the shared execution logic?" and "where is the host
implementation?" have the same answer, and that is the thing nobody tells you.
See [Architecture — the ExecBackend seam](architecture.md#the-execbackend-seam)
for why the split is where it is.

## `RunClaim`, from top to bottom

One claim is one call to `RunClaim` in `orchestrator.go`. Reading that function
top to bottom is the fastest way to understand the agent; this is the shape it
has.

```text
  claim arrives
      |
      v
  job deadline applied to ctx        (spec.timeoutMinutes)
      |
      v
  cancellation poller started        (watches for an operator cancel, or the
      |                               controller reaping the run out-of-band)
      v
  teardown defer REGISTERED  <-------- registered early, runs LAST
      |
      v
  masker installed                   (secrets known -> log writers mask)
      |
      v
  ===== main DAG =====               RunPipeline(c.Stages, ...)
      |
      v
  hook drain #1                      post: / cache: hooks from the DAG
      |
      v
  ===== finally: =====               RunPipeline(c.Finally, ...)  <-- second call
      |
      v
  hook drain #2                      hooks registered by finally: steps
      |
      v
  SetRunOutputs / FinishRun          the controller report
      |
      v
  [deferred] CloseScopes             scope containers/Pods, sidecar pump
```

Two orderings in that picture are easy to get backwards and both matter.

**The teardown defer is registered early and therefore runs last.** It sits
before the masker is installed, so that any early return between there and the
pump's start still tears down. It runs from a `defer`, so — independent of
where it sits relative to `RunClaim`'s other defers — it can only execute
after the function body has finished, and that body's last acts are
`SetRunOutputs` and `FinishRun`. So `CloseScopes` executes *after* `FinishRun`,
not before.

**`finally:` is a second, separate `RunPipeline` call.** Anything wired only
into the main path is accepted by the parser and silently does nothing in
`finally:` — no step status, no run status, no log line. This has bitten five
times. See
[Invariants — `finally:` is a second, separate pipeline invocation](invariants.md).

## The four cleanup windows

Everything after the main DAG that *executes* something carries a time ceiling:
hook drain, `finally:`, hook drain, teardown. Four windows, each
`DefaultFinallyBudget` (10 minutes), and on Kubernetes a **fifth** for claim-Pod
teardown.

The controller report is the deliberate exception — it is unbounded, because a
ceiling there would have nothing to hand off to.

**Do not add a post-DAG phase that executes something without a window.** These
numbers are published to operators who size rollout grace periods against them.

The full reasoning — why a ceiling exists at all, why the budget is not
`spec.timeoutMinutes`, why ten minutes, and what deadlocked before it existed —
is at `DefaultFinallyBudget` in `orchestrator.go`. Read it there.

## Where a step actually runs

A step reaches the machine through exactly one of the `ExecBackend` methods:

| Step shape | Method |
|---|---|
| a plain `run:` | `RunDefault` |
| `runsIn: <container>` | `RunNamedContainer` |
| a step inside a `uses:` scope | `EnsureScope` once, then `RunInScope` |

`EnsureScope` is called lazily, on the attempt that needs it — not up front.
That matters for retries: a scope is established inside the attempt loop, so
the code around it in `orchestrator.go` deliberately distinguishes "the handle
we would use" from "the handle we have".

### The shim

Every job container gets a small static Linux binary injected at
`/.ucd/ucd-sh`, embedded into the agent at build time from `internal/shim`.

It exists so a step is interpreted the same way regardless of what the user's
image happens to contain — an image with no `bash`, or a `sh` that is really
`dash`, still runs the step identically. `api.ClaimStep.Shell` carries the
controller-resolved interpreter argv; nil means "apply the shim default".

The embedded binary is selected by the **compiling** `GOARCH`, not the target
OS: a `windows/amd64` agent embeds `ucd-sh-amd64`. It is a committed build
product — see
[Architecture — generated artifacts](architecture.md#generated-artifacts) and
the warning there about regenerating on Linux only.

## Logs, and why the masker constrains flushing

Step output flows: process → `LogPusher` → batched HTTP → controller → Postgres.

`SetMasker` installs the secret masker before any log writer exists, so
everything a step emits is masked on the way out. The masker matches **per
line**, which imposes two rules that are not obvious from reading `LogPusher`
alone:

- **A flush must never split a line.** A value split across two lines matches
  nothing and both halves ship in clear. This is a real leak that shipped; see
  [Invariants — the masker matches per line](invariants.md).
- **Batches must never overtake one another.** The controller assigns `seq` on
  arrival, so a newer batch landing while an older one is queued permanently
  reorders the stored log. `flushPendingLocked` stops at the first failure and
  accepts head-of-line blocking as the price.

If you add a flush trigger, a retry path, or a second writer, those two
properties are what you must preserve.

## Cancellation

An operator cancel reaches the agent through a poller started near the top of
`RunClaim`, which sets `cancelledByMaster` and cancels the run context via
`cancelRun`. The flag is what distinguishes "cancelled" from "failed" — a step
killed by cancellation must not be retried, and the retry loop's
`cancelMasksFailure` check consults it before allowing another attempt.

The same poller also watches for the controller having marked the run
terminal out-of-band (e.g. the stuck-run reaper). That case sets a different
flag, `reapedByMaster`, and still cancels the run context, but `RunClaim`
takes a different exit at the end: it skips `SetRunOutputs`/`FinishRun`
entirely rather than reporting its own status, because the controller's
verdict is already authoritative.

`finally:` still runs, on a context derived with `context.WithoutCancel`. That
is the entire point of the phase, and it is also the source of a trap:
`WithoutCancel` strips the **deadline** along with the cancellation, and the
resulting context's `Done()` is nil, so a `select` on it blocks forever. That is
why the windows above exist.

## Proving both backends agree

`internal/paritycases` holds scenarios as data. Each `Case` builds a fresh
`api.ClaimResponse`, and a driver in `internal/agent` and another in
`internal/k8sagent` run the same case against their own backend.

**When you change behaviour above the seam, add a case.** A test that exercises
only one backend proves nothing about the promise that both behave identically —
and test drivers that quietly diverged from production are how a concurrency
change once passed while doing nothing.

## Reading order for a newcomer

1. `orchestrator.go`'s `RunClaim`, top to bottom, ignoring the details.
2. `DefaultFinallyBudget`'s comment — the best single explanation of why
   cleanup is shaped the way it is.
3. `backend.go` — the `ExecBackend` interface and its contracts.
4. `pipeline.go` — how stages expand into per-combination step copies via
   `ExpandMatrixStep` (`foreach:` compiles down to a single-dimension matrix
   before the agent ever sees it).
5. `runner.go` — `LogPusher`, and the ordering guarantee.
