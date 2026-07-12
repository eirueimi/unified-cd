# Step Shell Execution Overhaul: `shell:` Field, ucd-sh Shim, Go Keep-Alive

**Status**: Draft for review
**Date**: 2026-07-12

## Problem

Today the same `run:` script executes under three different shells depending on
where it lands, and two of the three impose image requirements the DSL never
states:

| Path | Shell today | Image requirement |
|---|---|---|
| host native | host `bash` (Windows: Git Bash) | agent host has bash |
| host claim pod exec | `sh -c` (ExecSpec.Shell default) | image has `sh` |
| k8s pod exec | `bash -lc` (hardcoded in executor.go) | **image has `bash`** |

Consequences, all observed live:

- A bash-less image (`golang:1.24-alpine` as `podImage`) fails every
  podTemplate-less k8s job with the cryptic `env: can't execute 'bash'` —
  and the docs' own `podImage` examples recommend exactly such images.
- The same script can be a bashism away from working on k8s but breaking on
  the host claim pod (or vice versa). PR #9 (pod-sidecar sh-compat) and
  PR #24's second commit (debian step container + init-container `buildctl`
  copy, purely to satisfy `bash -lc`) are both workarounds for this split.
- The keep-alive `sleep infinity` injected into the primary "job" container
  depends on a `sleep` binary in the image (and on GNU-style `infinity`
  support), and as a non-PID-1-aware process it never reaps zombies.

## Design summary

One execution model, one default interpreter, zero image requirements:

```
step exec argv = <effective shell argv> + [run: script]
default shell argv = ["/.ucd/ucd-sh", "-c"]        (injected Go shim, mvdan/sh)
keep-alive        = ["/.ucd/ucd-sh", "pause"]       (replaces sleep infinity)
```

- A new DSL field `shell:` (array of strings) overrides the default per step
  or per job — `[bash, -lc]` restores today's k8s behavior exactly;
  `[python3, -c]` is equally valid.
- The shim binary `ucd-sh` is injected at a reserved path `/.ucd` into every
  claim-pod/pod container: host via a read-only bind mount, k8s via an
  init container + emptyDir (the Tekton/Argo emissary carrier pattern).
- Explicitly rejected alternative: auto-delegation (shim exec's image bash
  when present). It makes execution semantics depend on image contents — a
  base-image swap would silently change shell dialect. The default is always
  the shim; real bash is an explicit `shell:` choice.

## Component 1: `shell:` DSL field

### Shape

```yaml
spec:
  shell: [bash, -lc]          # job-level default (optional)
  steps:
    - name: build
      shell: [bash, -euo, pipefail, -c]   # step-level override (optional)
      run: |
        make build | tee build.log
    - name: quick
      shell: [python3, -c]                # any interpreter
      run: print("hi")
    - name: default
      run: echo hi                        # -> ["/.ucd/ucd-sh", "-c", "echo hi"]
```

- Type: non-empty `[]string`. v1 accepts the array form only — no scalar
  shorthand, no string splitting. The array is exec'd verbatim (argv), the
  `run:` script appended as the final element. We never re-parse or quote.
- Validation at apply time: non-empty array of non-empty strings, else 400.
  A program missing from the target image is a runtime step failure with an
  actionable message (`step "build": exec "python3": not found in the
  container image — check shell: or the image`).

### Resolution priority (most specific wins)

1. `step.shell` — steps inside `parallel:` and `finally:` count as steps.
   A `post:` hook may declare its own optional `shell:` (same array form);
   when absent it inherits its owning step's effective shell. The override
   exists because inheritance alone breaks down for non-shell interpreters:
   a `shell: [python3, -c]` step with a shell-script cleanup hook needs
   `post: {shell: [sh, -c], run: ...}` to be expressible at all.
2. A `uses:` template's own declared shell, materialized at expansion time:
   a template step's own `shell:` survives as-is, and a template-level
   `spec.shell` is stamped onto each inlined step that lacks one. The caller
   cannot override either (the template author declared it because the
   script needs it). A template that declares neither inherits the caller's
   job default at runtime.
3. `spec.shell` (job-level).
4. System default `["/.ucd/ucd-sh", "-c"]` for container execution;
   host bash for `native: true` (unchanged in v1, see Non-goals).

`call:` does NOT inherit — a child run resolves entirely from its own job
spec, consistent with every other spec field. A `container:` step resolves
its shell inside the exec-target container (a sidecar exec needs the shell
present in the sidecar image, same rule as the primary).

### Plumbing

The controller materializes the effective shell argv per step onto
`api.ClaimStep` (same pattern as `container:`); agents perform no resolution.
Host side threads it into the existing `crt.ExecSpec.Shell []string`; the k8s
executor's `buildShellCommand`/`buildEnvShellCommand` take the argv prefix as
a parameter instead of hardcoding `bash -lc`. Behavior parity between the two
backends becomes structural.

Note the default changes `-lc` to `-c`: login-shell profile sourcing
(`/etc/profile` PATH setup) no longer happens by default. Jobs that relied on
it write `shell: [bash, -lc]` (migration guide item).

## Component 2: the ucd-sh shim

New `cmd/ucd-sh`, a static Go binary embedding `mvdan.cc/sh/v3` (BSD-3,
v3.13.1+) with three modes:

- `ucd-sh -c <script>` — run the script with the interp package.
- `ucd-sh pause` — keep-alive: block until SIGTERM/SIGINT then exit 0, and
  reap zombie children while PID 1 (built-in tini). Used for every container
  keep-alive that today hardcodes `sleep infinity` (claim-pod primary and
  netns pause containers, uses-scope containers, workspace-cleanup container,
  k8s primary injection).
- `ucd-sh --install <path>` — copy own executable to path (mode 0755); lets
  the k8s init container populate the shared volume without needing `cp` in
  its image (Argo argoexec's self-copy trick).

### Interpreter constraints (verified upstream, to be documented verbatim)

Supported: `set -e/-u/-x/-o pipefail`, pipes/redirects/heredocs, functions,
`local`, `[[ ]]`, arrays and associative arrays, arithmetic, command and
process substitution (Unix), `until/while/for/case`, most parameter
expansions, fan-out/join (`cmd & cmd & wait`), `wait $!` (virtual handle).

Not supported (upstream-verified, mvdan/sh v3.13.1): `trap` accepts only
EXIT and ERR (signal names error with status 2); `wait -n`/`-p`; `jobs`;
`kill $!` (no kill builtin; `$!` is a virtual `gN` handle no external kill
understands); `PIPESTATUS`; `shopt` beyond 6 options; `/dev/tcp`; real
fork/PID semantics (subshells are goroutines). Scripts needing these declare
`shell: [bash, -lc]`.

### trap sanitizer (required)

`trap f TERM INT EXIT` would exit status 2 at the trap line — under `set -e`
the script dies before doing anything. ucd-sh intercepts `trap` calls,
strips unsupported signal names, keeps EXIT/ERR, and emits one WARN line to
stderr recommending `shell: [bash, -lc]`. Graceful degradation instead of a
hard break. (Exact mvdan hook — CallHandler or equivalent — is an
implementation detail to pin during planning; if no clean hook exists, a
pre-parse AST rewrite of trap calls is the fallback.)

### Distribution

`go:embed` into the agent and k8s-agent binaries (arch matches GOARCH of the
build; host job containers share the host arch, k8s init container uses the
agent's own multi-arch image). Version-locked to the release; no separate
artifact.

## Component 3: `/.ucd` injection

- **Host (claim pod, uses-scope, cleanup containers)**: at startup the agent
  writes the embedded ucd-sh to `<agent data dir>/tools/ucd-sh`. Every
  container the agent creates gains a second mount
  `{HostPath: toolsDir, ContainerPath: "/.ucd", ReadOnly: true}` —
  `crt.Mount` grows a `ReadOnly` field (`:ro` in createArgs).
- **k8s**: podbuilder adds an `emptyDir` volume `ucd-tools`, mounts it at
  `/.ucd` on ALL containers (sidecars are `container:` exec targets too),
  and prepends an init container running the k8s-agent's own image with
  `command: ["/ucd-sh", "--install", "/.ucd/ucd-sh"]`. The init image is
  configurable (air-gapped registries).
- `/.ucd` is a reserved path, documented; a podTemplate mounting over it is
  user error (fails loudly at exec).

## Component 4: keep-alive = `ucd-sh pause`

All injected keep-alives switch from `["sleep","infinity"]` to
`["/.ucd/ucd-sh","pause"]`:

- k8s `injectSleepInfinity` (renamed accordingly) — still primary-only, and
  additionally skips injection when the author set command OR args (fixes
  the current clobber of author-set `args:`-only containers).
- host claim-pod primary and pause containers, uses-scope containers,
  workspace-cleanup container.

Wins: no `sleep` binary requirement (busybox `infinity` support no longer
matters; scratch/distroless job containers become valid exec substrates),
zombie reaping, prompt SIGTERM exit. The ENTRYPOINT bypass-vs-respect
question is orthogonal and NOT changed by this spec (current bypass behavior
retained; tracked separately).

## Component 5: compatibility corpus gate (CI)

A Go test suite (internal/shim) that:

1. Extracts every `run:` block from `examples/` and `templates/` and executes
   it under the pure interp with external commands stubbed via ExecHandler —
   proving every script we ship parses and uses only supported constructs.
   (Static audit already passed: zero hits for trap-signals, wait -n, jobs,
   $!, /dev/tcp, [[ in the current corpus.)
2. Pins targeted constructs: `set --` argv manipulation, IFS-newline
   splitting, `until`, arithmetic, fan-out/join, `wait $!` status collection,
   trap EXIT firing, trap TERM sanitization (WARN + no error), and asserts
   clear errors for `wait -n`. Also pins the background-daemon-at-exit
   behavior (does a lingering `cmd &` hang the run?) — currently unverified.
3. Doubles as the regression gate for mvdan/sh version bumps and as the
   scorecard if the shim interp is ever swapped (e.g. brush) — the shim
   boundary is the exec argv, so the interp is replaceable.

## Breaking changes & migration

- k8s default `bash -lc` → `ucd-sh -c`: bashism-dependent or
  profile-dependent scripts add `shell: [bash, -lc]` (one line, restores the
  exact old behavior).
- host claim pod default `sh -c` → `ucd-sh -c`: interp is a near-superset of
  the sh subset our templates use; corpus gate guards this.
- native steps: unchanged.
- Docs updated: `shell:` reference, interp constraints table, reserved
  `/.ucd`, migration guide section, and the `podImage` guidance (the
  bash-less `podImage` examples in docs/configuration.md and
  docs/kubernetes-integration.md become correct under the new default —
  the standing docs bug resolves itself).
- Follow-up unlocked (not in this spec's scope): PR #24's debian step
  container + buildctl-copy init container collapse to running the step
  directly in `moby/buildkit:rootless`.

## Non-goals / future work

- **Entrypoint respect (A/B)** for the primary container: undecided,
  separate track. This spec keeps Command-override semantics.
- **Native default → interp**: would drop the Windows Git Bash dependency
  (go-task precedent); attractive but out of scope for v1.
- **GHA-style file-based script passing** (`{0}` placeholder) for
  interpreters that cannot take code as argv, and for >ARG_MAX scripts.
- **Shell-less argv steps** (Tekton-style `command:`/`args:` step type
  exec'd directly without any interpreter).
- Upstreaming signal-trap support to mvdan/sh.

## Testing summary

- Unit: DSL parse/validation of `shell:`, resolution priority (step > uses >
  job > default), ClaimStep materialization, ExecSpec/argv construction on
  both backends, ucd-sh pause signal/reap behavior, trap sanitizer.
- Corpus gate (Component 5).
- Integration (docker-gated): a job in a bash-less image (alpine) runs green
  under the default; `shell: [bash, -lc]` runs in a bash image; a
  scratch/distroless container as primary with `ucd-sh pause` keep-alive
  stays exec-able.
- k8s parity: podbuilder init-container/volume assertions; parity suite
  passes with the new default on both backends.
