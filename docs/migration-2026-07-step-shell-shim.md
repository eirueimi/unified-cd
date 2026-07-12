# Migration: `shell:` field, `ucd-sh` shim, Go keep-alive

This release changes **how every `run:` step script is executed** on both
agents, for isolated jobs (the default — see [Job Isolation: `native` and
the claim pod](jobs.md#job-isolation-native-and-the-claim-pod)):

| Backend | Before | After |
|---|---|---|
| k8s-agent pod exec | `bash -lc "<script>"` (hardcoded) | `/.ucd/ucd-sh -c "<script>"` (the injected shim) |
| Standard agent claim-pod exec | `sh -c "<script>"` (`ExecSpec.Shell` default) | `/.ucd/ucd-sh -c "<script>"` (the injected shim) |
| Keep-alive (both backends) | `sleep infinity` | `/.ucd/ucd-sh pause` |
| `native: true` host steps | host `bash -lc` (Git Bash on Windows) | **unchanged** — still host `bash -lc` |

This is a **breaking change** for isolated jobs whose `run:` scripts rely on
real bash (or the login-shell profile-sourcing `-l` gives you) — see
[Job Reference: Shell (`shell:`)](jobs.md#shell-shell) for the full field
and its resolution priority, and the [interpreter constraints
table](jobs.md#the-default-the-ucd-sh-shim) for exactly what the new
default does and doesn't support. `native: true` jobs are not affected by
any of this migration.

## Why

The old defaults hardcoded a shell binary requirement the DSL never stated:
`bash -lc` on k8s meant every `podImage`/`podTemplate` container needed a
working `bash`, and this very docs set recommended bash-less images
(`golang:1.24-alpine`, `alpine:3.19`) as `podImage` — a standing doc bug.
The new default, the injected `ucd-sh` shim (a static Go binary embedding
[`mvdan.cc/sh`](https://github.com/mvdan/sh)), needs **no shell binary in
the image** — bash-less/sh-less images with coreutils (`alpine`,
busybox-based) now work as step containers with zero extra configuration.
(Truly empty images — `scratch`, distroless-static — remain limited on the
k8s agent: env application prepends the `env` binary they lack, so
env-carrying steps fail with exit 127.) See [Kubernetes Integration: Step
execution mechanism](kubernetes-integration.md#step-execution-mechanism)
for the full exec-model rewrite.

## Upgrade ordering (mixed-version fleets)

- **Upgrade the k8s-agent image (or point `shimImage` at an image
  containing `/ucd-sh`) together with or before the new agent.** The new
  agent's `ucd-shim` init container runs `shimImage` (default: the
  k8s-agent image itself); an image predating this feature has no
  `/ucd-sh`, and every job pod would hang at init.
- An **old agent behind a new controller** is safe: it silently ignores the
  `shell` field on claim steps and keeps its old defaults (`bash -lc` on
  k8s, `sh -c` in host claim pods) until upgraded.

## What you need to do

### 1. Bashism- or profile-dependent jobs: add `shell: [bash, -lc]`

If a job's `run:` scripts use constructs the shim doesn't support (real
signal traps, `wait -n`/`-p`, `jobs`, `kill $!`, `PIPESTATUS`, `/dev/tcp`,
`shopt` options beyond the supported subset — see the [full constraints
table](jobs.md#the-default-the-ucd-sh-shim)), or relied on `-l`
login-shell behavior (below), restore the exact old behavior with one line:

```yaml
# Before (implicit — this was the hardcoded k8s exec wrapper)
spec:
  steps:
    - name: build
      run: |
        trap 'cleanup' TERM
        wait -n
        ...

# After — restore real bash explicitly
spec:
  shell: [bash, -lc]   # job-level: every step in this job uses real bash
  steps:
    - name: build
      run: |
        trap 'cleanup' TERM
        wait -n
        ...
```

Prefer a step-level override (`steps[].shell: [bash, -lc]`) over the
job-level form if only one or two steps need it — see the [resolution
priority table](jobs.md#shell-shell) for how step-level, `uses:`-template,
and job-level `shell:` interact.

`shell: [bash, -lc]` (k8s) or the equivalent host-runnable image needs a
real `bash` in the target container/image, same as before this release —
the shim doesn't remove that requirement for jobs that opt back into bash,
it only stops **forcing** it on jobs that don't need it.

### 2. Login-shell (`-l`) profile sourcing: same fix

The default changed from `-lc` (k8s's old hardcoded wrapper) to plain `-c`:
**login-shell profile sourcing (`/etc/profile`, `~/.bash_profile` PATH
setup, etc.) no longer happens by default.** A job that relied on a
login shell putting something on `$PATH`, or setting an env var via a
profile script, needs the same `shell: [bash, -lc]` fix as above — it isn't
just about unsupported constructs, it's specifically about the `-l` flag.
If you're not sure whether a job depends on this, look for `run:` scripts
that invoke a tool with no full path and no explicit `export`/`ENV:` setup
for it in the job or its image.

### 3. Host claim-pod jobs: same shell, same fix

The standard agent's claim-pod exec changed in lockstep (`sh -c` →
`/.ucd/ucd-sh -c`) — this is a near-superset of the POSIX-`sh` subset most
scripts use, and the compatibility corpus gate
(`internal/shim/corpus_test.go`) verifies every script shipped in
`examples/` and `templates/` runs clean under the new default with zero
`shell:` overrides needed. If a host-executed job breaks, the fix is
identical to the k8s case: add `shell: [bash, -lc]` (the image/host needs a
real `bash` either way — the standard agent's claim pod containers pull
their own image, same as k8s).

### 4. Nothing else to change

- `ARG_MAX` (the OS-level limit on how large a single exec'd argv/script can
  be) is **unchanged** by this migration — the shim receives the script as
  one argv element exactly like `bash -c`/`sh -c` did, so scripts that fit
  before still fit now.
- Step outputs, `env:`, secrets, `if:`, `post:`, artifacts, cache, matrix/
  foreach — none of this changed. Only the interpreter that runs `run:`
  scripts changed.
- `podImage`/`podTemplate` container images that were already bash-less
  (and therefore already broken under the old hardcoded `bash -lc`) now
  work with no changes required — see [Configuration Reference: K8s Agent
  config fields](configuration.md#k8s-agent-config-fields) for the
  `podImage` guidance update.

## What did not change

- `native: true` jobs — still host `bash -lc` (Git Bash on Windows) by
  default, unaffected by any part of this migration.
- `call:` steps — always resolved their own `shell:` from the called job's
  own spec; this was already true and remains true (`call:` never inherited
  the caller's `shell:`, `env:`, or any other spec field).
- The `uses:` template scope/inlining model.
- Artifact/cache transfer mechanisms (the sidecar in k8s, the direct
  filesystem access on the host agent).

## Reference

- [Job Reference: Shell (`shell:`)](jobs.md#shell-shell) — field shape,
  resolution priority, the full interpreter constraints table, and the
  `trap` sanitizer.
- [Kubernetes Integration: Step execution
  mechanism](kubernetes-integration.md#step-execution-mechanism) — the
  `/.ucd` shim injection and keep-alive rewrite.
- [Configuration Reference: K8s Agent config
  fields](configuration.md#k8s-agent-config-fields) — the new `shimImage`
  field.
