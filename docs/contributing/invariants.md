# Invariants

Rules in this codebase that are load-bearing, easy to break by accident, and
whose violation is **silent** — no error, and often a *success* report.

Each entry says what the rule is, why it exists, and what actually broke when
it was violated. The "what broke" is the useful part: it is how you recognise
the same mistake in a new shape.

Read this before changing agent or controller internals. It is not exhaustive —
add to it when you find the next one.

---

## Execution semantics

### An error in an `if:` condition means the step RUNS

`dsl.EvalCondition` returns `(true, nil, err)` on a compile or evaluation
error. The fail-safe is deliberate — a broken expression must not silently skip
work — but it has a sharp consequence: **anything that makes a condition
*error* turns a closed gate into an open one.**

*What broke:* CEL raises `no such key` for an undefined map key. So
`if: params.deploy == "yes"` with `deploy` misspelled did not skip the step and
did not fail the run — it **ran the step**. A deploy gate failed open. The same
trap existed for `secrets`. Both now read an undefined key as the empty string
and write a System line into the run's log.

`steps` still uses CEL's default map semantics, deliberately: it is
`map(string, dyn)`, so defaulting a missing step to `""` would only move the
error to the `.outputs` access rather than closing it.

**If you add anything to the CEL environment, decide explicitly what an
undefined reference does.** Erroring is the dangerous choice, not the safe one.

### `finally:` is a second, separate pipeline invocation

`internal/agent/orchestrator.go` calls `RunPipeline` twice: once for the main
DAG, once for `finally:`. **Anything implemented or validated only on the main
path is accepted by the parser and silently ignored in `finally:`** — no step
status, no run status, no log line.

*What broke:* an audit found five separate features that did nothing in
`finally:` — a hook drain, `retry:` degrading to one attempt on a cancelled
run, a failing step reporting `Cancelled` instead of `Failed`, outputs never
promoted to run outputs, and the phase having no time ceiling at all.

**When you add a step-level feature, check it on both paths.**

### `context.WithoutCancel` strips the deadline, not just the cancellation

A context from `context.WithoutCancel` has a nil `Done()` channel. Any
`select` on `<-ctx.Done()` therefore blocks forever, and any deadline the
parent carried is gone.

*What broke:* the `finally:` phase ran on `WithoutCancel(ctx)` to survive
cancellation — which is correct — and thereby also lost the job's
`spec.timeoutMinutes`. A `call:` step in `finally:` with no per-step timeout
held an agent slot **indefinitely**, and could not self-heal: the reaper keys
on agent liveness, and the agent kept heartbeating.

The rule that came out of it: **every post-DAG phase that *executes* something
carries a budget; only the controller report does not.** The report is
deliberately unbounded — an agent that gave up on `FinishRun` is still claiming
and heartbeating, so no reaper would recover the run.

### `CloseScopes` runs once, after every `EnsureScope` has returned

It is called from a single `defer` in `RunClaim`, after the whole pipeline has
returned. Its `ctx` is the teardown phase's own budget window — a
`context.WithTimeout` over `context.WithoutCancel`.

**Implementations must thread that context through, not re-strip it.** A
`WaitGroup` join, which cannot take a context, must be raced against
`ctx.Done()` instead.

*What broke:* `k8sBackend.CloseScopes` re-stripped the deadline and then
blocked on a context-unaware join, so the teardown ceiling four pages of
documentation told operators to size against existed only on the host backend.

---

## Logs and secrets

### The masker matches per line, so a flush must never split a line

`secrets.Masker.Mask` is substring replacement applied to one log line. If a
value is split across two lines, neither fragment matches, and both ship
unmasked.

*What broke:* `LogPusher.Write` flushed as soon as the buffer crossed 4 KiB,
without regard for newlines. A step's stdout arrives from a pipe in chunks
unrelated to line boundaries, so a chunk boundary landing inside a secret split
it in two:

```text
[0] ...xxxxxxxxSUPER-SECRET
[1] -TOKEN-abcdefghijklmnop
```

Both shipped unmasked, and concatenating the stored log reconstructs the
secret. Size-triggered flushes now ship only complete lines, with a hard
ceiling for the pathological no-newline case.

**Any new flush trigger must respect line boundaries.**

### Log batches must never overtake one another

The controller assigns `seq` **on arrival**. If a newer batch lands while an
older one is still queued, the stored order stops matching the emission order —
permanently, for every reader, with no marker and no duplicates to make it
visible.

`flushPendingLocked` therefore retries oldest-first and **stops at the first
failure**, accepting head-of-line blocking as the price of the ordering
guarantee. The current buffer is then queued *behind* the backlog rather than
sent.

---

## Credentials and identity

### The two token audiences must stay different

Agent enrollment verifies a projected ServiceAccount token with audience
`unified-cd-agent-enrollment`. The store-credential broker verifies one with
audience `unified-cd-store-credentials`.

**If these were ever the same, any job Pod's token would enroll an agent.**

This is asserted from two layers and in both directions, and the Pod-spec test
pins the literal string rather than the constant, so renaming the constant
cannot quietly make them equal.

### `S3Config.Creds == nil` must fall back to a static key pair

The controller and the host agent construct `objectstore.S3Config` literals
without `Creds`. That nil-fallback in `NewS3ObjectStore` is the only thing
keeping them unchanged while the Kubernetes sidecar grows refreshable
credentials.

**If a change to the object-store seam requires touching
`cmd/controller/main.go` or `cmd/unified-cd-agent/main.go`, the seam is in the
wrong place.** Stop and reconsider rather than changing them.

### Credential-file change detection cannot use size

*What broke:* the file credential provider compared mtime and size. An S3
access key ID is always 20 characters and a secret always 40, so a real
rotation never changes the size — leaving mtime alone carrying the signal, and
mtime is quantised. A rotation landing inside one filesystem tick was invisible,
so a rotated credential silently never reached the client. It now compares a
SHA-256 of the contents.

### A payload-mapped webhook param must be constrained

`validateWebhookPayloadMappedParams` requires `pattern:`, `unvalidated: true`,
or `choices:` on any param whose value is templated from a webhook payload,
because param values are interpolated into step shell text.

**When adding a new way to constrain a param, wire it into that gate.** If you
do not, an author using the new mechanism is told to add a redundant
`pattern:`, and the path of least resistance becomes `unvalidated: true` — the
feature then makes security worse.

---

## Metrics

### The registry is private, so nothing is registered for you

`metrics.New()` builds a `prometheus.NewRegistry()` rather than using
`prometheus.DefaultRegisterer` — correct, because several `Server` instances
coexist in one test binary and the global registry would panic on duplicate
registration. But a private registry starts **empty**.

*What broke:* the Go and process collectors are auto-registered only on the
default registry. Choosing a private one silently cost every `go_*` and
`process_*` series, so a goroutine leak, a memory climb toward an OOMKill, and
GC pressure behind a latency regression were all invisible. `NewForController`
now registers them explicitly.

### A pass-level "success" can hide every item failing

Several background workers iterate a batch and swallow per-item failures so one
bad item cannot abort the sweep — the log archiver logs a run it could not
archive and returns `nil`.

*What broke:* a pass in which **every single item failed** reported success.
Items are therefore counted separately by result;
`rate(unifiedcd_background_task_items_total{result="error"}[15m])` is the query
that distinguishes "nothing to archive" from "nothing archivable".

---

## Structure and generation

### Hand-maintained lists drift — nine times so far

The most repeated defect in this codebase: a list someone must remember to
update, and a new field or kind that silently falls off it.

*What broke, most sharply:* the `uses:` inlining keep-list dropped `Approval`,
so **a shared template's human approval gate vanished on inlining and the step
reported Succeeded**.

Prefer deriving a list, or guarding it by reflection, over correcting it by
hand. Working precedents:

- `internal/gittemplate/inline_fields_test.go` — reflection over struct fields;
  fails when a new field has no recorded policy.
- `internal/schemakinds` — root kinds derived from the generated schema, used
  by both `docgen` and `export`'s completeness guard.
- `cmd/schemagen`'s `TestSchemaIsUpToDate` — regenerates in memory and diffs.

### A guard is not trusted until it has been seen to fail

*What broke:* `TestMetricsEndToEndWiring` re-performed main.go's registration
calls instead of calling them, so deleting a registration from main.go left it
green — and the controller did in fact ship with no runtime metrics while that
test passed.

**Break the thing, watch the guard fail, restore it, and say you did.**

### Generated files are committed, and `omitempty` decides schema `required`

`schemagen` derives JSON-Schema `required` from the struct tag: no
`omitempty`, not a pointer, therefore required.

*What broke:* `spec.params` was tagged `yaml:"params"`, so the schema required
it, and **21 of 26 shipped example manifests failed their own schema** — every
real Job showed as invalid in editors with YAML validation.

A field the store round-trips through JSON also needs a `json:` tag; the yaml
tag alone does not survive the trip.

### The shim binaries are not byte-reproducible across platforms

Same source, three environments, three different binaries — verified. Only the
host OS mattered; checkout path, GOROOT location and CPU count did not.

Also, `core.filemode` is `false` in this repo (it is developed on Windows) and
the committed blobs are mode `100644`, while a Linux build produces `100755`.

*What broke:* a byte-exact `git diff` CI guard failed on the very PR that
introduced it, for both reasons at once. It was withdrawn. Drift is now caught
by hashing the shim's **source**, which does not depend on build reproducibility
at all.

---

## Kubernetes specifics

### `rest.Config.Timeout` is deliberately not set

The same `rest.Config` drives `remotecommand.NewSPDYExecutor` for exec streams
and `GetLogs(Follow: true)`. Because it becomes `http.Client.Timeout` — a cap
on the whole request including the body read — any value that bounds teardown
would also kill running steps and follow-mode log reads.

**Bound at the call sites instead.** This reasoning is recorded at the code so
nobody "fixes" it.

### `envFrom.secretRef` cannot cross namespaces, and is snapshotted

A Secret consumed through `envFrom` is read once at container creation and
never updates, and `LocalObjectReference` has no namespace field.

*What broke:* the documentation said to create the artifact Secret "in the
agent's namespace", but job Pods run in a different namespace. Following it
produced `CreateContainerConfigError` and a five-minute unexplained timeout,
because the Pod-start wait only polled phase and never read the container's
waiting reason.
