# Host Container Entrypoint Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make host claim-pod container `command`/`args` match k8s PodSpec semantics — `command` overrides the image ENTRYPOINT, `args` overrides CMD — and harden the `ucd-sh pause` keep-alive against ENTRYPOINT-bearing images.

**Architecture:** Split `runtime.CreateSpec.Command` into `Entrypoint` (ENTRYPOINT override, emitted as `--entrypoint "" <entrypoint...>` positional) and `Args` (CMD override, positional). The claim-pod builder maps a podTemplate container's `command`→`Entrypoint` and `args`→`Args` (no longer merged), and keep-alives set `Entrypoint: ucd-sh pause`. Runtimes that don't support `--entrypoint ""` clear degrade to positional args + a WARN.

**Tech Stack:** Go, docker/podman/nerdctl/wslc CLI (internal/runtime/ocicli.go), Apple `container` CLI (apple.go), the host claim pod (internal/agent).

**Spec:** docs/superpowers/specs/2026-07-13-host-entrypoint-parity-design.md — read it first; it is the authority on semantics. Tasks 1–3 implement the entrypoint/args split; Tasks 4–7 implement the five additional host/k8s parity fixes in the spec's "Additional host/k8s parity fixes (audit findings #1–#5)" section.

## Global Constraints

- `CreateSpec.Entrypoint []string`: nil ⇒ use image ENTRYPOINT; non-nil ⇒ override. `CreateSpec.Args []string`: nil ⇒ use image CMD; non-nil ⇒ override. There is NO `Command` field after this work.
- createArgs tail, after `spec.Image`, is EXACTLY: nil/nil ⇒ nothing; Entrypoint nil + Args non-nil ⇒ `<args...>`; Entrypoint non-nil ⇒ `--entrypoint "" <entrypoint...> <args...>` (empty-string clear only — never a multi-element `--entrypoint` value).
- Degrade: a runtime whose name is in a package-level no-empty-clear set omits `--entrypoint ""` when Entrypoint is non-nil (emits `<entrypoint...> <args...>` positionally) and logs ONE WARN. That set starts EMPTY — add a runtime only after the manual verification in Task 2 proves it necessary.
- Keep-alive argv is EXACTLY `["/.ucd/ucd-sh","pause"]` and is set via `Entrypoint`, `Args` nil, for: claim-pod pause + primary "job" containers, uses-scope containers, workspace-cleanup container.
- k8s backend (internal/k8sagent) command/args handling for SIDECARS is NOT modified — it is already correct. The ONLY k8s changes in this work are: the primary `job` keep-alive (Task 4, fix #1), an early unnamed-container validation (Task 5, fix #5), and cache-restore hit/miss accuracy (Task 6, fix #4).
- Parity-fix directions are FIXED (spec §"Additional host/k8s parity fixes"): #1 both backends always force `ucd-sh pause` on the primary `job` container; #2 host WARNs and ignores `resources.requests` (limits unchanged); #3 non-string env `value` hard-errors on both backends; #4 k8s cache log made honest via a `UCD_CACHE_RESULT=hit|miss` stdout marker (exit code stays 0); #5 unnamed container hard-errors at pod-build time on both backends.
- Keep-alive argv is EXACTLY `["/.ucd/ucd-sh","pause"]` (`ucdShPause` host / `ucdKeepAliveArgv()` k8s) everywhere it appears.
- The repo vendors deps; local runs may need `GOFLAGS=-mod=mod` but must NOT commit go.mod/go.sum/vendor changes. English prose everywhere. Gates per task: `go build ./...`, `go vet ./...`, named test packages `-count=1`.

---

### Task 1: Runtime Entrypoint/Args split + claim-pod command/args split + keep-alive + all callers

This is one task because renaming `CreateSpec.Command` ripples to every caller atomically — the build only compiles once all are updated. It ends with a green build and passing unit tests at both the runtime and agent layers.

**Files:**
- Modify: `internal/runtime/runtime.go` (CreateSpec struct: replace `Command` with `Entrypoint` + `Args`)
- Modify: `internal/runtime/ocicli.go:96-97` (createArgs tail) + add the no-empty-clear set
- Modify: `internal/runtime/apple.go:86` (createArgs tail, same logic)
- Modify: `internal/agent/claim_pod.go` (containerDef struct ~20-31; parseContainerDef ~55-56; keep-alive pause ~269; sidecar/primary create ~281-294)
- Modify: `internal/agent/scope.go:82` (`Command: ucdShPause` → `Entrypoint: ucdShPause`)
- Modify: `internal/agent/workspace.go:98` (`Command: ucdShPause` → `Entrypoint: ucdShPause`)
- Test: `internal/runtime/ocicli_test.go` (or the existing createArgs argv test file — grep `createArgs` in `internal/runtime/*_test.go`), `internal/runtime/apple_test.go`, `internal/agent/claim_pod_test.go`

**Interfaces:**
- Produces: `runtime.CreateSpec{Entrypoint []string, Args []string}` (Command removed).
- Produces: `containerDef{Entrypoint []string, Args []string}` (Command removed) in internal/agent.

- [ ] **Step 1: Write the failing runtime argv tests**

Add to the runtime argv test file (mirror the existing `createArgs`/`runArgs` test style — find it with `grep -rln 'createArgs\|runArgs' internal/runtime/*_test.go`). Use a helper that finds the tail after the image; assert the three rows plus degrade:

```go
func TestOCICLICreateArgs_ArgsOnly_Positional(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "img", Args: []string{"serve", "--port", "80"}})
	// no --entrypoint anywhere; tail after image is exactly the args
	assert.NotContains(t, got, "--entrypoint")
	assert.Equal(t, []string{"img", "serve", "--port", "80"}, got[len(got)-4:])
}

func TestOCICLICreateArgs_EntrypointOverride_ClearsAndPositions(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "img", Entrypoint: []string{"kubectl"}, Args: []string{"get", "pods"}})
	// --entrypoint "" appears BEFORE the image; entrypoint+args are positional after it
	assert.Equal(t, []string{"--entrypoint", "", "img", "kubectl", "get", "pods"}, got[len(got)-6:])
}

func TestOCICLICreateArgs_NoEntrypointNoArgs_Bare(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "img"})
	assert.Equal(t, "img", got[len(got)-1]) // image is the last token, no tail
	assert.NotContains(t, got, "--entrypoint")
}

func TestOCICLICreateArgs_EntrypointOverride_DegradesOnNoClearRuntime(t *testing.T) {
	r := &ociCLI{bin: "fakeruntime"}
	// Force the degrade path by putting this runtime in the no-clear set for the test.
	noEmptyEntrypointClear["fakeruntime"] = true
	defer delete(noEmptyEntrypointClear, "fakeruntime")
	got := r.createArgs(CreateSpec{Image: "img", Entrypoint: []string{"kubectl"}, Args: []string{"get"}})
	assert.NotContains(t, got, "--entrypoint")                         // no clear emitted
	assert.Equal(t, []string{"img", "kubectl", "get"}, got[len(got)-3:]) // positional fallback (image ENTRYPOINT stays)
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/runtime/ -run 'CreateArgs' -count=1`
Expected: compile failure (`CreateSpec` has no `Entrypoint`/`Args`; `noEmptyEntrypointClear` undefined).

- [ ] **Step 3: Update CreateSpec (runtime.go)**

Replace the `Command` field:

```go
	// Entrypoint overrides the image's ENTRYPOINT (k8s container.command).
	// nil means "use the image's ENTRYPOINT". When non-nil, the runtime
	// clears the image ENTRYPOINT (docker `--entrypoint ""`) and runs
	// Entrypoint followed by Args as the container's argv.
	Entrypoint []string
	// Args overrides the image's CMD (k8s container.args): positional
	// arguments after the (possibly overridden) entrypoint. nil means "use
	// the image's CMD".
	Args []string
```

- [ ] **Step 4: Update ociCLI createArgs + add the degrade set (ocicli.go)**

First add the package-level degrade set (near the top of ocicli.go, after the imports):

```go
// noEmptyEntrypointClear names runtimes whose CLI does NOT support the
// `--entrypoint ""` empty-clear form. For those, an Entrypoint override
// degrades to positional args (the image ENTRYPOINT still runs) plus a WARN.
// Seeded empty; add a runtime only when real-binary verification proves it
// necessary (see the host-entrypoint-parity design doc).
var noEmptyEntrypointClear = map[string]bool{}
```

Then replace the createArgs tail — the doc comment (~66-69) and the last three lines (`args = append(args, spec.Image)` / `args = append(args, spec.Command...)` / `return args`) — with (note `--entrypoint ""` must be emitted BEFORE the image):

```go
	// An Entrypoint override clears the image ENTRYPOINT (docker
	// `--entrypoint ""`, which must precede the image) and runs
	// Entrypoint+Args as positional argv. Args-only leaves the image
	// ENTRYPOINT in place. See CreateSpec.Entrypoint/Args.
	if len(spec.Entrypoint) > 0 {
		if noEmptyEntrypointClear[r.bin] {
			slog.Warn("runtime does not support clearing the image ENTRYPOINT (--entrypoint \"\"); "+
				"running command as positional args — the image's own ENTRYPOINT still applies", "runtime", r.bin)
		} else {
			args = append(args, "--entrypoint", "")
		}
	}
	args = append(args, spec.Image)
	args = append(args, spec.Entrypoint...)
	args = append(args, spec.Args...)
	return args
```

Add `"log/slog"` to ocicli.go's imports if not present.

- [ ] **Step 5: Update appleContainer createArgs (apple.go:86)**

Apply the identical tail (Apple `container` uses `--entrypoint` like docker; it is currently unverified, so it participates in the same `noEmptyEntrypointClear` set via its name "container"). Add `"log/slog"` to apple.go's imports if not present:

```go
	if len(spec.Entrypoint) > 0 && !noEmptyEntrypointClear["container"] {
		args = append(args, "--entrypoint", "")
	} else if len(spec.Entrypoint) > 0 {
		slog.Warn("runtime does not support clearing the image ENTRYPOINT (--entrypoint \"\"); "+
			"running command as positional args — the image's own ENTRYPOINT still applies", "runtime", "container")
	}
	args = append(args, spec.Image)
	args = append(args, spec.Entrypoint...)
	args = append(args, spec.Args...)
	return args
```

Update apple.go's doc comment (:63) to describe Entrypoint/Args instead of Command.

- [ ] **Step 6: Run runtime tests**

Run: `go test ./internal/runtime/ -run 'CreateArgs' -count=1`
Expected: PASS (all four).

- [ ] **Step 7: Write the failing claim_pod split tests**

Add to `internal/agent/claim_pod_test.go` (mirror the existing parseContainerDef / Start tests — grep `parseContainerDef\|TestClaimPod` there):

```go
func TestParseContainerDef_CommandArgsSplit(t *testing.T) {
	def := parseContainerDef("web", map[string]any{
		"image":   "nginx",
		"command": []any{"nginx"},
		"args":    []any{"-g", "daemon off;"},
	})
	assert.Equal(t, []string{"nginx"}, def.Entrypoint)
	assert.Equal(t, []string{"-g", "daemon off;"}, def.Args)
}

func TestParseContainerDef_ArgsOnly(t *testing.T) {
	def := parseContainerDef("web", map[string]any{"image": "nginx", "args": []any{"-t"}})
	assert.Nil(t, def.Entrypoint)
	assert.Equal(t, []string{"-t"}, def.Args)
}

func TestParseContainerDef_Neither_BothNil(t *testing.T) {
	def := parseContainerDef("db", map[string]any{"image": "mysql:8"})
	assert.Nil(t, def.Entrypoint)
	assert.Nil(t, def.Args)
}
```

Also extend the existing keep-alive/Start test (the one asserting the primary "job" container's argv — grep `ucdShPause\|StartPauseFirst\|primaryContainerName` in claim_pod_test.go) to assert the primary is created with `Entrypoint: ["/.ucd/ucd-sh","pause"]` (the recorded CreateSpec's `Entrypoint`, not `Command`), and a `command`-bearing sidecar is created with `Entrypoint`/`Args` from its def.

- [ ] **Step 8: Run to verify they fail**

Run: `go test ./internal/agent/ -run 'ParseContainerDef|ClaimPod' -count=1`
Expected: compile failure (`containerDef` has no `Entrypoint`/`Args`).

- [ ] **Step 9: Split containerDef + parseContainerDef (claim_pod.go)**

Replace `containerDef.Command` (~26-31) with:

```go
	// Entrypoint is the podTemplate container's command (ENTRYPOINT override,
	// CreateSpec.Entrypoint); nil = use the image's ENTRYPOINT. Args is its
	// args (CMD override, CreateSpec.Args); nil = use the image's CMD. A
	// service sidecar (mysql, redis, ...) that sets neither runs its image's
	// own entrypoint. claimPodManager.Start forces the primary "job"
	// container's Entrypoint to ucd-sh pause regardless of what the
	// podTemplate set, so it stays alive as an exec target.
	Entrypoint []string
	Args       []string
```

Replace the two merge lines (:55-56) with:

```go
	def.Entrypoint = stringSlice(c["command"])
	def.Args = stringSlice(c["args"])
```

- [ ] **Step 10: Update Start (claim_pod.go ~281-294)**

Replace the `cmd := def.Command; if primary { cmd = ucdShPause }` block and the `Command: cmd` field:

```go
		// A service sidecar runs its own entrypoint/CMD (Entrypoint/Args from
		// its podTemplate); the primary "job" container is forced to the
		// ucd-sh pause keep-alive via Entrypoint (clearing whatever ENTRYPOINT
		// its image declares) so it stays alive as the step exec target.
		entrypoint, cargs := def.Entrypoint, def.Args
		if def.Name == primaryContainerName {
			entrypoint, cargs = ucdShPause, nil
		}
		mounts := append([]crt.Mount{{HostPath: m.workDir, ContainerPath: m.mountPath}}, ucdToolsMount(m.toolsDir)...)
		h, err := m.rt.Create(ctx, crt.CreateSpec{
			Image:            def.Image,
			Env:              def.Env,
			CPULimit:         def.CPULimit,
			MemLimit:         def.MemLimit,
			WorkDir:          m.mountPath,
			Mounts:           mounts,
			NetworkContainer: pause.ID,
			Entrypoint:       entrypoint,
			Args:             cargs,
		})
```

- [ ] **Step 11: Update the pause + scope + workspace keep-alive callers**

`claim_pod.go:269` — `Command: ucdShPause` → `Entrypoint: ucdShPause`.
`scope.go:82` — `Command: ucdShPause` → `Entrypoint: ucdShPause`.
`workspace.go:98` — `Command: ucdShPause` → `Entrypoint: ucdShPause`.

- [ ] **Step 12: Run the whole affected suite + build**

Run: `go build ./... && go vet ./... && go test ./internal/runtime/ ./internal/agent/ -count=1`
Expected: build clean, vet clean, all tests PASS. (This catches any other `CreateSpec{... Command:}` or `.Command` reference across the repo — grep `\.Command` and `Command:` under internal/ if the build fails, and fix each to Entrypoint/Args.)

- [ ] **Step 13: Commit**

```bash
git add internal/runtime/ internal/agent/
git commit -m "feat(runtime,agent): split container Command into Entrypoint+Args for k8s parity"
```

---

### Task 2: Docker-gated integration tests + runtime empty-clear verification

**Files:**
- Test: `internal/agent/claim_pod_integration_test.go` (add two cases; follow the existing `//go:build integration` + `runtime.Detect` skip pattern already in that file)

**Interfaces:**
- Consumes: Task 1's `CreateSpec.Entrypoint`/`Args`, the split `parseContainerDef`, and the pause-via-Entrypoint keep-alive.

- [ ] **Step 1: Write the entrypoint-override integration test**

Use an image with a known non-trivial ENTRYPOINT. `busybox` has none, so build the override case around an image that does: use `docker.io/library/httpd:2.4` (ENTRYPOINT is `httpd-foreground`) as a sidecar, override its command to a shim-independent probe, OR — simplest and hermetic — assert the PRIMARY keep-alive works on an ENTRYPOINT-bearing primary image. Add both:

```go
//go:build integration

// A primary "job" container whose IMAGE declares its own ENTRYPOINT must
// still keep alive under ucd-sh pause (Entrypoint override clears the image
// ENTRYPOINT). Before the Entrypoint split this ran `<image-entrypoint>
// /.ucd/ucd-sh pause` and the pod never became exec-able.
func TestClaimPod_Integration_EntrypointImagePrimaryKeepsAlive(t *testing.T) {
	skipIfNoDocker(t) // reuse the existing skip helper in this file
	// Build a job whose primary "job" container image has an ENTRYPOINT.
	// httpd:2.4's ENTRYPOINT is ["httpd-foreground"]; with the fix, ucd-sh
	// pause replaces it and a default step can exec into the container.
	// ... construct a claim with podTemplate job container image httpd:2.4,
	// run a default step `echo alive`, assert Succeeded and stdout "alive".
}

// A sidecar whose image has an ENTRYPOINT, overridden via command:, runs the
// override — proving host now matches k8s (command replaces ENTRYPOINT).
func TestClaimPod_Integration_SidecarCommandOverridesImageEntrypoint(t *testing.T) {
	skipIfNoDocker(t)
	// podTemplate sidecar image httpd:2.4 with command: ["sh","-c","echo OVERRIDE > /shared/marker; sleep infinity"]
	// (or a netns-reachable probe). A default step verifies the override ran,
	// not httpd-foreground. Follow the existing redis/mysql sidecar test shape
	// in this file for wiring (netns share, wait loop).
}
```

Write the FULL bodies following the existing `TestClaimPod_Integration_*` tests in this file (they already show how to build a claim, apply a podTemplate, run a step, and assert status/logs). Reuse their skip helper and harness verbatim — do not invent new scaffolding.

- [ ] **Step 2: Build the shim and run the integration tests against real Docker**

```bash
GOOS=linux GOARCH=$(go env GOARCH) go build -o internal/shim/embedded/ucd-sh-$(go env GOARCH) ./cmd/ucd-sh
go test -tags integration ./internal/agent/ -run 'Integration_(EntrypointImagePrimary|SidecarCommandOverrides)' -count=1 -v
```
Expected: both PASS. Then restore the placeholder: `git checkout -- internal/shim/embedded/` and verify `git status` shows only the test file changed.

- [ ] **Step 3: Manually verify `--entrypoint ""` empty-clear per runtime; record results**

For each runtime binary available on the machine, run and record the exit status + behavior:

```bash
for rt in docker podman nerdctl; do command -v $rt >/dev/null && { echo "== $rt =="; $rt run --rm --entrypoint "" alpine:3 echo ok; }; done
```
Expected: prints `ok` on runtimes that support empty-clear. Record which (if any) FAIL in the task report AND in the docs matrix (Task 3). If any fails, add its name to `noEmptyEntrypointClear` in `internal/runtime/ocicli.go` (Apple `container` and `wslc` are typically unavailable here — leave them out of the set unless verified failing; document them as "unverified" in the matrix). If you change `noEmptyEntrypointClear`, re-run `go test ./internal/runtime/ -count=1`.

- [ ] **Step 4: Commit**

```bash
git add internal/agent/claim_pod_integration_test.go internal/runtime/
git commit -m "test(agent): integration coverage for entrypoint override + keep-alive on entrypoint images"
```

---

### Task 3: Docs, migration guide, CHANGELOG

**Files:**
- Modify: `docs/jobs.md` (the podTemplate `command`/`args` section — grep `command`/`args` near the podTemplate docs)
- Modify: `docs/kubernetes-integration.md` (the host-vs-k8s container behavior note, if present)
- Modify/Create: the migration guide entry (follow the existing `docs/migration-2026-07-*.md` pattern)
- Modify: `CHANGELOG.md` if the repo has one (grep for it; skip if absent)

- [ ] **Step 1: Document the aligned semantics + per-runtime matrix**

In `docs/jobs.md`'s podTemplate container section, state that a container's `command:` overrides the image ENTRYPOINT and `args:` overrides CMD on BOTH backends now (host reaches parity via `--entrypoint ""` clear). Add the runtime support matrix from Task 2's verification: which runtimes support the empty-clear (and thus real ENTRYPOINT override) vs degrade to CMD-only + WARN. Note the keep-alive detail: the primary "job" container's own image ENTRYPOINT is always ignored (it runs `ucd-sh pause`).

- [ ] **Step 2: Migration note**

Add a migration entry: host `command:` semantics changed — previously the host merged `command`+`args` into positional CMD (image ENTRYPOINT always ran); now `command` replaces the ENTRYPOINT, matching k8s. Jobs that relied on the old host merge move their values to `args:`. `args:`-only and no-command sidecars are unaffected.

- [ ] **Step 3: dsl walkers still pass; commit**

Run: `go test ./internal/dsl/ -count=1`
Expected: PASS (no example changes, but confirm nothing regressed).

```bash
git add docs/ CHANGELOG.md
git commit -m "docs: host command/args now match k8s ENTRYPOINT/CMD semantics + runtime matrix"
```

### Task 4: k8s primary `job` keep-alive always forces pause (fix #1)

Make k8s match the host: the primary `job` container always runs `ucd-sh pause`, discarding any `command`/`args` the podTemplate set on it, so it stays a live exec target. Sidecars are untouched.

**Files:**
- Modify: `internal/k8sagent/podbuilder.go` (`injectKeepAlive` ~205-212, and its doc comment ~180-204)
- Test: `internal/k8sagent/podbuilder_test.go` (invert `TestInjectKeepAlive_JobKeepsExplicitCommand` / `...KeepsExplicitArgs`; keep the sidecar-untouched cases)

**Interfaces:**
- Consumes: `ucdKeepAliveArgv()`, `primaryContainerName` (unchanged).

- [ ] **Step 1: Invert the two failing tests**

In `podbuilder_test.go`, replace the bodies of `TestInjectKeepAlive_JobKeepsExplicitCommand` and `TestInjectKeepAlive_JobKeepsExplicitArgs` (rename each to `..._JobForcesPauseOverExplicitCommand` / `..._JobForcesPauseOverExplicitArgs`) to assert the primary is FORCED to pause even when it arrived with a command/args:

```go
func TestInjectKeepAlive_JobForcesPauseOverExplicitCommand(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{
		{Name: "job", Image: "img", Command: []string{"my-server", "--port", "80"}},
	}}
	injectKeepAlive(spec)
	// The primary job's own command is discarded — it must keep-alive.
	assert.Equal(t, []string{ucdMountPath + "/ucd-sh", "pause"}, spec.Containers[0].Command)
	assert.Nil(t, spec.Containers[0].Args)
}

func TestInjectKeepAlive_JobForcesPauseOverExplicitArgs(t *testing.T) {
	spec := &corev1.PodSpec{Containers: []corev1.Container{
		{Name: "job", Image: "img", Args: []string{"--flag"}},
	}}
	injectKeepAlive(spec)
	assert.Equal(t, []string{ucdMountPath + "/ucd-sh", "pause"}, spec.Containers[0].Command)
	assert.Nil(t, spec.Containers[0].Args)
}
```

Leave the sidecar-untouched test(s) (a non-`job` container with/without a command keeps its own command/nil) exactly as they are — grep `TestInjectKeepAlive` to find and preserve them.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/k8sagent/ -run 'InjectKeepAlive' -count=1`
Expected: the two inverted tests FAIL (current code skips injection when a command/args is set), sidecar tests PASS.

- [ ] **Step 3: Change injectKeepAlive**

Replace the loop body so the primary is unconditional and clears Args:

```go
func injectKeepAlive(podSpec *corev1.PodSpec) {
	for i := range podSpec.Containers {
		c := &podSpec.Containers[i]
		if c.Name == primaryContainerName {
			// The primary "job" container is the exec target for
			// container:-less steps, so it ALWAYS runs ucd-sh pause,
			// discarding any command/args the podTemplate set on it —
			// honoring a non-persistent user command would let the
			// container exit and break every later step's exec-in. This
			// matches the host claim pod (claimPodManager.Start forces the
			// primary's Entrypoint to ucd-sh pause). Sidecars are left
			// untouched: a sidecar with no command runs its image's own
			// entrypoint (its service); one with a command overrides it.
			c.Command = ucdKeepAliveArgv()
			c.Args = nil
		}
	}
}
```

Update the doc comment above `injectKeepAlive` (the paragraph describing the "only when Command AND Args are empty" guard and the args-clobber rationale) to describe the new always-force-on-primary behavior. Leave `defaultPodSpec`'s "Command intentionally left unset" comment as-is (it's still correct — the injected pause fills it), but drop its now-stale reference to injectKeepAlive "only injects when Command AND Args are both empty."

- [ ] **Step 4: Run tests + build**

Run: `go build ./... && go test ./internal/k8sagent/ -run 'InjectKeepAlive|BuildPod' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/podbuilder.go internal/k8sagent/podbuilder_test.go
git commit -m "fix(k8s): primary job container always keeps alive, matching host (parity #1)"
```

---

### Task 5: host container-def hardening + k8s unnamed-container validation (fixes #2, #3, #5)

Three host `parseContainerDef`/`claimContainerDefs` gaps plus the k8s side of the unnamed-container fix. `parseContainerDef` and `claimContainerDefs` gain an `error` return; `claimPodManager.Start` propagates it. NOTE: Task 1 already converted this file's `Command` field to `Entrypoint`/`Args` and changed `Start`'s per-container block — write against that post-Task-1 shape (the file you read will already have `Entrypoint`/`Args`).

**Files:**
- Modify: `internal/agent/claim_pod.go` (`parseContainerDef` ~44-87 → returns error; `claimContainerDefs` ~139-160 → returns error, unnamed hard-error; `Start` ~274 → propagate error)
- Modify: `internal/k8sagent/podbuilder.go` (`BuildPod` ~130-135, add empty-name validation to the existing reserved-name guard loop)
- Test: `internal/agent/claim_pod_test.go`, `internal/k8sagent/podbuilder_test.go`

**Interfaces:**
- Produces: `parseContainerDef(name string, c map[string]any) (containerDef, error)`; `claimContainerDefs(pt *dsl.PodTemplate, runnerImage string) ([]containerDef, error)`.

- [ ] **Step 1: Write the failing host tests**

Add to `claim_pod_test.go` (adjust existing `parseContainerDef`/`claimContainerDefs` test call sites to the new two-value returns — grep them first):

```go
func TestParseContainerDef_ResourcesRequestsWarnsIgnored(t *testing.T) {
	// requests present → parsed OK (no error), limits still applied, requests ignored.
	def, err := parseContainerDef("web", map[string]any{
		"image": "nginx",
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "500m", "memory": "256Mi"},
			"limits":   map[string]any{"cpu": "1", "memory": "512Mi"},
		},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, def.CPULimit) // limits honored
	assert.NotEmpty(t, def.MemLimit)
}

func TestParseContainerDef_NonStringEnvValueErrors(t *testing.T) {
	_, err := parseContainerDef("web", map[string]any{
		"image": "nginx",
		"env":   []any{map[string]any{"name": "PORT", "value": 8080}}, // number, not string
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "value must be a string")
}

func TestParseContainerDef_MissingEnvValueStillSkipsNoError(t *testing.T) {
	// No `value` key at all (valueFrom-style) → still WARN+skip, NOT an error.
	def, err := parseContainerDef("web", map[string]any{
		"image": "nginx",
		"env":   []any{map[string]any{"name": "SECRET"}},
	})
	require.NoError(t, err)
	assert.Empty(t, def.Env)
}

func TestClaimContainerDefs_UnnamedContainerErrors(t *testing.T) {
	pt := &dsl.PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"image": "nginx"}, // no name
	}}}
	_, err := claimContainerDefs(pt, "runner:latest")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no name")
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/agent/ -run 'ParseContainerDef|ClaimContainerDefs' -count=1`
Expected: compile failure (single-value returns) → after fixing call sites, the new assertions fail.

- [ ] **Step 3: parseContainerDef returns error; add #2 WARN and #3 hard-error**

Change the signature to `func parseContainerDef(name string, c map[string]any) (containerDef, error)`. In the env loop, split "no value key" from "wrong-typed value":

```go
	if envs, ok := c["env"].([]any); ok {
		for _, raw := range envs {
			e, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			en, _ := e["name"].(string)
			if en == "" {
				continue
			}
			rawVal, present := e["value"]
			if !present {
				// valueFrom / fieldRef etc. — not resolvable on the host.
				slog.Warn("podTemplate container env without a literal value is ignored on the host agent",
					"container", name, "env", en)
				continue
			}
			ev, ok := rawVal.(string)
			if !ok {
				// A malformed job: k8s hard-errors on this (json.Unmarshal into
				// EnvVar{Value string}); the host matches instead of silently dropping.
				return containerDef{}, fmt.Errorf("podTemplate container %q env %q: value must be a string (got %T); quote the value", name, en, rawVal)
			}
			def.Env = append(def.Env, en+"="+ev)
		}
	}
```

After the `resources.limits` block, add the #2 requests WARN:

```go
	if res, ok := c["resources"].(map[string]any); ok {
		if lim, ok := res["limits"].(map[string]any); ok {
			cpu, _ := lim["cpu"].(string)
			mem, _ := lim["memory"].(string)
			def.CPULimit, def.MemLimit = limitStrings(cpu, mem)
		}
		if reqs, ok := res["requests"].(map[string]any); ok && len(reqs) > 0 {
			slog.Warn("podTemplate container resources.requests is not supported on the host agent "+
				"(docker/podman have no request concept) and is ignored; use resources.limits or route to a Kubernetes agent",
				"container", name)
		}
	}
	return def, nil
```

Ensure `fmt` is imported in claim_pod.go (it is — `Start` uses `fmt.Errorf`).

- [ ] **Step 4: claimContainerDefs returns error; #5 unnamed hard-error**

Change the signature to `func claimContainerDefs(pt *dsl.PodTemplate, runnerImage string) ([]containerDef, error)`. Replace the `name == ""` silent `continue` with a hard error, and thread `parseContainerDef`'s error:

```go
		for idx, raw := range containers {
			c, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			name, _ := c["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("podTemplate container at index %d has no name", idx)
			}
			if seen[name] {
				slog.Warn("podTemplate has more than one container with the same name; keeping the first and dropping the duplicate", "container", name)
				continue
			}
			seen[name] = true
			def, err := parseContainerDef(name, c)
			if err != nil {
				return nil, err
			}
			defs = append(defs, def)
		}
```

At every `return defs` in `claimContainerDefs` (including the injected-primary tail), change to `return defs, nil`.

- [ ] **Step 5: Start propagates the error**

In `Start`, change the loop head:

```go
	defs, err := claimContainerDefs(pt, m.runnerImage)
	if err != nil {
		return fmt.Errorf("claim pod: %w", err)
	}
	for _, def := range defs {
```

- [ ] **Step 6: Write the failing k8s unnamed-container test**

Add to `podbuilder_test.go` (mirror the existing reserved-name guard test — grep `is reserved for the artifact sidecar`):

```go
func TestBuildPod_UnnamedContainerErrors(t *testing.T) {
	at := &dsl.AgentTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"image": "nginx"}, // no name
	}}}
	_, err := BuildPod(buildPodInputWith(at)) // use this file's existing BuildPod test harness/inputs
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no name")
}
```

Use the same `BuildPod` invocation shape the existing tests in this file use (grep `BuildPod(` in `podbuilder_test.go` and copy the harness — do not invent new inputs).

- [ ] **Step 7: Add the k8s empty-name validation**

In `BuildPod`, extend the existing guard loop (the one checking `artifactSidecarName`) to also reject empty names, before the Pod is returned:

```go
	// Guard against user-supplied containers using the reserved sidecar name
	// or having no name (k8s would otherwise reject the latter only later, at
	// the API server, as an opaque run-creation failure — fail early here to
	// match the host claimContainerDefs check).
	for i, c := range podSpec.Containers {
		if c.Name == "" {
			return nil, fmt.Errorf("podTemplate container at index %d has no name", i)
		}
		if c.Name == artifactSidecarName {
			return nil, fmt.Errorf("container name %q is reserved for the artifact sidecar", artifactSidecarName)
		}
	}
```

(Replace the existing reserved-name-only loop with this combined loop; the injected `job`/sidecar/shim containers all have names, so they pass.)

- [ ] **Step 8: Run the affected suites + build**

Run: `go build ./... && go vet ./... && go test ./internal/agent/ ./internal/k8sagent/ -count=1`
Expected: build/vet clean, all PASS (fix any remaining `parseContainerDef(`/`claimContainerDefs(` single-value call sites the build flags).

- [ ] **Step 9: Commit**

```bash
git add internal/agent/claim_pod.go internal/agent/claim_pod_test.go internal/k8sagent/podbuilder.go internal/k8sagent/podbuilder_test.go
git commit -m "fix(agent,k8s): host resources.requests warn + non-string env & unnamed-container hard errors (parity #2,#3,#5)"
```

---

### Task 6: k8s cache restore hit/miss accuracy (fix #4)

The `unified-sidecar cache restore` already knows hit vs miss but always exits 0, and k8s `CacheRestore` reports every exit-0 as a hit. Emit a stdout marker from the sidecar and parse it in `CacheRestore`, keeping the best-effort exit-0 contract.

**Files:**
- Modify: `cmd/unified-sidecar/run.go` (`runCache` "restore" branch ~76-84: add stdout marker)
- Modify: `internal/k8sagent/backend.go` (`CacheRestore` ~161-181 capture+parse stdout; add a stdout-capturing exec variant near `sidecarExecArgv` ~235-244)
- Test: `cmd/unified-sidecar/run_test.go`, `internal/k8sagent/backend_test.go` (if a `CacheRestore`/marker-parse unit is feasible; otherwise a focused parse-helper test)

**Interfaces:**
- Produces: the stable marker strings `UCD_CACHE_RESULT=hit` / `UCD_CACHE_RESULT=miss` on the sidecar's stdout.
- Produces (internal): a `parseCacheResult(stdout string) (hit bool)` helper in backend.go, defaulting to `true` when no marker is present.

- [ ] **Step 1: Write the failing sidecar marker test**

In `run_test.go` (mirror the existing `runCache`/`run` test that captures stderr — grep `runCache\|cache restore` there), assert the restore branch writes the marker to STDOUT (separate from the human line on stderr). If the existing harness only captures one stream, extend it to capture stdout too:

```go
func TestRunCache_RestoreEmitsHitMarkerToStdout(t *testing.T) {
	// store seeded so Restore returns hit=true (follow the existing seeding helper)
	var stdout, stderr bytes.Buffer
	ec := runCacheWithStdout(context.Background(), seededStore(t, "k"), "restore",
		[]string{"--key", "k", "--path", t.TempDir()}, &stdout, &stderr)
	assert.Equal(t, 0, ec)
	assert.Contains(t, stdout.String(), "UCD_CACHE_RESULT=hit")
}

func TestRunCache_RestoreEmitsMissMarkerToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ec := runCacheWithStdout(context.Background(), emptyStore(t), "restore",
		[]string{"--key", "absent", "--path", t.TempDir()}, &stdout, &stderr)
	assert.Equal(t, 0, ec)
	assert.Contains(t, stdout.String(), "UCD_CACHE_RESULT=miss")
}
```

If `runCache` currently takes only `stderr`, add a `stdout io.Writer` parameter (thread it from `run`'s existing stdout) rather than inventing a parallel function — update the existing signature and all call sites; the test names above then use `runCache` directly.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cmd/unified-sidecar/ -run 'RestoreEmits' -count=1`
Expected: FAIL (no marker on stdout yet).

- [ ] **Step 3: Emit the marker from the sidecar restore branch**

Thread a `stdout io.Writer` into `runCache` (add the param, pass `os.Stdout`/the real stdout from `run`). In the "restore" branch, on the hit and miss branches, write the marker to stdout (keep the existing human line on stderr):

```go
		hit, err := cache.Restore(ctx, store, *path, *key, restoreKeys)
		if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
			fmt.Fprintf(stderr, "cache restore error (ignored): %v\n", err)
			// error path: leave no marker → CacheRestore keeps its lenient default (hit=true)
		} else if hit {
			fmt.Fprintf(stderr, "cache hit: %s\n", *key)
			fmt.Fprintln(stdout, "UCD_CACHE_RESULT=hit")
		} else {
			fmt.Fprintf(stderr, "cache miss: %s\n", *key)
			fmt.Fprintln(stdout, "UCD_CACHE_RESULT=miss")
		}
		return 0 // best-effort: never fail the step
```

Update the file-top comment "Cache operations are best-effort (always exit 0 ...)" to add: restore additionally emits a `UCD_CACHE_RESULT=hit|miss` marker on stdout for the caller to distinguish, without affecting the exit code.

- [ ] **Step 4: Run to verify the sidecar tests pass**

Run: `go test ./cmd/unified-sidecar/ -run 'RestoreEmits' -count=1`
Expected: PASS.

- [ ] **Step 5: Capture + parse the marker in k8s CacheRestore**

Add a stdout-capturing exec variant beside `sidecarExecArgv` (it currently discards stdout via `io.Discard`):

```go
// sidecarExecArgvCapturingStdout is sidecarExecArgv but returns the sidecar's
// stdout (still shipping stderr to the log pusher). Used by CacheRestore to read
// the UCD_CACHE_RESULT marker.
func (b *k8sBackend) sidecarExecArgvCapturingStdout(ctx context.Context, targetPod, container string, argv []string) (int, string, error) {
	if targetPod == "" {
		targetPod = b.podName
	}
	stderrPusher := agentlib.NewLogPusher(b.a.client, b.a.cfg.AgentID, b.runID, 0, "stderr")
	stderrPusher.SetMasker(b.masker)
	var stdout bytes.Buffer
	ec, err := b.a.exec.ExecStepArgv(ctx, targetPod, container, argv, &stdout, stderrPusher)
	stderrPusher.Flush(ctx)
	return ec, stdout.String(), err
}

// parseCacheResult reads the UCD_CACHE_RESULT marker from the sidecar's stdout.
// Absent marker (older sidecar, or the error path that emits none) defaults to a
// hit, preserving the historical lenient best-effort behavior.
func parseCacheResult(stdout string) bool {
	return !strings.Contains(stdout, "UCD_CACHE_RESULT=miss")
}
```

Change `CacheRestore` to use it:

```go
	ec, stdout, err := b.sidecarExecArgvCapturingStdout(ctx, targetPod, sidecar, argv)
	if err != nil {
		return false, err
	}
	// exit code stays best-effort (0 on hit/miss/error); the true hit/miss comes
	// from the sidecar's UCD_CACHE_RESULT stdout marker so the orchestrator logs
	// an accurate hit/miss (parity with the host's ErrCacheMiss-based bool).
	_ = ec
	return parseCacheResult(stdout), nil
```

Add `"bytes"` and `"strings"` to backend.go imports if missing. (Leave `sidecarExecArgv` in place — `CacheSave` and other callers still use it.)

- [ ] **Step 6: Test + build**

Run: `go build ./... && go test ./cmd/unified-sidecar/ ./internal/k8sagent/ -count=1`
Expected: PASS. If a direct `CacheRestore` unit isn't feasible without a live pod, at least add a `parseCacheResult` table test (hit marker → true; miss marker → false; empty → true).

- [ ] **Step 7: Commit**

```bash
git add cmd/unified-sidecar/ internal/k8sagent/backend.go internal/k8sagent/backend_test.go
git commit -m "fix(k8s,sidecar): honest cache hit/miss via UCD_CACHE_RESULT stdout marker (parity #4)"
```

---

### Task 7: Docs + migration for the parity fixes

**Files:**
- Modify: `docs/jobs.md` and/or `docs/kubernetes-integration.md` (podTemplate container semantics — grep `podTemplate`, `resources`, `command` near the container docs)
- Modify/append: the migration guide from Task 3 (same file) — add the parity-fix behavior changes
- Modify: `CHANGELOG.md` if present

- [ ] **Step 1: Document the aligned behaviors**

Add to the podTemplate container docs: (#1) the primary `job` container always runs the keep-alive on BOTH backends — a `command`/`args` set on `job` is ignored (put your workload in steps, not the job container's command); (#2) `resources.requests` is ignored on the host agent (docker/podman have no request concept) and logs a WARN — use `resources.limits`, or run on a Kubernetes agent for requests; (#3) an env `value` must be a string — an unquoted number/bool (`value: 8080`) is now a hard error on both backends (quote it: `value: "8080"`); (#5) every podTemplate container must have a `name` — an unnamed container is now a hard error at job start on both backends.

- [ ] **Step 2: Migration note**

Append to the migration guide: these four are behavior changes — jobs that previously relied on a `command` on the `job` container running (k8s only), an unquoted numeric env value (host silently dropped it), or an unnamed container (host silently dropped it) will now be corrected/rejected. Give the one-line fix for each (move workload to steps; quote the value; add a name). #4 is log-accuracy only (no user action).

- [ ] **Step 3: Confirm nothing regressed; commit**

Run: `go test ./internal/dsl/ -count=1`
Expected: PASS.

```bash
git add docs/ CHANGELOG.md
git commit -m "docs: host/k8s container parity fixes (#1-#5) behavior + migration notes"
```

---

## Self-Review

**Spec coverage:** CreateSpec split → T1; createArgs truth table + degrade → T1 (steps 4-5); keep-alive via Entrypoint → T1 (steps 10-11); parseContainerDef split → T1 (step 9); integration + runtime verification → T2; entrypoint docs/migration → T3. Parity fix #1 (k8s always-pause) → T4; #2 requests WARN + #3 non-string env hard-error + #5 unnamed hard-error (host) + #5 k8s validation → T5; #4 cache marker → T6; parity docs/migration → T7. All spec sections (including the "Additional host/k8s parity fixes" section) covered.

**Placeholder scan:** Integration test bodies (T2 step 1) are described-not-shown because they must mirror the existing `TestClaimPod_Integration_*` harness in the same file verbatim — the implementer copies a concrete existing pattern rather than inventing one; the assertions and images are exact. All code steps in T1 show full code. T4–T6 show full code; the two spots that say "mirror the existing harness" (T5 step 6 k8s `BuildPod` test inputs, T6 step 1 sidecar stdout capture) point at a concrete existing test to copy, not an invented one.

**Type consistency:** `Entrypoint []string` / `Args []string` used identically on `CreateSpec` (runtime.go) and `containerDef` (claim_pod.go); `noEmptyEntrypointClear map[string]bool` referenced consistently in ocicli.go, apple.go, and the T1 degrade test; keep-alive argv `["/.ucd/ucd-sh","pause"]` (`ucdShPause` host / `ucdKeepAliveArgv()` k8s) set via `Entrypoint`/`Command` in every caller including T4's always-pause primary. Parity: `parseContainerDef` → `(containerDef, error)` and `claimContainerDefs` → `([]containerDef, error)` used consistently across T5's call-site updates and `Start`; the marker string `UCD_CACHE_RESULT=hit|miss` is identical in the T6 sidecar emitter and the `parseCacheResult` consumer; the empty-name error message `"...container at index %d has no name"` matches between host `claimContainerDefs` and k8s `BuildPod`.

**Cross-task ordering:** T5 edits the same `claim_pod.go` `Start`/container-def code that T1 already converted from `Command` to `Entrypoint`/`Args`; T5's steps are written against the post-T1 shape and only add the `error` return + #2/#3/#5 logic (they do not touch Entrypoint/Args). Running T1→T5 in order is required.
