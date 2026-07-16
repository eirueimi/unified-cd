# Path Hardening Wave 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the AppSource git option-injection (controller RCE), reject `workingDir` on step-targeted containers (G5 desync), switch OCI bind mounts to the Windows-safe `--mount` form (G6), and close the G2/G3 doc gaps.

**Architecture:** Validation at the resource boundary (`internal/dsl` — new shared `ValidateGitRef`, AppSource field hardening, workingDir rule) plus defense-in-depth at the git argv sites (`internal/gittemplate/fetch.go`) and a construction-only change in `internal/runtime/ocicli.go`.

**Tech Stack:** Go; existing table-test harnesses (`internal/dsl` parse tests, `internal/runtime/ocicli_lifecycle_test.go`).

## Global Constraints

- **Security first:** `spec.repoURL` must match a scheme allowlist (`https://`, `http://`, `ssh://`, or scp-like `git@host:path`) and never start with `-`. `spec.targetRevision` must satisfy the shared git-ref allowlist (start alphanumeric; charset `[A-Za-z0-9._/+-]`). `spec.path` must not start with `-` (plus the existing `..` rejection).
- The git-ref allowlist becomes `dsl.ValidateGitRef` (single source); `gittemplate`'s `refAllowed` (uri.go:9) delegates to it. `gittemplate` imports `dsl` already; `dsl` must NOT import `gittemplate`.
- Argv defense-in-depth regardless of Validate: `--` before the ls-tree path; explicit leading-`-` repoURL rejection in `ResolveCommitSHA` and `fetchDirWithURL`.
- **G5 rule (locked):** `workingDir` on a STEP-TARGETED container (the `job` container, or any container named by a step's `container:`) is an apply-time error for inline podTemplates (incl. `override.containers`). Sidecars may keep `workingDir`. Named agent-side templates: build-time check in podbuilder for the `job` container only.
- **G6:** `ociCLI.createArgs` emits `--mount type=bind,source=...,target=...[,readonly]`; paths containing `,` or `=` are rejected with a clear error. `apple.go` stays on `-v` (no drive-letter colon problem on macOS) with a comment.
- All new failures are deterministic errors naming the offending field/value.
- Full suite before push: `go test ./... -count=1`; no `-race` (CGO disabled). `go generate ./...` must stay drift-free.

---

### Task 1: `dsl.ValidateGitRef` + AppSource field hardening

**Files:**
- Create: `internal/dsl/gitref.go`
- Modify: `internal/dsl/appsource_parse.go` (`Validate`)
- Modify: `internal/gittemplate/uri.go` (delegate `refAllowed`)
- Test: `internal/dsl/gitref_test.go` (create), `internal/dsl/appsource_parse_test.go` (extend — find the existing file; if named differently, use it)

**Interfaces:**
- Produces: `func ValidateGitRef(ref string) error`; `func ValidateGitRepoURL(url string) error` (scheme allowlist + no leading `-`).

- [ ] **Step 1: Write the failing tests**

Create `internal/dsl/gitref_test.go`:

```go
package dsl

import "testing"

func TestValidateGitRef(t *testing.T) {
	for _, ok := range []string{"main", "v1.2.3", "feature/x", "abc123DEF", "release-2026.07+build"} {
		if err := ValidateGitRef(ok); err != nil {
			t.Errorf("%q should be a valid ref: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "-main", "--upload-pack=x", "HEAD~1", "main@{upstream}", "a b", "^caret"} {
		if err := ValidateGitRef(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestValidateGitRepoURL(t *testing.T) {
	for _, ok := range []string{
		"https://github.com/org/repo.git",
		"http://internal.example/repo.git",
		"ssh://git@host/org/repo.git",
		"git@github.com:org/repo.git",
	} {
		if err := ValidateGitRepoURL(ok); err != nil {
			t.Errorf("%q should be a valid repo URL: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"",
		"--upload-pack=touch /tmp/pwned",
		"-o=x",
		"ext::sh -c whoami",
		"file:///etc",
		"/local/path",
		"github.com/org/repo", // no scheme
	} {
		if err := ValidateGitRepoURL(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
```

Extend the AppSource validate tests (grep `func TestAppSource` in `internal/dsl/` for the file):

```go
func TestAppSourceValidate_GitArgvHardening(t *testing.T) {
	base := func() AppSource {
		return AppSource{
			APIVersion: SupportedAPIVersion, Kind: "AppSource",
			Metadata: Metadata{Name: "a"},
			Spec: AppSourceSpec{RepoURL: "https://github.com/org/repo.git", TargetRevision: "main", Path: "apps/x"},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid appsource must pass: %v", err)
	}
	cases := map[string]func(*AppSource){
		"upload-pack repoURL": func(a *AppSource) { a.Spec.RepoURL = "--upload-pack=touch /tmp/pwned" },
		"schemeless repoURL":  func(a *AppSource) { a.Spec.RepoURL = "github.com/org/repo" },
		"dash revision":       func(a *AppSource) { a.Spec.TargetRevision = "-main" },
		"tilde revision":      func(a *AppSource) { a.Spec.TargetRevision = "HEAD~1" },
		"dash path":           func(a *AppSource) { a.Spec.Path = "--output=/etc/x" },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			a := base()
			mut(&a)
			if err := a.Validate(); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}
```

(Adjust the `AppSourceSpec` field construction to the real struct in `internal/dsl/appsource_types.go` — check required fields like SyncPolicy; add minimal valid values.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dsl/ -run 'ValidateGitRef|ValidateGitRepoURL|GitArgvHardening' -count=1`
Expected: FAIL (undefined functions; hardening absent).

- [ ] **Step 3: Implement**

Create `internal/dsl/gitref.go`:

```go
package dsl

import (
	"fmt"
	"regexp"
	"strings"
)

// gitRefRe is the shared allowlist for user-supplied git refs (branches, tags,
// full SHAs). Anchored to start alphanumeric so a ref can never be parsed as a
// git option (-... / --...), and restricted to a conservative charset that
// excludes relative-ref syntax (HEAD~1, @{upstream}) and shell metacharacters.
// internal/gittemplate's uses:// URI parsing delegates here; AppSource
// targetRevision validation uses it directly.
var gitRefRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/+-]*$`)

// ValidateGitRef rejects a ref that could inject git options or relative-ref
// syntax into a git argv.
func ValidateGitRef(ref string) error {
	if ref == "" {
		return fmt.Errorf("git ref is required")
	}
	if !gitRefRe.MatchString(ref) {
		return fmt.Errorf("git ref %q contains invalid characters (must start alphanumeric; allowed: A-Z a-z 0-9 . _ / + -)", ref)
	}
	return nil
}

// ValidateGitRepoURL restricts a repository URL to network transports the
// controller expects: https://, http://, ssh://, or scp-like git@host:path.
// This blocks git option injection (a URL starting with '-' would be read as
// an option by ls-remote/fetch), local/ext transports (file://, ext:: — which
// can execute commands), and schemeless strings.
func ValidateGitRepoURL(url string) error {
	if url == "" {
		return fmt.Errorf("repo URL is required")
	}
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("repo URL %q must not start with '-'", url)
	}
	switch {
	case strings.HasPrefix(url, "https://"), strings.HasPrefix(url, "http://"), strings.HasPrefix(url, "ssh://"):
		return nil
	}
	// scp-like: git@host:path (no scheme). Require the git@host: shape.
	if m := regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`).MatchString(url); m {
		return nil
	}
	return fmt.Errorf("repo URL %q must use https://, http://, ssh://, or scp-like user@host: form", url)
}
```

In `internal/dsl/appsource_parse.go` `Validate`, replace the three bare non-empty checks' section with (keep the empty checks; add after them):

```go
	if err := ValidateGitRepoURL(a.Spec.RepoURL); err != nil {
		return fmt.Errorf("spec.repoURL: %w", err)
	}
	if err := ValidateGitRef(a.Spec.TargetRevision); err != nil {
		return fmt.Errorf("spec.targetRevision: %w", err)
	}
	if strings.HasPrefix(a.Spec.Path, "-") {
		return fmt.Errorf("spec.path %q must not start with '-'", a.Spec.Path)
	}
```

In `internal/gittemplate/uri.go`, delete the local `refAllowed` var and replace its use (uri.go ~40-42) with `dsl.ValidateGitRef(ref)` (adapt the error wrapping to keep the existing message shape — the troubleshooting docs grep for `contains invalid characters`, which the new dsl message preserves). Add the `dsl` import.

- [ ] **Step 4: Run tests + affected packages**

Run: `go test ./internal/dsl/ ./internal/gittemplate/ -count=1`
Expected: PASS (uri tests still green — verify the error-message-asserting test in `uri_test.go` still matches; adjust ITS expectation only if it asserted the exact old wording and the new wording is deliberate).

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/ internal/gittemplate/uri.go
git commit -m "feat(dsl): shared git ref/repoURL allowlists; harden AppSource validation (option-injection RCE)"
```

---

### Task 2: fetch.go argv defense-in-depth

**Files:**
- Modify: `internal/gittemplate/fetch.go`
- Test: `internal/gittemplate/fetch_argv_test.go` (create)

- [ ] **Step 1: Write the failing test**

The functions run real git; test the GUARDS (which fire before any exec). Create `internal/gittemplate/fetch_argv_test.go` (check `ResolveCommitSHA`/`FetchDir`'s receiver + signature in fetch.go and the exported wrapper the controller uses — write the test against whichever is callable; if only reachable via the `Fetcher` struct, construct it zero-value):

```go
package gittemplate

import (
	"context"
	"strings"
	"testing"
)

func TestResolveCommitSHA_RejectsDashURL(t *testing.T) {
	f := &Fetcher{}
	_, err := f.ResolveCommitSHA(context.Background(), "--upload-pack=touch /tmp/pwn", "main", "", "")
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("dash repoURL must be rejected before exec, got %v", err)
	}
}

func TestFetchDir_RejectsDashURL(t *testing.T) {
	f := &Fetcher{}
	_, err := f.FetchDir(context.Background(), "--upload-pack=x", "main", "p", "", "")
	if err == nil || !strings.Contains(err.Error(), "must not start with '-'") {
		t.Fatalf("dash repoURL must be rejected before exec, got %v", err)
	}
}
```

(Verify the exact method names/signatures — grep `func (f \*Fetcher)` in fetch.go; the controller interface at `internal/controller/appsource_reconciler.go:50` names them. Adjust arities.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/gittemplate/ -run 'RejectsDashURL' -count=1`
Expected: FAIL (no guard — git exec fails with a different message, or hangs; guards absent).

- [ ] **Step 3: Implement**

In `fetch.go`:
- At the top of `ResolveCommitSHA` and of `fetchDirWithURL` (or their exported entry points — wherever repoURL first arrives), add:

```go
	if strings.HasPrefix(repoURL, "-") {
		return "", fmt.Errorf("repo URL %q must not start with '-' (git option injection)", repoURL)
	}
```

(adapt return shape per function.)
- ls-tree site (~fetch.go:290-293): insert `--` before the path:

```go
	lsArgs := []string{"ls-tree", "-r", "--name-only", "FETCH_HEAD"}
	if treePath != "" {
		lsArgs = append(lsArgs, "--", treePath)
	}
```

Confirm ls-tree accepts `-- <path>` (it does; path filters follow `--`).

- [ ] **Step 4: Run tests + package**

Run: `go test ./internal/gittemplate/ -count=1`
Expected: PASS (fetchdir tests exercise ls-tree with real git — the `--` must not break them).

- [ ] **Step 5: Commit**

```bash
git add internal/gittemplate/fetch.go internal/gittemplate/fetch_argv_test.go
git commit -m "fix(gittemplate): argv defense — reject dash repo URLs, guard ls-tree path with --"
```

---

### Task 3: reject `workingDir` on step-targeted containers (G5)

**Files:**
- Modify: `internal/dsl/parse.go` (Job.Validate rule) — helper may live in `internal/dsl/container.go`
- Modify: `internal/k8sagent/podbuilder.go` (named-template `job`-container build-time check)
- Test: `internal/dsl/workingdir_test.go` (create), k8sagent podbuilder test extension

**Interfaces:**
- Produces: `func validateStepTargetedWorkingDir(spec Spec) error` (unexported, dsl): collects step-targeted container names (`"job"` + every step's non-empty `Container`, incl. parallel and finally), then errors if any inline podTemplate container OR override container with one of those names declares a `workingDir` key.

- [ ] **Step 1: Write the failing tests**

Create `internal/dsl/workingdir_test.go`:

```go
package dsl

import (
	"strings"
	"testing"
)

func wdJob(container string, podTpl string) string {
	return `apiVersion: unified-cd/v1
kind: Job
metadata: {name: x}
spec:
` + podTpl + `
  steps:
    - {name: s, ` + container + `run: echo}
`
}

func TestJobValidate_WorkingDirOnStepTargets(t *testing.T) {
	// job container with workingDir, default-target step -> error
	bad1 := wdJob("", `  podTemplate:
    spec:
      containers: [{name: job, image: img, workingDir: /app}]
`)
	if _, err := Parse(strings.NewReader(bad1)); err == nil || !strings.Contains(err.Error(), "workingDir") {
		t.Errorf("workingDir on the job container must be rejected, got %v", err)
	}

	// container: tools targeted by a step, tools has workingDir -> error
	bad2 := wdJob(`container: tools, `, `  podTemplate:
    spec:
      containers: [{name: job, image: img}, {name: tools, image: img2, workingDir: /app}]
`)
	if _, err := Parse(strings.NewReader(bad2)); err == nil || !strings.Contains(err.Error(), "tools") {
		t.Errorf("workingDir on a step-targeted container must be rejected, got %v", err)
	}

	// sidecar (not targeted) with workingDir -> OK
	ok := wdJob("", `  podTemplate:
    spec:
      containers: [{name: job, image: img}, {name: helper, image: img2, workingDir: /srv}]
`)
	if _, err := Parse(strings.NewReader(ok)); err != nil {
		t.Errorf("workingDir on a non-targeted sidecar must be allowed: %v", err)
	}

	// override container targeted by a step -> error
	bad3 := wdJob(`container: extra, `, `  podTemplate:
    spec:
      containers: [{name: job, image: img}]
    override:
      containers: [{name: extra, image: img3, workingDir: /app}]
`)
	if _, err := Parse(strings.NewReader(bad3)); err == nil || !strings.Contains(err.Error(), "extra") {
		t.Errorf("workingDir on a targeted override container must be rejected, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/dsl/ -run WorkingDirOnStepTargets -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement (dsl)**

Add (container.go or parse.go):

```go
// validateStepTargetedWorkingDir rejects a workingDir on any container a step
// executes in (the primary "job" container or any container named by a step's
// container: field, in steps, parallel sub-steps, or finally). Steps always
// run at the workspace mount: the k8s executor inherits the container's
// workingDir, while artifact/cache resolution and UNIFIED_WORKSPACE use the
// workspace mount path — a divergent workingDir silently desynchronizes them,
// and the artifact sidecar can only reach files under the workspace volume.
// Sidecars (containers no step targets) may set workingDir freely.
func validateStepTargetedWorkingDir(spec Spec) error {
	targeted := map[string]bool{PrimaryContainerName: true}
	collect := func(entries []StepEntry) {
		for _, e := range entries {
			if e.Container != "" {
				targeted[e.Container] = true
			}
			for _, p := range e.Parallel {
				if p.Container != "" {
					targeted[p.Container] = true
				}
			}
		}
	}
	collect(spec.Steps)
	collect(spec.Finally)

	check := func(defs []map[string]any, where string) error {
		for _, c := range defs {
			name := DefName(c)
			if !targeted[name] {
				continue
			}
			if _, has := c["workingDir"]; has {
				return fmt.Errorf("%s container %q declares workingDir, but steps execute in it: steps always run at the workspace mount (artifact/cache paths and UNIFIED_WORKSPACE resolve there); move the cd into the step script, or put workingDir on a sidecar", where, name)
			}
		}
		return nil
	}
	if err := check(PodTemplateContainers(spec.PodTemplate), "podTemplate"); err != nil {
		return err
	}
	if spec.PodTemplate != nil && spec.PodTemplate.Override != nil {
		if err := check(spec.PodTemplate.Override.Containers, "podTemplate.override"); err != nil {
			return err
		}
	}
	return nil
}
```

Wire into `Job.Validate` next to the other podTemplate checks. NOTE: JobTemplate's pod subset also carries containers — apply the same rule there (a template contributing a step-targeted container with workingDir): in `JobTemplate.Validate`, reuse via `validateStepTargetedWorkingDir(t.ToSpec())`.

- [ ] **Step 4: Implement (podbuilder build-time check for named templates)**

In `internal/k8sagent/podbuilder.go`, at the workspace-injection site (~line 398-403 where `WorkingDir` is defaulted): the primary `job` container from a NAMED agent template may carry `workingDir` (apply-time can't see agent config). Change the default-when-empty logic for the `job` container specifically: if `podSpec.Containers[i].Name == "job"` (use the dsl constant alias) AND `WorkingDir != ""` AND `WorkingDir != mountPath` → return an error (`BuildPod` can return errors) with the same invariant message. Other containers keep the current preserve behavior. Add/extend a podbuilder unit test: named-template job container with workingDir → BuildPod error; sidecar workingDir → preserved.

- [ ] **Step 5: Run tests + affected packages**

Run: `go test ./internal/dsl/ ./internal/k8sagent/ -count=1`
Expected: PASS (repo templates/examples must not regress — buildkit's containers have no workingDir).

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/ internal/k8sagent/
git commit -m "feat(dsl,k8s): reject workingDir on step-targeted containers (G5 cwd/resolution desync)"
```

---

### Task 4: `--mount` form for OCI bind mounts (G6)

**Files:**
- Modify: `internal/runtime/ocicli.go` (createArgs mounts loop)
- Test: `internal/runtime/ocicli_lifecycle_test.go` (extend) or new `ocicli_mount_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestCreateArgs_MountForm(t *testing.T) {
	cases := []struct {
		host, ctr string
		ro        bool
		want      string
	}{
		{"/host/ws", "/workspace", false, "type=bind,source=/host/ws,target=/workspace"},
		{"/host/ws", "/workspace", true, "type=bind,source=/host/ws,target=/workspace,readonly"},
		{`C:\ws`, "/workspace", false, `type=bind,source=C:\ws,target=/workspace`},
	}
	for _, c := range cases {
		spec := ContainerSpec{Image: "img", Mounts: []Mount{{HostPath: c.host, ContainerPath: c.ctr, ReadOnly: c.ro}}}
		args := (&ociCLI{bin: "docker"}).createArgs(spec) // adapt constructor/receiver to actual code
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--mount "+c.want) && !containsPair(args, "--mount", c.want) {
			t.Errorf("mount %v: argv %v missing --mount %s", c, args, c.want)
		}
		if strings.Contains(joined, " -v ") {
			t.Errorf("old -v form still present: %v", args)
		}
	}
}

func TestCreateArgs_MountRejectsSeparators(t *testing.T) {
	spec := ContainerSpec{Image: "img", Mounts: []Mount{{HostPath: "/a,b", ContainerPath: "/w"}}}
	_, err := (&ociCLI{bin: "docker"}).createArgsChecked(spec) // see implementation note
	if err == nil {
		t.Fatal("a mount path containing ',' must be rejected")
	}
}
```

**Implementation note:** inspect `createArgs`'s real signature first (grep `func.*createArgs` in ocicli.go). If it cannot return an error today, either (a) add an `error` return and update its callers inside `internal/runtime` (preferred), or (b) validate mounts earlier in the exported create path. Adapt the test to the chosen seam; `containsPair` = helper asserting consecutive argv elements. Copy struct/field names from the actual `internal/runtime` types (grep `Mounts`, `HostPath`).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/runtime/ -run 'MountForm|MountRejects' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement**

Replace the mounts loop in `createArgs` (ocicli.go:104-110):

```go
	for _, m := range spec.Mounts {
		if strings.ContainsAny(m.HostPath, ",=") || strings.ContainsAny(m.ContainerPath, ",=") {
			return nil, fmt.Errorf("mount path %q -> %q contains ',' or '=', which cannot be expressed in --mount syntax", m.HostPath, m.ContainerPath)
		}
		v := "type=bind,source=" + m.HostPath + ",target=" + m.ContainerPath
		if m.ReadOnly {
			v += ",readonly"
		}
		args = append(args, "--mount", v)
	}
```

(with the error-return signature change from the note; `--mount` is supported by docker, podman ≥ 1.x, and nerdctl — the drivers this CLI wraps). In `internal/runtime/apple.go:79` leave `-v` but add a comment: macOS host paths have no drive-letter colon; `container` CLI `--mount` support unverified.
Update existing lifecycle tests that assert the old `-v host:ctr` argv to the new form (they exercise POSIX paths — update expectations, don't delete assertions).

- [ ] **Step 4: Run package**

Run: `go test ./internal/runtime/ -count=1`
Expected: PASS. Also `go test ./internal/agent/ -count=1` (claim-pod integration constructs mounts — Docker-dependent tests may skip without a runtime; fine).

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/
git commit -m "fix(runtime): Windows-safe --mount form for OCI bind mounts (G6)"
```

---

### Task 5: Docs (G2 note, G3 entry, AppSource constraints) + full sweep

**Files:**
- Modify: `docs/troubleshooting.md` (G3 scoped-uses empty-fs entry; G2 behavior-change note appended to the existing "escapes the workspace" entry), `docs/resources.md` (AppSource repoURL/targetRevision/path constraints), `docs/jobs.md` (workingDir rule note in the podTemplate/container section).

- [ ] **Step 1: Write the docs**
- Troubleshooting: new entry "Scoped `uses` step can't find workspace files" — scope starts from a FRESH filesystem (never shares the outer workspace); files are silently absent, not an error; pass inputs via `with:` / `downloadArtifact`, extract outputs via `uploadArtifact` / `outputs:`. Add to the existing absolute-path entry: note that before the 2026-07 path hardening, absolute artifact/cache paths were silently re-rooted (k8s) or host-resolved — they now hard-fail.
- resources.md AppSource section: document the new constraints (scheme allowlist for repoURL; ref charset for targetRevision; path must not start with `-`), with the exact error strings.
- jobs.md: in the podTemplate container docs, document the workingDir rule (step-targeted containers must not set it; sidecars may; the invariant and the error string).

- [ ] **Step 2: Full sweep**

`go build ./... && go generate ./...` (no drift), `go vet ./internal/...`, full `go test ./... -count=1` (known internal/cli flake: isolate-rerun 3x if hit).

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs: appsource git constraints, workingDir rule, scoped-uses and absolute-path notes"
```

---

## Notes for the executor
- Task order 1→2 (dsl allowlists feed fetch guards conceptually but are independent — either order compiles; keep 1 first for message consistency), 3, 4, 5.
- Verify EVERY signature the plan guesses (`Fetcher` methods, `createArgs`, runtime structs) before writing tests — adjust arities to reality, keep assertions.
- `uri_test.go` may assert the old ref-error wording — the new `dsl.ValidateGitRef` message keeps the `contains invalid characters` phrase; adjust only what genuinely changed.
- Full-suite gate before finishing (merge discipline).
