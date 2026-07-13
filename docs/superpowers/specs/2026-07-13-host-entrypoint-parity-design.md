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

- No change to k8s command/args handling (already correct).
- No multi-element `--entrypoint` support (unnecessary; the clear+positional form
  covers every case).
- No change to `shell:` (the step interpreter) — orthogonal: `shell:` decides how
  a `run:` script is exec'd INTO a running container; Entrypoint/Args decide what
  the container's own process is.
