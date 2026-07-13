# Host Container Entrypoint Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make host claim-pod container `command`/`args` match k8s PodSpec semantics — `command` overrides the image ENTRYPOINT, `args` overrides CMD — and harden the `ucd-sh pause` keep-alive against ENTRYPOINT-bearing images.

**Architecture:** Split `runtime.CreateSpec.Command` into `Entrypoint` (ENTRYPOINT override, emitted as `--entrypoint "" <entrypoint...>` positional) and `Args` (CMD override, positional). The claim-pod builder maps a podTemplate container's `command`→`Entrypoint` and `args`→`Args` (no longer merged), and keep-alives set `Entrypoint: ucd-sh pause`. Runtimes that don't support `--entrypoint ""` clear degrade to positional args + a WARN.

**Tech Stack:** Go, docker/podman/nerdctl/wslc CLI (internal/runtime/ocicli.go), Apple `container` CLI (apple.go), the host claim pod (internal/agent).

**Spec:** docs/superpowers/specs/2026-07-13-host-entrypoint-parity-design.md — read it first; it is the authority on semantics.

## Global Constraints

- `CreateSpec.Entrypoint []string`: nil ⇒ use image ENTRYPOINT; non-nil ⇒ override. `CreateSpec.Args []string`: nil ⇒ use image CMD; non-nil ⇒ override. There is NO `Command` field after this work.
- createArgs tail, after `spec.Image`, is EXACTLY: nil/nil ⇒ nothing; Entrypoint nil + Args non-nil ⇒ `<args...>`; Entrypoint non-nil ⇒ `--entrypoint "" <entrypoint...> <args...>` (empty-string clear only — never a multi-element `--entrypoint` value).
- Degrade: a runtime whose name is in a package-level no-empty-clear set omits `--entrypoint ""` when Entrypoint is non-nil (emits `<entrypoint...> <args...>` positionally) and logs ONE WARN. That set starts EMPTY — add a runtime only after the manual verification in Task 2 proves it necessary.
- Keep-alive argv is EXACTLY `["/.ucd/ucd-sh","pause"]` and is set via `Entrypoint`, `Args` nil, for: claim-pod pause + primary "job" containers, uses-scope containers, workspace-cleanup container.
- k8s backend (internal/k8sagent) is NOT modified — it is already correct.
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

## Self-Review

**Spec coverage:** CreateSpec split → T1; createArgs truth table + degrade → T1 (steps 4-5); keep-alive via Entrypoint → T1 (steps 10-11); parseContainerDef split → T1 (step 9); k8s unchanged → (no task, correct); integration + runtime verification → T2; docs/migration → T3. All spec sections covered.

**Placeholder scan:** Integration test bodies (T2 step 1) are described-not-shown because they must mirror the existing `TestClaimPod_Integration_*` harness in the same file verbatim — the implementer copies a concrete existing pattern rather than inventing one; the assertions and images are exact. All code steps in T1 show full code.

**Type consistency:** `Entrypoint []string` / `Args []string` used identically on `CreateSpec` (runtime.go) and `containerDef` (claim_pod.go); `noEmptyEntrypointClear map[string]bool` referenced consistently in ocicli.go, apple.go, and the T1 degrade test; keep-alive argv `["/.ucd/ucd-sh","pause"]` (`ucdShPause`) set via `Entrypoint` in all four callers.
