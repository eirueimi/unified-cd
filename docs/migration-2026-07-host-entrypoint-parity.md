# Migration: host container `command:` now overrides ENTRYPOINT (k8s parity)

This release changes **what the standard (host) agent's claim pod does with
a `podTemplate` container's `command:`/`args:`**, for isolated jobs (the
default — see [Job Isolation: `native` and the claim
pod](jobs.md#job-isolation-native-and-the-claim-pod)).

| Backend | Before | After |
|---|---|---|
| k8s-agent (Pod container) | `command` overrides image `ENTRYPOINT`, `args` overrides `CMD` (native `corev1.Container` semantics) | **unchanged** |
| Standard agent (claim-pod container) | `command`+`args` merged into one positional `CMD` override; the image's own `ENTRYPOINT` **always ran** first | `command` overrides `ENTRYPOINT` (via docker `--entrypoint ""` clear), `args` overrides `CMD` — matching k8s |
| Primary `job` container keep-alive, standard agent only | Set via `Command`, positionally appended after whatever `ENTRYPOINT` the image declared — a latent bug for an ENTRYPOINT-bearing primary image | Set via an `Entrypoint` override — the image's own `ENTRYPOINT` is now actually cleared, not just appended to |

The k8s-agent's primary `job` container keep-alive is unaffected by this
particular release: it already used a native `Command` override (a real
`ENTRYPOINT` replacement, not a positional append), so it was never subject
to the host-only latent bug described below. At the time of this release it
still only injected the keep-alive when the `job` container had neither
`command` nor `args` set; a later release (see [parity-fixes
addendum](#addendum-2026-07-host-k8s-container-parity-fixes-1-5) below)
brought k8s to the standard agent's always-force behavior, so this
divergence no longer exists.

This is a **breaking change** for standard-agent (host) jobs whose
`podTemplate` sidecars set `command:` on an image that also declares its own
`ENTRYPOINT`, and relied on the old host-only merge behavior. See [Kubernetes
Integration Guide: Host container command/args
semantics](kubernetes-integration.md#host-container-commandargs-semantics)
for the full before/after truth table and the per-runtime support matrix for
the `--entrypoint ""` clear this relies on.

## Why

A `podTemplate` container's `command`/`args` meant different things on the
two backends. k8s built a native `corev1.Container`, so `command` overrode
the image `ENTRYPOINT` and `args` overrode `CMD` — standard Kubernetes/OCI
semantics. The standard agent's claim pod instead merged `command` + `args`
into one slice and appended it as positional arguments
(`docker run <image> <command...> <args...>`) — Docker treats positional
args as `CMD`, so on the host, `command` only ever overrode `CMD` and the
image's `ENTRYPOINT` **always still ran**. A sidecar with `ENTRYPOINT
["helm"]` and `command: ["kubectl", ...]` ran `helm kubectl ...` on the host
but the intended `kubectl ...` on k8s — silently wrong, and undetectable
without an ENTRYPOINT-bearing image to expose it.

This also meant a `podTemplate` that set the standard agent's primary `job`
container to an ENTRYPOINT-bearing image was a latent bug: the keep-alive
(`/.ucd/ucd-sh pause`) was appended positionally after whatever `ENTRYPOINT`
the image declared, so the container could run
`<image-entrypoint> /.ucd/ucd-sh pause` instead of the keep-alive — breaking
every later step's exec-in. This release fixes that on the standard agent
by forcing the keep-alive via an `Entrypoint` override (which clears the
image's own `ENTRYPOINT`), exactly like a real k8s Pod's `command:`/`args:`
override already did. (The k8s-agent's own keep-alive injection was never
subject to this bug — see the table above.)

## What you need to do

### Jobs that relied on the old host-only merge: move values to `args:`

If a host-runnable `podTemplate` sidecar sets `command:` specifically to
supply arguments to the image's own `ENTRYPOINT` (relying on the old
"`command` becomes `CMD`, `ENTRYPOINT` still runs" behavior), move those
values to `args:` instead — `args:` still overrides only `CMD` and leaves
the image `ENTRYPOINT` in place, on both backends, before and after this
release:

```yaml
# Before — relied on the host's old merge behavior (image ENTRYPOINT still
# ran, "command" values were appended after it as CMD). This job also ran
# differently on k8s all along, where "command" already replaced ENTRYPOINT.
podTemplate:
  spec:
    containers:
      - name: web
        image: myimage-with-entrypoint
        command: ["--flag", "value"]   # intended as ENTRYPOINT args

# After — use args: to keep overriding only CMD, on both backends
podTemplate:
  spec:
    containers:
      - name: web
        image: myimage-with-entrypoint
        args: ["--flag", "value"]      # image ENTRYPOINT still runs, receives these
```

If a job genuinely wants to **replace** the image's `ENTRYPOINT` (the
common, intended use of `command:`), no change is needed — `command:`
now does exactly that on the host, matching what it already did on k8s.

### `args:`-only and no-command/args sidecars: unaffected

A sidecar that sets only `args:` (no `command:`) is unaffected: `args:`
overrode `CMD` only before this release and still does. A sidecar that sets
neither (running its own image `ENTRYPOINT`/`CMD` unmodified — e.g. `mysql`,
`redis`) is also unaffected.

### Primary `job` container: no action needed

The primary `job` container's `command`/`args` (if a `podTemplate` sets
them) were always overridden by the forced `ucd-sh pause` keep-alive on the
host, and still are — this doesn't change what a job author needs to write.
What changes is that the keep-alive is now robust against an
ENTRYPOINT-bearing `job` image; if a `podTemplate` previously worked around
the latent bug (e.g. by picking an ENTRYPOINT-less image for the primary
container specifically to avoid it), that workaround is no longer necessary
but is harmless to leave in place.

## Verifying the `--entrypoint ""` clear on your runtime

The host-side fix relies on the container CLI supporting `--entrypoint ""`
(the empty-clear form). This was verified against real Docker (29.6.1) only;
podman, nerdctl, wslc, and Apple `container` are **unverified** — not
confirmed to support it, not confirmed to fail it. See the [per-runtime
support
matrix](kubernetes-integration.md#per-runtime-support-for-the-entrypoint-clear-standard-agent-only)
for the current list. A runtime later found not to support the clear
degrades automatically to the pre-parity behavior (positional `CMD`
override, image `ENTRYPOINT` still runs) plus a `WARN` log — it does not
fail the run, so upgrading is safe even on an unverified runtime; only the
`ENTRYPOINT`-replacement guarantee wouldn't hold until that runtime is
verified and, if necessary, added to the docs matrix.

## What did not change

- k8s-agent `command`/`args` semantics — already native, already correct;
  nothing in this release touches the k8s Pod-build path for sidecar
  `command`/`args`.
- k8s-agent primary `job` container keep-alive injection — at the time of
  this release, still guarded (injected only when the container had neither
  `command` nor `args` set), unlike the standard agent's now-unconditional
  force. That divergence was closed in a later release — see the
  [parity-fixes addendum](#addendum-2026-07-host-k8s-container-parity-fixes-1-5)
  below.
- `dsl.HostSupportedContainerFields` — `command` and `args` were already
  host-supported fields (routing to a host vs. k8s agent is unaffected by
  this release).
- `resources.requests`, non-string `env` values, unnamed `podTemplate`
  containers, and the k8s cache hit/miss log — separate host/k8s parity
  fixes, not part of this release; see the [parity-fixes
  addendum](#addendum-2026-07-host-k8s-container-parity-fixes-1-5) below.
- `shell:` (the step interpreter) — orthogonal: `shell:` decides how a
  `run:` script is exec'd *into* a running container; `command`/`args`
  decide what the container's own process is.

## Reference

- [Kubernetes Integration Guide: Host container command/args
  semantics](kubernetes-integration.md#host-container-commandargs-semantics)
  — the full truth table and per-runtime `--entrypoint ""` support matrix.
- [Job Reference: Kubernetes Pod Template
  (`podTemplate`)](jobs.md#kubernetes-pod-template-podtemplate) — the
  `podTemplate` container fields the host claim pod understands.
- [Job Reference: Job Isolation: `native` and the claim
  pod](jobs.md#job-isolation-native-and-the-claim-pod) — how the claim pod
  is built and how `container:` targets its containers.

---

## Addendum (2026-07): host-k8s container parity fixes (#1-#5)

A follow-up audit found five more places where the standard agent and the
k8s-agent diverged on the same `podTemplate` input. All five are now fixed
so both backends behave identically. Four are user-visible behavior
changes; the fifth is a log-accuracy-only fix with no user action.

| # | What changed | Before | After |
|---|---|---|---|
| 1 | Primary `job` container keep-alive | k8s only injected `ucd-sh pause` when the `job` container had neither `command` nor `args` set — a `podTemplate` command on `job` actually ran on k8s (but was always overridden on the host). | **Both backends** unconditionally force `ucd-sh pause` on the primary `job` container, discarding any `command`/`args` set on it. |
| 2 | `resources.requests` on the host | Silently dropped with no diagnostic (`resources` is a host-supported field, so no WARN loop fired). | Host logs one WARN and ignores `resources.requests`; `resources.limits` is unaffected and still applies on both backends. |
| 3 | Non-string `env` `value` | Host silently dropped it with a misleading "no literal value" WARN; k8s already hard-failed. | **Both backends** hard-error at job start when an env value is not a string (e.g., unquoted number or boolean). The host reports `podTemplate container %q env %q: value must be a string (got %T); quote the value`, while k8s fails with a generic JSON type-mismatch error. |
| 4 | k8s cache hit/miss log | A genuine cache miss on k8s was logged as `"cache hit"` (the sidecar's exit-0-always contract was mapped straight to `hit=true`). | The log is now accurate — a miss logs `"cache miss"`. **Log-accuracy only** — caching behavior, exit codes, and the never-fail-on-miss policy are unchanged. No user action needed. |
| 5 | Unnamed `podTemplate` container | Host silently skipped it (`continue`); k8s sent it to the API server, which rejected it late, as a run-creation failure. | **Both backends** hard-error at job start: `podTemplate container at index N has no name`. |

### What you need to do

**#1 — Move the `job` container's workload into `steps:`.** If a
`podTemplate` set `command`/`args` on the container named `job` expecting
it to run (this only ever worked on k8s — the host always overrode it),
that command is now silently replaced by the keep-alive on k8s too. Put
the build/test/deploy commands as `steps:` (optionally with
`container: job`, though that's already the default) instead:

```yaml
# Before — only ever ran on k8s; the host already ignored this
podTemplate:
  spec:
    containers:
      - name: job
        image: golang:1.24-alpine
        command: ["go", "build", "./..."]

# After — put the workload in steps: on both backends
podTemplate:
  spec:
    containers:
      - name: job
        image: golang:1.24-alpine
steps:
  - name: build
    run: go build ./...
```

**#2 — No action required unless you relied on the silent drop.** If a
`podTemplate` container set `resources.requests` on the host agent
expecting it to have an effect, it never did (it was already ignored) —
you'll now see a WARN making that explicit. Use `resources.limits`
instead, or route the job to a Kubernetes agent (`agentSelector:
[kind:k8s]`) if you need real CPU/memory requests.

**#3 — Quote non-string env values.** An `env` entry like `value: 8080` (an
unquoted number, decoded as YAML `int`) now fails the job at start on
**both** backends instead of silently vanishing on the host. Quote it:

```yaml
# Before — silently dropped on the host, already failed on k8s
env:
  - name: PORT
    value: 8080

# After — works on both backends
env:
  - name: PORT
    value: "8080"
```

**#4 — No action needed.** This is a log-accuracy fix on k8s only; nothing
about cache behavior, step success/failure, or exit codes changed.

**#5 — Add a `name` to every `podTemplate` container.** A container entry
with no `name` now fails the job at pod-build time on both backends
instead of being silently dropped (host) or rejected late by the
Kubernetes API server (k8s):

```yaml
# Before — silently skipped on the host, rejected late by k8s
podTemplate:
  spec:
    containers:
      - image: aquasec/trivy:latest   # no name

# After — add a name
podTemplate:
  spec:
    containers:
      - name: trivy
        image: aquasec/trivy:latest
```

### Reference

- [Job Reference: podTemplate container parity
  notes](jobs.md#podtemplate-container-parity-notes-host-and-k8s) — the
  consolidated behavior for #1/#2/#3/#5.
- [Kubernetes Integration Guide: Keep-alive
  `ucd-sh pause`](kubernetes-integration.md#keep-alive-ucd-sh-pause) — #1
  detail.
- [Kubernetes Integration Guide: podTemplate container
  validation](kubernetes-integration.md#podtemplate-container-validation)
  — #3/#5 detail on the k8s side.
