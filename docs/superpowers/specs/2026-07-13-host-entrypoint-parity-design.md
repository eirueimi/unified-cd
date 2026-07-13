# Host Container command/args → k8s PodSpec Semantics

**Status**: Draft for review
**Date**: 2026-07-13

## Problem

A podTemplate container's `command`/`args` mean different things on the two
backends today:

- **k8s** builds a native `corev1.Container`, so `command` overrides the image
  ENTRYPOINT and `args` overrides CMD — standard Kubernetes/OCI semantics.
- **host** (claim pod) merges `command` + `args` into one slice and appends it
  as positional arguments (`docker run <image> <command...> <args...>`). Docker
  treats positional args as CMD, so on the host `command` overrides only CMD and
  the image's ENTRYPOINT **always still runs** — and `command` vs `args` are
  indistinguishable.

Consequences:
1. A sidecar (or primary) with `ENTRYPOINT ["helm"]` and `command: ["kubectl", …]`
   runs `helm kubectl …` on the host but the intended `kubectl …` on k8s.
2. The primary "job" keep-alive sets `Command = ["/.ucd/ucd-sh","pause"]`, passed
   positionally — so a primary image with its own ENTRYPOINT would run
   `<image-entrypoint> /.ucd/ucd-sh pause` and the keep-alive breaks. This works
   today only because the default runner image has no ENTRYPOINT; a podTemplate
   that sets the `job` container to an ENTRYPOINT-bearing image is a latent bug.

Goal: make the host match k8s — `command` overrides the image ENTRYPOINT, `args`
overrides CMD, the two are distinct — and harden the keep-alive against
ENTRYPOINT-bearing primary images.

## Design

### Runtime layer: split CreateSpec.Command into Entrypoint + Args

`internal/runtime` (`CreateSpec`) replaces the single `Command []string` with:

- `Entrypoint []string` — the k8s `command` (ENTRYPOINT override). `nil` means
  "use the image's ENTRYPOINT".
- `Args []string` — the k8s `args` (CMD override). `nil` means "use the image's
  CMD".

`createArgs` (both `ociCLI` and `appleContainer`) emits, after all existing flags
and `spec.Image`:

| Entrypoint | Args | emitted tail | resulting process |
|---|---|---|---|
| nil | nil | *(nothing)* | image ENTRYPOINT + CMD (default) |
| nil | non-nil | `<args...>` | imageENTRYPOINT + args |
| non-nil | any | `--entrypoint "" <entrypoint...> <args...>` | entrypoint + args (image ENTRYPOINT ignored) |

Only the **empty-string clear** form of `--entrypoint` is ever emitted — never a
multi-element `--entrypoint` value. That deliberately sidesteps docker's
single-token `--entrypoint` limit and nerdctl's "multi-string entrypoint not
supported" gap (containerd/nerdctl#1069): the full override argv rides in as
positional args after the cleared entrypoint. This reproduces k8s semantics
exactly for all three rows.

### Keep-alive uses Entrypoint override

Every injected `ucd-sh pause` keep-alive (claim-pod primary and pause containers,
uses-scope containers, workspace-cleanup container) switches from
`Command: ["/.ucd/ucd-sh","pause"]` (positional) to
`Entrypoint: ["/.ucd/ucd-sh","pause"], Args: nil`. Because `--entrypoint ""`
clears the image ENTRYPOINT, `pause` runs regardless of what ENTRYPOINT the
container image declares — fixing the latent primary-image bug above.

### parseContainerDef splits command/args

`internal/agent/claim_pod.go` `containerDef` gains `Entrypoint []string` and
`Args []string` (replacing the merged `Command`). `parseContainerDef` maps the
podTemplate container's `command` → `Entrypoint` and `args` → `Args`, no longer
concatenating them. A sidecar that sets neither (`Entrypoint` and `Args` both
nil) still runs its image's own entrypoint/CMD — the PR #13 behavior is
preserved. `dsl.HostSupportedContainerFields` already lists `command` and `args`,
so routing is unaffected.

### Degradation for runtimes without `--entrypoint ""` clear

`--entrypoint ""` clear is well supported by docker and modern podman, but its
support on nerdctl / wslc / Apple `container` is not documented and must be
verified on real binaries during implementation. For any runtime confirmed NOT
to support the empty clear, the runtime driver — which knows its own name —
degrades: when `Entrypoint` is non-nil it emits the positional
`<entrypoint...> <args...>` **without** the `--entrypoint ""` flag (today's
behavior: CMD override only, the image ENTRYPOINT still runs) and logs one WARN
naming the runtime and the limitation. This never silently produces a *broken*
command; it produces the pre-existing behavior plus a clear diagnostic. The set
of "no empty-clear" runtimes is a small explicit list in the runtime package,
seeded by the implementation-time verification (empty until proven necessary —
do not speculatively add runtimes).

### k8s backend: unchanged

The k8s path already uses native `corev1.Container.Command`/`.Args`
(ENTRYPOINT/CMD override) and `injectKeepAlive` already sets `.Command` (a native
ENTRYPOINT override that ignores the image ENTRYPOINT). Nothing changes there;
this spec only brings the host to parity with it.

## Breaking change & migration

Host sidecar/primary `command:` semantics change: previously
`command: [X]` on the host produced `imageENTRYPOINT X` (CMD override only); now
it produces `X` (ENTRYPOINT replaced), matching k8s and the documented PodSpec
model. Jobs that relied on the old host-only merge behavior (rare, undocumented,
and divergent from k8s) must move the arguments to `args:`. Documented in the
migration guide and CHANGELOG. `args:`-only and no-command/args sidecars are
unaffected.

## Testing

- **runtime unit** (`ocicli` + `appleContainer` argv tests): assert the three
  truth-table rows — nil/nil emits no tail; Args-only emits positional args with
  NO `--entrypoint`; Entrypoint-set emits `--entrypoint ""` then
  `entrypoint... args...`. Assert the degrade path (a runtime in the no-clear set
  omits `--entrypoint ""` and warns) via a table entry.
- **claim_pod unit**: a sidecar with `command` only → Entrypoint set, Args nil;
  `args` only → Args set, Entrypoint nil; both → both; neither → both nil (image
  default). Primary/pause/scope/cleanup keep-alive → Entrypoint = pause argv.
- **docker-gated integration** (build tag `integration`, runtime.Detect skip):
  (1) a sidecar whose image has a real `ENTRYPOINT` (e.g. a purpose-built or
  known image), with `command` overriding it, actually runs the override, not the
  image entrypoint; (2) a primary "job" container on an ENTRYPOINT-bearing image
  keeps alive under `ucd-sh pause` (the latent-bug fix), proven by a default step
  exec'ing successfully into it.
- **Runtime verification (manual, recorded in the report + docs matrix)**: run
  `<runtime> run --entrypoint "" <image> echo ok` for docker / podman / nerdctl /
  wslc / Apple `container` on available binaries; record which support the empty
  clear. Runtimes that fail get added to the no-clear set with a docs note.

## Non-goals

- No change to k8s sidecar command/args handling (already correct — a k8s
  sidecar's `command`/`args` override its image ENTRYPOINT/CMD natively). The one
  k8s change in this work is the primary `job` keep-alive (see parity fix #1
  below), not sidecar semantics.
- No multi-element `--entrypoint` support (unnecessary; the clear+positional form
  covers every case).
- No change to `shell:` (the step interpreter) — orthogonal: `shell:` decides how
  a `run:` script is exec'd INTO a running container; Entrypoint/Args decide what
  the container's own process is.

---

# Additional host/k8s parity fixes (audit findings #1–#5)

A follow-up audit of the two backends' container-def handling surfaced five
places where host and k8s diverge on the same podTemplate input. All five are
folded into this parity work (same `internal/agent/claim_pod.go` /
`internal/k8sagent` surface as the entrypoint split above). Each fix and its
chosen direction:

## Fix #1 — primary `job` keep-alive: both backends always force `ucd-sh pause`

**Divergence:** the host claim pod always replaces the primary `job` container's
argv with `ucd-sh pause` (`claim_pod.go` `Start`), but k8s `injectKeepAlive`
(`podbuilder.go`) injects the keep-alive **only when** the `job` container has no
`command` AND no `args` — so a podTemplate that sets `command` on `job` runs that
command on k8s and `pause` on the host.

**Decision (chosen): both backends always force the keep-alive on the primary
`job` container**, ignoring any `command`/`args` the podTemplate set on it. The
execution model requires the `job` container to stay alive as the exec target for
`container:`-less steps; honoring a non-persistent user command (as k8s does
today) lets the container exit and breaks every later step's exec-in. This is the
safer direction and matches what the host already does.

- k8s: `injectKeepAlive` drops the `len(Command)==0 && len(Args)==0` guard **for
  the primary container only** — for `primaryContainerName` it unconditionally
  sets `Command = ucdKeepAliveArgv()` and clears `Args`. Sidecars are still left
  untouched (a sidecar with no command runs its own entrypoint; a sidecar with a
  command overrides it — unchanged).
- host: already forces pause on the primary via the Entrypoint override (the
  entrypoint-split work above) — no further change.
- The k8s tests that currently assert the primary keeps an explicit
  command/args (`TestInjectKeepAlive_JobKeepsExplicitCommand`,
  `TestInjectKeepAlive_JobKeepsExplicitArgs`) are inverted to assert the new
  always-pause behavior; the sidecar-untouched tests stay.

## Fix #2 — `resources.requests` on the host: WARN and ignore

**Divergence:** k8s honors both `resources.requests` and `resources.limits`; the
host `parseContainerDef` reads only `limits`, so `requests` vanishes with no
diagnostic — yet `resources` is in `HostSupportedContainerFields`, so routing
never sends the job to k8s and the per-field WARN loop never fires (the key
`resources` itself is allowed).

**Decision (chosen): host emits one WARN when `resources.requests` is present and
ignores it; `limits` continue to apply as today.** docker/podman have no
"request" concept (only limits), so there is nothing to map to; the WARN closes
the silent-drop gap without a routing change. `parseContainerDef` checks for a
non-empty `resources.requests` sub-map and logs
`"podTemplate container resources.requests is not supported on the host agent
(docker/podman have no request concept) and is ignored; use resources.limits or
route to a Kubernetes agent"`.

## Fix #3 — non-string env `value`: both backends hard-error

**Divergence:** an env entry whose `value` is not a string (e.g. the unquoted
YAML `value: 8080`, decoded as a number) is silently dropped on the host with a
misleading "without a literal value is ignored" WARN (which implies `valueFrom`);
on k8s the same map fails `json.Unmarshal` into `corev1.EnvVar{Value string}`, so
`BuildPod` returns a hard error and the run fails loudly.

**Decision (chosen): match k8s — the host hard-errors on a non-string env
`value`.** `parseContainerDef` distinguishes "no `value` key at all" (still the
existing `valueFrom`-style WARN + skip — legitimately unsupported on the host)
from "`value` present but not a string" (a malformed job → hard error:
`"podTemplate container %q env %q: value must be a string (got %T); quote the
value"`). This requires `parseContainerDef` to return an `error` (see the plan's
error-propagation task).

## Fix #5 — unnamed podTemplate container: both backends hard-error early

**Divergence:** a container map with no `name` is silently `continue`-skipped on
the host (`claimContainerDefs`); on k8s it flows into the Pod object and is
rejected only later by the API server at pod-create time (a run-creation
failure), not at build time.

**Decision (chosen): both backends hard-error early at pod-build time.** The host
`claimContainerDefs` returns an error on an empty container name instead of
skipping; k8s `BuildPod` adds an early validation loop (alongside the existing
reserved-name guard) that rejects any container with an empty name before the Pod
is sent to the API server. Both fail with a clear message
(`"podTemplate container at index %d has no name"`) rather than one silently
dropping and the other failing late and opaquely.

## Fix #4 — k8s cache hit/miss log accuracy

**Divergence:** `unified-sidecar cache restore` knows internally whether the
restore hit or missed (it prints "cache hit"/"cache miss" to its own stderr) but
always exits 0, and k8s `CacheRestore` maps "exit 0" → `hit=true`
unconditionally. The shared orchestrator logs `"cache hit"` off that bool, so on
k8s a genuine miss is logged as a hit. (Host `CacheRestore` reports a true
hit/miss via `ErrCacheMiss`, so only k8s is wrong.) No functional impact —
caching still works; the step never fails on a miss — but the log lies.

**Decision (chosen): make the k8s log honest without touching the best-effort
exit-0 contract.** `unified-sidecar cache restore` additionally writes a stable
machine-readable marker line to **stdout** — `UCD_CACHE_RESULT=hit` or
`UCD_CACHE_RESULT=miss` — on the respective branch (the human-readable line stays
on stderr, exit code stays 0 always). k8s `CacheRestore` captures the sidecar's
stdout (a small buffer instead of `io.Discard`) and parses the marker: `miss` →
`(false, nil)`, `hit` → `(true, nil)`, marker absent (older sidecar / error path)
→ `(true, nil)` preserving today's lenient default. The orchestrator log is then
accurate on both backends with no change to the lenient never-fail policy.

## Parity-fixes non-goals

- No apply-time DSL validation for #3/#5 — the hard error lands at pod-build time
  (host `Start`/`claimContainerDefs`, k8s `BuildPod`), which is where k8s already
  fails and is symmetric across backends. Moving validation to apply time is a
  larger, separate change.
- No `resources.requests`→docker-limit mapping for #2 (semantics don't
  correspond; a mapping would mislead).
- No sidecar cache-protocol redesign for #4 — the stdout marker is additive and
  backward-compatible (absent marker → today's behavior).
