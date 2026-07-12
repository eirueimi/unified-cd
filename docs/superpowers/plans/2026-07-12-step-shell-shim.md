# Step Shell Execution Overhaul Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One execution model for `run:` scripts — default interp via an injected `ucd-sh` shim (mvdan.cc/sh), an argv-array `shell:` override field, and a Go `pause` keep-alive replacing `sleep infinity`.

**Architecture:** A new `cmd/ucd-sh` binary (mvdan/sh interp + pause + self-install) is injected at `/.ucd` into every pod/claim-pod container (host: read-only bind mount; k8s: init container + emptyDir). The controller resolves the effective `shell:` argv per step onto `ClaimStep`; agents map "empty = shim default" and exec `shell argv + [script]` verbatim.

**Tech Stack:** Go, mvdan.cc/sh/v3 (BSD-3), existing crt runtime / k8s podbuilder seams.

**Spec:** docs/superpowers/specs/2026-07-12-step-shell-shim-design.md — read it first; it is the authority on semantics.

## Global Constraints

- Default shell argv is exactly `["/.ucd/ucd-sh", "-c"]`; keep-alive argv is exactly `["/.ucd/ucd-sh", "pause"]`; the reserved container path is exactly `/.ucd`.
- Exec argv is always `effective shell argv + [script]`, exec'd verbatim — never re-parse, split, or quote any element.
- Resolution priority: `step.shell` > uses:-stamped > `spec.shell` > empty. `ClaimStep.Shell` empty/nil means "agent applies the shim default". Agents contain NO other resolution logic.
- `post:` hooks: own `shell:` optional; absent = owning step's effective shell.
- `call:` children inherit nothing. `native: true` steps default to the host shell exactly as today; an explicit `shell:` on a native step is honored as a host argv.
- mvdan.cc/sh/v3 at v3.13.1 or newer; repo vendors (`go mod vendor` after adding).
- Breaking-change posture: k8s `bash -lc` and host claim-pod `sh -c` defaults BOTH change to the shim; do not add hidden compat fallbacks.
- All prose/comments in English. Gates per task: `go build ./...`, `go vet ./...`, and the named test packages with `-count=1`.

---

### Task 1: DSL — `shell:` on Spec, Step, and PostStep + validation + schema

**Files:**
- Modify: `internal/dsl/types.go` (Spec, Step ×2 struct variants, PostStep)
- Modify: `internal/dsl/parse.go` (validation)
- Modify: `schemas/` via schemagen, `docs/field-reference.md` via docgen (regen)
- Test: `internal/dsl/parse_test.go` (or the file where field validation tests live)

**Interfaces:**
- Produces: `Spec.Shell []string`, `Step.Shell []string` (both step struct variants at types.go:86 and :116), `PostStep.Shell []string` — yaml tag `shell,omitempty`.
- Produces: validation helper `func validShellArgv(sel []string) error` — nil is valid (unset); non-nil must be non-empty and contain no empty strings.

- [ ] **Step 1: failing tests** — table test: `shell: [bash, -lc]` on step/spec/post parses into the fields; `shell: []` fails validation with an error containing "shell"; `shell: ["", "-c"]` fails; `shell: "bash"` (scalar) is a parse error (array-only).
- [ ] **Step 2: run tests, verify FAIL** (`go test ./internal/dsl/ -run Shell -count=1`).
- [ ] **Step 3: implement** — add the three fields + call `validShellArgv` from the same place other per-step validations run in parse.go (Job.Validate path), for spec-level, every step (incl. parallel/finally), and post.
- [ ] **Step 4: regen** — run the repo's schemagen/docgen targets (see Makefile `manifests`/docgen usage); commit regenerated files.
- [ ] **Step 5: tests PASS; run the Examples/Templates walkers** (`go test ./internal/dsl/ -count=1`).
- [ ] **Step 6: commit** `feat(dsl): shell: argv-array field on spec, steps, and post hooks`.

### Task 2: `cmd/ucd-sh` + `internal/shim` — interp runner, trap sanitizer, pause, install

**Files:**
- Create: `cmd/ucd-sh/main.go`
- Create: `internal/shim/run.go`, `internal/shim/sanitize.go`, `internal/shim/pause.go`
- Test: `internal/shim/run_test.go`, `internal/shim/sanitize_test.go`, `internal/shim/pause_test.go` (pause test linux/darwin-gated where signals are involved)
- Modify: `go.mod`/`go.sum` + `vendor/` (add mvdan.cc/sh/v3)

**Interfaces:**
- Produces: `shim.Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer, env []string, dir string) (exitCode int, err error)` — parses with `syntax.NewParser()`, applies `SanitizeTraps`, runs with `interp.New(interp.StdIO(...), interp.Env(...), interp.Dir(dir))`; maps `interp.ExitStatus` to the exit code.
- Produces: `shim.SanitizeTraps(f *syntax.File, warn func(msg string))` — AST rewrite via `syntax.Walk`: for every CallExpr whose first literal word is `trap`, remove argument words that are signal names other than `EXIT`/`ERR` (case-insensitive match on `INT`, `TERM`, `HUP`, `QUIT`, `SIG*`, and numeric signals); if all condition words were removed, replace the call with `true`. Each removal calls `warn` once with a message naming the signal and recommending `shell: [bash, -lc]`.
- Produces: `shim.Pause()` — installs `signal.Notify` for SIGTERM/SIGINT, and when `os.Getpid() == 1` reaps zombies in a `syscall.Wait4` loop (build-tagged unix); returns on signal.
- CLI contract (main.go): `ucd-sh -c <script>` (stdin/stdout/stderr passthrough, exit code = script's); `ucd-sh pause`; `ucd-sh --install <path>` (copy `os.Executable()` to path, 0755, atomic rename); anything else exits 2 with usage.

- [ ] **Step 1: failing tests** — Run: `echo hi` → stdout "hi" exit 0; `exit 3` → exit 3; `set -e; false; echo no` → nonzero, no "no"; `set -o pipefail; false | true` → nonzero. Sanitize: `trap c EXIT` untouched; `trap c TERM EXIT` → TERM removed + one warn + EXIT still fires (run it: script `trap 'echo cleaned' TERM EXIT` prints "cleaned" once at exit, no error); `trap c INT` alone → call becomes no-op + warn. Pause: send SIGTERM to the test process's child running `ucd-sh pause` (exec the built test binary or gate as integration); `--install` copies and preserves exec bit.
- [ ] **Step 2: verify FAIL.**
- [ ] **Step 3: implement** run.go / sanitize.go / pause.go / main.go as specified.
- [ ] **Step 4: `go mod vendor`; build `go build ./cmd/ucd-sh`; tests PASS** (`go test ./internal/shim/ -count=1`).
- [ ] **Step 5: commit** `feat(shim): ucd-sh — mvdan/sh runner with trap sanitizer, pause keep-alive, self-install`.

### Task 3: Build pipeline — embed ucd-sh in the host agent; ship it in the k8s-agent image

**Files:**
- Create: `internal/shim/embedded/embed.go` (`//go:embed ucd-sh` with a doc comment; the binary file is git-ignored and produced by the build)
- Modify: `Makefile` (`build` target: build `GOOS=linux GOARCH=$(HOSTARCH) ./cmd/ucd-sh` into `internal/shim/embedded/ucd-sh` BEFORE building the agent), `.air.agent.toml` (same two-stage `cmd`), `.gitignore` (`internal/shim/embedded/ucd-sh`)
- Modify: `docker/k8s-agent.Dockerfile` (build `/ucd-sh` into the image alongside the agent binary), `docker/dev.Dockerfile` if it prebuilds anything
- Test: a build-tag-free unit test asserting `embedded.Bytes()` is non-empty when the file exists, skipped otherwise

**Interfaces:**
- Produces: `embedded.Bytes() []byte` (host agent) and image path `/ucd-sh` (k8s-agent image).
- Note: the host agent embeds a LINUX binary of the host's GOARCH (containers share the host arch); a Windows/macOS agent binary embeds the linux shim for its containers. The k8s init container uses the image file, not the embed.

- [ ] **Step 1: wire Makefile + air + Dockerfile; `make build` and the air cmd both produce the embed file first.**
- [ ] **Step 2: `go build ./...` clean with the embed present; commit** `build: two-stage build embeds ucd-sh in the agent and ships it in the k8s-agent image`.

### Task 4: Controller — effective shell on ClaimStep + uses: stamping

**Files:**
- Modify: `internal/api/types.go` (`ClaimStep.Shell []string json:"shell,omitempty"`, `PostStep.Shell []string json:"shell,omitempty"`)
- Modify: `internal/controller/planned_steps.go` (or wherever ClaimStep is built from dsl.Step) — resolve `step.Shell` → else `spec.Shell` → else nil; same for post (post.Shell → else the step's resolved value → else nil… NOTE: carry post.Shell only when post declares one; an empty post.Shell means "inherit at the agent from the step's ClaimStep.Shell")
- Modify: `internal/gittemplate/inline.go` — in `expandUsesStep`, stamp `tplSpec.Shell` onto every inlined step whose own Shell is nil (template step.Shell survives by copy); callers cannot override (no merge with caller spec here — caller-level resolution naturally skips steps that already carry Shell)
- Test: `internal/controller/planned_steps_test.go` (or the ClaimStep-building tests), `internal/gittemplate/inline_test.go`

**Interfaces:**
- Consumes: Task 1 fields. Produces: `api.ClaimStep.Shell`, `api.PostStep.Shell` — nil = shim default at the agent.

- [ ] **Step 1: failing tests** — job spec.shell [bash,-lc] + one step shell [sh,-c] + one bare step → ClaimSteps carry [sh,-c] and [bash,-lc]; bare job → nil. uses: template with spec.shell → inlined steps stamped; template step with own shell keeps it; caller spec.shell does NOT override stamped values; undeclared template + caller spec.shell → inlined steps carry the caller value (resolution happens after inlining, so assert the final ClaimStep). post with own shell carried; post without → nil.
- [ ] **Step 2: FAIL → implement → PASS** (`go test ./internal/controller/ ./internal/gittemplate/ -count=1`).
- [ ] **Step 3: commit** `feat(controller): resolve effective shell argv onto claim steps; uses: templates stamp their declared shell`.

### Task 5: Host agent + runtime — /.ucd mount, keep-alive pause, exec threading

**Files:**
- Modify: `internal/runtime/runtime.go` (`Mount.ReadOnly bool`; `ExecSpec.Shell` doc: default now `{"/.ucd/ucd-sh","-c"}`), `internal/runtime/ocicli.go` (`:ro` suffix in mount args; keep createArgs Command semantics), `internal/runtime/apple.go` (parity)
- Modify: `internal/agent/agent.go` (startup: write `embedded.Bytes()` to `<workspaceDir>/../tools/ucd-sh` — pick the agent data dir used elsewhere; 0755)
- Modify: `internal/agent/claim_pod.go` (all containers +`{toolsDir, "/.ucd", ReadOnly:true}` mount; primary+pause Command → `["/.ucd/ucd-sh","pause"]`), `internal/agent/scope.go`, `internal/agent/workspace.go` (same mount + pause command)
- Modify: `internal/agent/orchestrator.go` + `internal/agent/backend_host.go` — thread `ClaimStep.Shell` into ExecSpec.Shell / RunStep: container paths: nil → `{"/.ucd/ucd-sh","-c"}`, else the argv; native path: nil → today's host bash behavior (RunStep unchanged), non-nil → exec the argv + [script] on the host
- Test: runtime argv tests (`:ro`), claim_pod_test (per-container Mounts + Commands), orchestrator/backend tests for shell threading incl. post inherit
- Test (integration, docker-gated): a claim-pod job in `alpine:3` (NO bash) with a default step runs green (proves shim exec + pause keep-alive end-to-end); `shell: [bash,-lc]` step in a debian image runs green

**Interfaces:**
- Consumes: Task 2 shim binary via Task 3 embed; Task 4 ClaimStep.Shell.

- [ ] **Step 1: failing unit tests (mount :ro, per-container pause command, shell threading, post inherit).**
- [ ] **Step 2: FAIL → implement → PASS** (`go test ./internal/runtime/ ./internal/agent/ -count=1`).
- [ ] **Step 3: docker-gated integration tests PASS** (`go test -tags integration ./internal/agent/ -run Shim -count=1`).
- [ ] **Step 4: commit** `feat(agent): inject /.ucd shim, ucd-sh pause keep-alives, shell argv exec threading`.

### Task 6: k8s agent — init container, volume, keep-alive fix, executor argv

**Files:**
- Modify: `internal/k8sagent/podbuilder.go` — `ucd-tools` emptyDir + volumeMount `/.ucd` on ALL containers; init container `{name: ucd-shim, image: cfg.ShimImage, command: ["/ucd-sh","--install","/.ucd/ucd-sh"], volumeMounts:[/.ucd]}`; `injectSleepInfinity` → `injectKeepAlive` with `["/.ucd/ucd-sh","pause"]`, skipping when the author set command OR args (clobber fix)
- Modify: `internal/k8sagent/config.go` (`ShimImage string`, default `"ghcr.io/eirueimi/unified-cd-k8s-agent:latest"`, yaml `shimImage`)
- Modify: `internal/k8sagent/executor.go` — `buildShellCommand(shell []string, script)` / `buildEnvShellCommand(shell, script, env)`: shell nil → `{"/.ucd/ucd-sh","-c"}`; env still applied via the `env` argv wrapper
- Modify: `internal/k8sagent/backend.go` — thread ClaimStep.Shell into ExecStep and post-hook exec
- Test: podbuilder tests (init container, volume on every container, keep-alive skip-on-args), executor argv tests, parity suite green

- [ ] **Step 1: failing tests → implement → PASS** (`go test ./internal/k8sagent/ -count=1`).
- [ ] **Step 2: commit** `feat(k8sagent): /.ucd shim via init container, pause keep-alive, shell argv executor`.

### Task 7: Compatibility corpus gate

**Files:**
- Create: `internal/shim/corpus_test.go`
- Test data: every `run:` block under `examples/` and `templates/` (walk with yaml, reuse the dsl walker pattern from examples_parse_test.go)

**Behavior:**
- Each extracted script runs through `shim.Run` with an `interp.ExecHandlers` stub: every external command returns success with empty output (builtins stay real). PASS = no parse error, no interp "unsupported" error, exit handling sane. Scripts whose step declares a non-default shell are skipped (none exist today).
- Construct pins (separate tests): `set -- a b; set -- "$@" c; echo $#` → 3; IFS-newline splitting loop; `until`/arithmetic; `a & b & wait` fan-out; `p=$!; wait $p` status collection; `trap 'echo x' EXIT` fires; `trap c TERM EXIT` sanitized (warn recorded, EXIT fires, exit 0); `wait -n` → clear error (pin the message); background command still running at script end — PIN the actual behavior (document whether interp waits or orphans; if it hangs, bound the test with a timeout and record the finding as a docs constraint).

- [ ] **Step 1: implement walker + pins; all corpus scripts PASS; pins PASS** (`go test ./internal/shim/ -run Corpus -count=1`).
- [ ] **Step 2: commit** `test(shim): compatibility corpus gate over examples/ and templates/ + construct pins`.

### Task 8: Docs, migration guide, examples touch-ups

**Files:**
- Modify: `docs/jobs.md` (new "Shell (`shell:`)" section: argv semantics, priority incl. uses:/post:/call:/native rules, the verified interp constraints table from the spec, `/.ucd` reserved path)
- Modify: `docs/kubernetes-integration.md` (exec model rewrite: shim replaces `bash -lc`; init container + shimImage config), `docs/configuration.md` (shimImage; podImage guidance — bash-less images now fine by default), `docs/agents.md` (host tools dir, native unchanged note)
- Create/modify: migration guide entry (follow the existing `docs/migration-2026-07-job-isolation.md` pattern): `-lc`→`-c` profile note, bashism jobs add `shell: [bash, -lc]`, constraint table link
- Verify: examples stay green under the dsl walkers; no example needs `shell:` (corpus gate proves it) — do NOT add shell: to examples

- [ ] **Step 1: write docs; regen docgen if field-reference is generated; dsl walkers PASS.**
- [ ] **Step 2: commit** `docs: shell: field, shim exec model, interp constraints, migration guide`.

### Task 9 (coordinator, post-merge): live verification — the user's explicit acceptance gate

Not a subagent task. After the feature merges to main and the dev stack rebuilds:

- [ ] Re-run every locally-runnable example under the NEW default (lc- renamed copies, established procedure): hello, params, matrix, parallel-steps, step-condition, concurrency-mutex, call-job, pod-sidecar, approval, artifacts, finally (both paths), cache, call-template, git-template, post-step-demo — all must reach the same terminal status as the pre-shim baseline run.
- [ ] k8s: lc-k8s-post (post output still lands in logs) AND a podTemplate-less job on the `golang:1.24-alpine` podImage cluster config — previously failing with `env: can't execute 'bash'`, must now SUCCEED (the headline proof).
- [ ] host: a claim-pod job with an `alpine:3` (bash-less) primary via podTemplate — succeeds under the shim; and one `shell: [bash, -lc]` job — succeeds.
- [ ] Report the full result table; any failure is triaged before the feature is declared done.

## Self-review notes

- Type consistency: `Shell []string` everywhere (dsl, api.ClaimStep, api.PostStep); argv order `shell + [script]` fixed in Global Constraints.
- Spec coverage: C1→T1/T4, C2→T2, C3→T3/T5/T6, C4→T5/T6, C5→T7, migration/docs→T8, user acceptance→T9.
- Known open point carried from spec: trap sanitizer uses AST rewrite (decided here, not CallHandler — no dependency on interp handler semantics).
