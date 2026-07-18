# Security Hardening Wave Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the remaining provenance/authorization gaps from the 2026-07-18 trust-boundary audit that PR #63 did not cover.

**Architecture:** Nine independent hardening changes across the agent (step environment isolation, shim integrity), the controller (per-run authorization residuals, webhook auth), the DSL (param validation, webhook auth schema), and the cache (job namespacing). Each is a self-contained fix with its own regression test; none depends on another's implementation except where noted in **Interfaces**.

**Tech Stack:** Go 1.24+, chi router, PostgreSQL store, S3/Garage object store, testify (`require`/`assert`), `net/http/httptest`.

## Global Constraints

- Baseline is main @ `fda4bf3` (post PR #63). PR #63 already fixed: secrets-fetch run scoping (principal + `runId` + `agentRunGuard`, legacy rejected) and artifact-upload `agentRunGuard` for non-legacy auth. **Do not re-implement those.**
- Breaking changes are explicitly approved by the user. Take the strictest safe option; every breaking change ships a migration note in `docs/`.
- `toolsDir` **MUST remain under `wsBase`** — see `internal/agent/agent.go:651-667`. A remote container runtime (`DOCKER_HOST=tcp://…`, dind) shares only `wsBase`; a `toolsDir` outside it bind-mounts an empty dir at `/.ucd` with no error and every container entrypoint fails `exit status 127`. Never "fix" C-2 by relocating it.
- `SecretsNeeded` is **not persisted** — it is computed in the claim handler (`internal/controller/api_agent.go:269`) and returned in the claim response (`internal/api/types.go:101`). A-3 must recompute, not read a column.
- Job-author `image:` values stay untouched (a job author can already run arbitrary code in their own job). Only fleet-wide *default* images get digest-pinned.
- Full `go test ./... -count=1` green before finishing. Known transient flake in `internal/cli` — isolate-rerun that package up to 3× to confirm.
- After the code tasks, the compose stack (`docker-compose.yaml`) must run a real isolated job, a cache save/restore, and an artifact round-trip. C-2 and C-3 can pass unit tests while breaking the real dind path.

---

### Task 1: A-1 — Agent step environment allowlist

**Files:**
- Create: `internal/agent/stepenv.go`
- Modify: `internal/agent/runner.go:98-165` (all three exec builders), `internal/agent/backend_host.go`
- Test: `internal/agent/stepenv_test.go`, `internal/agent/runner_test.go`

**Interfaces:**
- Consumes: `AgentConfig.ExposeEnv []string` (`internal/agent/agent.go:65`).
- Produces: `agent.StepEnv(exposeEnv []string, extraEnv []string) []string` — the full `cmd.Env` slice for a step. Used by all three runner entry points.

Current leak (`internal/agent/runner.go:103, 133, 159`): `cmd.Env = append(os.Environ(), extraEnv...)`, and when `extraEnv` is empty `cmd.Env` is left nil so `os/exec` inherits the parent env anyway. Either way the agent's credentials (`UNIFIED_AGENT_TOKEN`, `UNIFIED_CACHE_KEY`, `UNIFIED_CACHE_SECRET`) reach the step.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/stepenv_test.go`:

```go
package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		require.Len(t, parts, 2, "malformed env entry %q", kv)
		m[parts[0]] = parts[1]
	}
	return m
}

func TestStepEnv_ExcludesAgentCredentials(t *testing.T) {
	t.Setenv("UNIFIED_AGENT_TOKEN", "super-secret")
	t.Setenv("UNIFIED_CACHE_KEY", "ck")
	t.Setenv("UNIFIED_CACHE_SECRET", "cs")
	t.Setenv("UNIFIED_TOKEN", "ut")
	t.Setenv("UNIFIED_CONTROLLER_KEY", "uck")

	got := envMap(t, StepEnv(nil, nil))
	for _, banned := range []string{
		"UNIFIED_AGENT_TOKEN", "UNIFIED_CACHE_KEY", "UNIFIED_CACHE_SECRET",
		"UNIFIED_TOKEN", "UNIFIED_CONTROLLER_KEY",
	} {
		assert.NotContains(t, got, banned, "%s must never reach a step", banned)
	}
}

func TestStepEnv_KeepsShellBaseline(t *testing.T) {
	got := envMap(t, StepEnv(nil, nil))
	assert.Contains(t, got, "PATH", "a step needs PATH to resolve binaries")
}

func TestStepEnv_ExposeEnvAllowlisted(t *testing.T) {
	t.Setenv("MY_BUILD_FLAG", "on")
	t.Setenv("NOT_LISTED", "nope")

	got := envMap(t, StepEnv([]string{"MY_BUILD_FLAG"}, nil))
	assert.Equal(t, "on", got["MY_BUILD_FLAG"])
	assert.NotContains(t, got, "NOT_LISTED")
}

func TestStepEnv_DenylistBeatsExposeEnv(t *testing.T) {
	t.Setenv("UNIFIED_AGENT_TOKEN", "super-secret")
	// An operator must not be able to foot-gun a credential into steps.
	got := envMap(t, StepEnv([]string{"UNIFIED_AGENT_TOKEN"}, nil))
	assert.NotContains(t, got, "UNIFIED_AGENT_TOKEN")
}

func TestStepEnv_ExtraEnvWins(t *testing.T) {
	t.Setenv("MY_BUILD_FLAG", "from-host")
	got := envMap(t, StepEnv([]string{"MY_BUILD_FLAG"}, []string{"MY_BUILD_FLAG=from-step"}))
	assert.Equal(t, "from-step", got["MY_BUILD_FLAG"])
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestStepEnv -v`
Expected: FAIL — `undefined: StepEnv`.

- [ ] **Step 3: Implement `StepEnv`**

Create `internal/agent/stepenv.go`:

```go
package agent

import (
	"os"
	"runtime"
	"strings"
)

// stepEnvDenied lists environment variables that must NEVER reach a job step,
// even if an operator names them in ExposeEnv. These are the agent's own
// credentials: leaking them lets any job author act as the agent (and, via the
// cache credentials, write directly to the shared object store, bypassing every
// controller-side check).
var stepEnvDenied = map[string]bool{
	"UNIFIED_AGENT_TOKEN":    true,
	"UNIFIED_CACHE_KEY":      true,
	"UNIFIED_CACHE_SECRET":   true,
	"UNIFIED_TOKEN":          true,
	"UNIFIED_CONTROLLER_KEY": true,
}

// stepEnvBaseline returns the environment variable names a shell needs to
// function at all. Everything else must be opted in via ExposeEnv.
func stepEnvBaseline() []string {
	if runtime.GOOS == "windows" {
		return []string{"PATH", "PATHEXT", "SystemRoot", "SystemDrive", "COMSPEC", "TEMP", "TMP", "USERPROFILE"}
	}
	return []string{"PATH", "HOME", "PWD", "SHELL", "TMPDIR", "LANG", "LC_ALL", "TZ", "USER"}
}

// StepEnv builds the environment for a job step. It deliberately does NOT
// inherit the agent's process environment (see stepEnvDenied): the agent's env
// holds fleet credentials, and a step is authored by a job author we do not
// trust with them. The k8s agent already builds a fresh env this way
// (imageStepEnv); this is the host-side equivalent.
//
// Precedence, lowest to highest: OS baseline -> ExposeEnv allowlist -> extraEnv
// (the orchestrator's already-expanded step env). Denied names are dropped at
// every layer except extraEnv, which the controller — not the job author —
// controls.
func StepEnv(exposeEnv []string, extraEnv []string) []string {
	out := make([]string, 0, len(extraEnv)+16)
	seen := map[string]bool{}

	add := func(name string) {
		if name == "" || seen[name] || stepEnvDenied[name] {
			return
		}
		v, ok := os.LookupEnv(name)
		if !ok {
			return
		}
		seen[name] = true
		out = append(out, name+"="+v)
	}

	for _, name := range stepEnvBaseline() {
		add(name)
	}
	for _, name := range exposeEnv {
		add(strings.TrimSpace(name))
	}
	// extraEnv wins: append last so a duplicate key overrides earlier entries
	// (os/exec uses the last occurrence).
	out = append(out, extraEnv...)
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run TestStepEnv -v`
Expected: PASS (all five).

- [ ] **Step 5: Wire it into all three exec builders**

In `internal/agent/runner.go`, the three functions `RunStep` (:98), `RunStepWithShell` (:127), and `RunStepCapture` (:153) each contain:

```go
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
```

Replace each with an **unconditional** assignment so no path can silently inherit:

```go
	// Always set Env: a nil cmd.Env makes os/exec inherit the agent's whole
	// environment, which is exactly the leak StepEnv exists to prevent.
	cmd.Env = StepEnv(exposeEnv, extraEnv)
```

Thread `exposeEnv []string` in as a new parameter on all three functions (place it immediately before `workDir`), and update every caller — find them with:

```bash
grep -rn "RunStep(\|RunStepWithShell(\|RunStepCapture(" --include=*.go internal/ cmd/ | grep -v "func "
```

Callers inside the agent pass `a.ExposeEnv` (or `b.a.ExposeEnv` from the host backend, `internal/agent/backend_host.go`). If `os` becomes unused in `runner.go`, drop the import.

- [ ] **Step 6: Verify the whole agent package**

Run: `go test ./internal/agent/ -count=1`
Expected: PASS. If an existing test asserted that a step sees an inherited variable, that test encoded the vulnerability — update it to use `ExposeEnv` and note the change in the commit message.

- [ ] **Step 7: Commit**

```bash
git add internal/agent/stepenv.go internal/agent/stepenv_test.go internal/agent/runner.go internal/agent/runner_test.go internal/agent/backend_host.go
git commit -m "fix(agent): build step env from an allowlist instead of inheriting the agent's environment"
```

---

### Task 2: A-4 — Artifact upload uses the hardened key builder

**Files:**
- Modify: `internal/controller/api_artifacts.go` (the upload handler, around the `fmt.Sprintf` key build)
- Test: `internal/controller/api_artifacts_test.go`

**Interfaces:**
- Consumes: `artifact.ArtifactKey(runID, name string) (string, error)` — see Step 3; `internal/artifact/store.go` currently has this logic as the unexported `artifactKey`.
- Produces: nothing consumed by later tasks.

The upload handler is the only artifact path that builds its object key with raw `fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name)` instead of `artifactKey()`, so the `isSafeArtifactPathSegment` guard from the #26 traversal fix does not apply. chi's `{name}` does not match `/`, so this is not currently exploitable — but the defense is missing on the one path whose input comes straight off the network.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/api_artifacts_test.go` (follow the existing tests in this file for server/run setup):

```go
func TestArtifactUpload_RejectsUnsafeName(t *testing.T) {
	// A name that is not a plain single path segment must be rejected before
	// it reaches the object store, by the same guard every other artifact
	// path uses (isSafeArtifactPathSegment via artifactKey).
	for _, name := range []string{"..", ".", ""} {
		_, err := artifact.ArtifactKey("run1", name)
		require.Error(t, err, "name %q must be rejected", name)
	}
	key, err := artifact.ArtifactKey("run1", "build-output")
	require.NoError(t, err)
	assert.Equal(t, "artifacts/run1/build-output.tar.gz", key,
		"valid names must produce the exact same key as before, so existing artifacts still resolve")
}
```

Import `"github.com/eirueimi/unified-cd/internal/artifact"` in the test file if not present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestArtifactUpload_RejectsUnsafeName -v`
Expected: FAIL — `undefined: artifact.ArtifactKey` (it is currently unexported).

- [ ] **Step 3: Export the key builder**

In `internal/artifact/store.go`, rename the unexported `artifactKey` to the exported `ArtifactKey` (same signature, same body, same doc comment — keep the comment explaining it is the last line of defense against traversal, see #26). Update its in-package callers (`Upload`, `Download`).

- [ ] **Step 4: Use it in the upload handler**

In `internal/controller/api_artifacts.go`, replace the raw key build:

```go
	key := fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name)
```

with:

```go
	key, err := artifact.ArtifactKey(runID, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
```

Add the `internal/artifact` import; drop `fmt` if it becomes unused.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/controller/ ./internal/artifact/ -count=1`
Expected: PASS — including the existing artifact round-trip tests, which prove valid names still produce identical keys.

- [ ] **Step 6: Commit**

```bash
git add internal/artifact/store.go internal/controller/api_artifacts.go internal/controller/api_artifacts_test.go
git commit -m "fix(controller): build artifact upload keys with the hardened ArtifactKey guard"
```

---

### Task 3: A-3 — Constrain secrets fetch to the run's `SecretsNeeded`

**Files:**
- Modify: `internal/controller/api_secrets.go` (`handleAgentSecretsFetch`), `internal/controller/api_agent.go` (extract the name-collection helper)
- Test: `internal/controller/api_secrets_test.go`

**Interfaces:**
- Consumes: `collectSecretNames(tpl string, seen map[string]struct{})` (`internal/controller/api_agent.go:440`); `agentRunGuard` / `respondRunWriteVerdict` (already applied by PR #63 — do not re-add).
- Produces: `(*Server).secretNamesForRun(ctx context.Context, runID string) (map[string]struct{}, error)`.

PR #63 made this endpoint require an authenticated non-legacy principal, a `runId`, and a passing `agentRunGuard`. What remains: the handler still loops over caller-supplied `req.Names` and decrypts whatever is asked, so an agent holding a valid credential for *any* claimed run can read *any* secret. `SecretsNeeded` is not persisted, so it must be recomputed from the run's stored spec.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/api_secrets_test.go` (mirror the setup of the existing secrets-fetch tests, including a claimed run so `agentRunGuard` passes):

```go
func TestAgentSecretsFetch_RejectsNameNotNeededByRun(t *testing.T) {
	// Setup: a run whose spec references only {{ .Secrets.NEEDED }}, plus an
	// unrelated secret the run does not reference.
	// (Reuse this file's existing helper for building a server + claimed run.)
	srv, agentID, runID := newSecretsFetchFixture(t, `steps:
  - name: s
    run: echo {{ .Secrets.NEEDED }}
`)
	mustSetSecret(t, srv, "NEEDED", "ok")
	mustSetSecret(t, srv, "OTHER", "must-not-leak")

	// Asking for a secret the run does not declare must be refused outright.
	rr := postFetchSecrets(t, srv, agentID, runID, []string{"OTHER"})
	assert.Equal(t, http.StatusForbidden, rr.Code)
	assert.NotContains(t, rr.Body.String(), "must-not-leak")

	// The run's own secret still resolves.
	rr = postFetchSecrets(t, srv, agentID, runID, []string{"NEEDED"})
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "ok")
}
```

If `newSecretsFetchFixture`, `mustSetSecret`, or `postFetchSecrets` do not already exist in this file, write them from the patterns the surrounding tests use — do not invent a new mocking framework.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestAgentSecretsFetch_RejectsNameNotNeededByRun -v`
Expected: FAIL — the request for `OTHER` returns 200 with the secret value.

- [ ] **Step 3: Extract the per-run name set**

The claim handler builds the set as a side effect of `buildStages` (`internal/controller/api_agent.go:262-266`):

```go
	secretsNeeded := map[string]struct{}{}
	stepIdx := 0 // flat step counter across steps and finally

	resp.Stages = buildStages(spec.Steps, &stepIdx, secretsNeeded, spec.Shell)
	resp.Finally = buildStages(spec.Finally, &stepIdx, secretsNeeded, spec.Shell)
```

Add a method that reproduces exactly that traversal from the stored spec, so the fetch path and the claim path can never disagree about what a run "needs":

```go
// secretNamesForRun recomputes the set of secret names a run's spec references.
// SecretsNeeded is NOT persisted — the claim handler computes it and returns it
// in the claim response — so the fetch path recomputes it from the stored spec
// rather than trusting the names the caller asked for.
//
// It deliberately reuses buildStages, which is what populates secretsNeeded on
// the claim path: any future step shape that can carry a secret reference is
// then covered here automatically.
func (s *Server) secretNamesForRun(ctx context.Context, runID string) (map[string]struct{}, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("load run %s: %w", runID, err)
	}
	var spec dsl.JobSpec
	if err := json.Unmarshal(run.Spec, &spec); err != nil {
		return nil, fmt.Errorf("parse run spec %s: %w", runID, err)
	}
	needed := map[string]struct{}{}
	stepIdx := 0
	_ = buildStages(spec.Steps, &stepIdx, needed, spec.Shell)
	_ = buildStages(spec.Finally, &stepIdx, needed, spec.Shell)
	return needed, nil
}
```

Match the exact way the claim handler loads and unmarshals the run's spec — read the lines above `:262` and mirror them (field name, snapshot vs job spec). If the claim path already has a helper for that load, reuse it instead of duplicating.

Leave the claim handler as-is: it already has `spec` in hand, so routing it through this method would mean loading the run twice. The shared piece is `buildStages`, which both now rely on.

- [ ] **Step 4: Enforce it in the fetch handler**

In `internal/controller/api_secrets.go`, after the existing `agentRunGuard` check and before the decrypt loop:

```go
	allowed, err := s.secretNamesForRun(r.Context(), req.RunID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, name := range req.Names {
		if _, ok := allowed[name]; !ok {
			// Do not echo the requested name's value or existence.
			http.Error(w, "secret not needed by this run", http.StatusForbidden)
			return
		}
	}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/controller/ -count=1`
Expected: PASS, including the existing PR #63 tests (legacy rejected, `runId` required, guard enforced).

- [ ] **Step 6: Commit**

```bash
git add internal/controller/api_secrets.go internal/controller/api_agent.go internal/controller/api_secrets_test.go
git commit -m "fix(controller): restrict agent secret fetch to the run's declared secrets"
```

---

### Task 4: A-2 — Apply per-run authorization to legacy principals

**Files:**
- Modify: `internal/controller/agent_guard.go:104, 119`; `internal/controller/agent_auth.go:123`; `internal/controller/api_agent.go:36, 154`; `internal/controller/api_artifacts.go` (the `AuthMethod != "legacy"` condition)
- Test: `internal/controller/agent_guard_test.go`, `internal/controller/api_artifacts_test.go`

**Interfaces:**
- Consumes: `agentRunGuard(ctx, agentID, runID string, rejectTerminal bool) (runWriteVerdict, error)`, `respondRunWriteVerdict(w, v, runID) bool`.
- Produces: nothing consumed by later tasks.

Five sites gate their check on `principal.AuthMethod != "legacy"`, so while compatibility mode is configured one shared token bypasses run-ownership everywhere except secrets. `internal/controller/api_secrets.go` **rejects** legacy (fail-closed) and is correct — leave it exactly as is.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/agent_guard_test.go`:

```go
func TestAgentRunGuard_AppliesToLegacyPrincipal(t *testing.T) {
	// A legacy (shared-token) caller must not be able to write to a run it did
	// not claim. Compatibility mode keeps such agents CONNECTED; it must not
	// exempt them from run ownership.
	srv, _ := newGuardFixture(t)
	claimedRun := mustCreateClaimedRun(t, srv, "agent-a")
	otherRun := mustCreateClaimedRun(t, srv, "agent-b")

	rr := postStepReportAsLegacy(t, srv, "agent-a", otherRun)
	assert.Equal(t, http.StatusForbidden, rr.Code, "legacy caller must not write to another agent's run")

	rr = postStepReportAsLegacy(t, srv, "agent-a", claimedRun)
	assert.Less(t, rr.Code, 400, "legacy caller must still write to the run it claimed")
}
```

Build `newGuardFixture`, `mustCreateClaimedRun`, and `postStepReportAsLegacy` from the patterns already used in this file's PR #63 tests.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestAgentRunGuard_AppliesToLegacyPrincipal -v`
Expected: FAIL — the write to `otherRun` succeeds because the guard is skipped for legacy.

- [ ] **Step 3: Remove the legacy exemptions**

At each of these sites, drop the `AuthMethod != "legacy"` condition so the guard runs for every principal:

- `internal/controller/agent_guard.go:104` and `:119`
- `internal/controller/api_agent.go:36` and `:154`
- `internal/controller/api_artifacts.go` (the upload guard added by PR #63)

At `internal/controller/agent_auth.go:123`, the path-binding check is:

```go
		if principal.AuthMethod != "legacy" && principal.AgentID != chi.URLParam(r, "agentId") {
```

A legacy principal has no verified `AgentID`, so it cannot be compared to the path. Bind it instead: for a legacy principal, adopt the `{agentId}` from the path as its identity, so the guard downstream has an agent to check ownership against. Add a comment stating that this is identity *assignment*, not verification — legacy callers are only as trustworthy as the shared token, and the guard limits blast radius to runs that agent actually claimed.

Leave `internal/controller/api_secrets.go` unchanged (legacy stays rejected).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/controller/ -count=1`
Expected: PASS. Existing legacy tests that asserted a bypass encoded the vulnerability — update them and say so in the commit message.

- [ ] **Step 5: Document the behavior change**

In `docs/` (the agent authentication/migration page that PR #63 added — find it with `grep -rln "legacySharedToken" docs/`), add: legacy shared-token agents remain able to connect, but are now subject to the same per-run ownership checks as enrolled agents. An agent that writes to runs it did not claim will start receiving 403 and must be migrated to per-agent enrollment.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/agent_guard.go internal/controller/agent_auth.go internal/controller/api_agent.go internal/controller/api_artifacts.go internal/controller/agent_guard_test.go docs/
git commit -m "fix(controller): enforce per-run ownership for legacy shared-token agents"
```

---

### Task 5: B-1 — Webhook auth must be explicit

**Files:**
- Modify: `internal/dsl/webhook_parse.go:40-49`, `internal/dsl/types.go` (or wherever `WebhookAuth` is defined), `internal/controller/api_webhooks.go:112`
- Test: `internal/dsl/webhook_parse_test.go`, `internal/controller/api_webhooks_test.go`

**Interfaces:**
- Produces: `WebhookAuth.AllowUnauthenticated bool` (`yaml:"allowUnauthenticated,omitempty"`).

Today an omitted `auth:` block silently becomes `type: none`, and the ingress route sits outside all auth middleware — so forgetting `auth:` publishes an unauthenticated remote trigger.

- [ ] **Step 1: Write the failing test**

Add to `internal/dsl/webhook_parse_test.go`:

```go
func TestParseWebhookReceiver_OmittedAuthIsRejected(t *testing.T) {
	_, err := ParseWebhookReceiver(strings.NewReader(`apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata: {name: wh}
spec:
  trigger: {job: build}
`))
	require.Error(t, err, "an omitted auth block must not silently mean unauthenticated")
	assert.Contains(t, err.Error(), "auth")
}

func TestParseWebhookReceiver_NoneRequiresExplicitOptIn(t *testing.T) {
	_, err := ParseWebhookReceiver(strings.NewReader(`apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata: {name: wh}
spec:
  trigger: {job: build}
  auth: {type: none}
`))
	require.Error(t, err, "type: none must require allowUnauthenticated: true")

	wr, err := ParseWebhookReceiver(strings.NewReader(`apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata: {name: wh}
spec:
  trigger: {job: build}
  auth: {type: none, allowUnauthenticated: true}
`))
	require.NoError(t, err)
	assert.Equal(t, "none", wr.Spec.Auth.Type)
	assert.True(t, wr.Spec.Auth.AllowUnauthenticated)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dsl/ -run TestParseWebhookReceiver -v`
Expected: FAIL — both currently parse without error.

- [ ] **Step 3: Implement**

Add `AllowUnauthenticated bool \`yaml:"allowUnauthenticated,omitempty"\`` to the webhook auth struct. In `internal/dsl/webhook_parse.go`, replace the defaulting switch:

```go
	switch wr.Spec.Auth.Type {
	case "hmac-sha256", "github", "token":
	case "none":
		if !wr.Spec.Auth.AllowUnauthenticated {
			return nil, fmt.Errorf("webhook receiver %q: auth.type \"none\" requires allowUnauthenticated: true — an unauthenticated webhook lets anyone trigger this job", wr.Metadata.Name)
		}
	case "":
		return nil, fmt.Errorf("webhook receiver %q: spec.auth is required (use type: hmac-sha256, github, or token; or type: none with allowUnauthenticated: true)", wr.Metadata.Name)
	default:
		// keep the existing unknown-type error
	}
```

In `internal/controller/api_webhooks.go:112`, the condition `if spec.Auth.Type != "none" && spec.Auth.Type != ""` currently treats an empty type as unauthenticated. Since parse now rejects empty, tighten it to `if spec.Auth.Type != "none"` so a stored legacy row with an empty type **fails closed** rather than skipping verification.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/dsl/ ./internal/controller/ -count=1`
Expected: PASS. Existing fixtures that omit `auth:` must be updated — that is the intended breaking change.

- [ ] **Step 5: Update examples and document the migration**

Update any `examples/` webhook receiver that omits `auth:` (check `examples/jobs/webhook-receiver.yaml`), and add a migration note in `docs/`: `spec.auth` is now required on `WebhookReceiver`; receivers that relied on the implicit `none` must declare `auth: {type: none, allowUnauthenticated: true}` or, preferably, adopt a real auth type. Per `AGENTS.md`, examples and templates must be updated alongside docs whenever behavior changes.

- [ ] **Step 6: Commit**

```bash
git add internal/dsl/ internal/controller/api_webhooks.go internal/controller/api_webhooks_test.go examples/ docs/
git commit -m "fix(dsl): require explicit webhook auth; unauthenticated needs allowUnauthenticated"
```

---

### Task 6: B-2 — `pattern:` validation on every param path

**Files:**
- Modify: `internal/dsl/types.go:55-61` (`Input`), `internal/controller/params.go:22` (`resolveParams`), `internal/dsl/webhook_parse.go` (payload-mapped params must be validated)
- Test: `internal/controller/params_test.go`, `internal/dsl/webhook_parse_test.go`

**Interfaces:**
- Consumes: `resolveParams(inputs []dsl.Input, supplied map[string]string) (map[string]string, error)` — the single choke point for every param source (webhook mapping, CLI `--param`, `call:` `with:`, schedule params).
- Produces: `Input.Pattern string`, `Input.Unvalidated bool`.

Param values are interpolated into step shell text (`dsl.ExpandTemplate(step.Run, …)` → `sh -lc`), so an externally-sourced param is command injection. Validating in `resolveParams` covers all sources at once, since the injection sink is shared.

- [ ] **Step 1: Write the failing test**

Add to `internal/controller/params_test.go`:

```go
func TestResolveParams_EnforcesPattern(t *testing.T) {
	inputs := []dsl.Input{{Name: "ref", Type: "string", Pattern: `^[A-Za-z0-9._/-]+$`}}

	_, err := resolveParams(inputs, map[string]string{"ref": "main; rm -rf /"})
	require.Error(t, err, "a value with shell metacharacters must be rejected")
	assert.Contains(t, err.Error(), "ref")

	got, err := resolveParams(inputs, map[string]string{"ref": "refs/heads/main"})
	require.NoError(t, err)
	assert.Equal(t, "refs/heads/main", got["ref"])
}

func TestResolveParams_InvalidPatternIsAnError(t *testing.T) {
	inputs := []dsl.Input{{Name: "ref", Type: "string", Pattern: "([unclosed"}}
	_, err := resolveParams(inputs, map[string]string{"ref": "x"})
	require.Error(t, err, "a malformed pattern must fail loudly, not silently allow everything")
}

func TestResolveParams_NoPatternStillWorks(t *testing.T) {
	inputs := []dsl.Input{{Name: "msg", Type: "string"}}
	got, err := resolveParams(inputs, map[string]string{"msg": "anything goes"})
	require.NoError(t, err)
	assert.Equal(t, "anything goes", got["msg"])
}
```

Add to `internal/dsl/webhook_parse_test.go`:

```go
func TestParseWebhookReceiver_MappedParamNeedsValidation(t *testing.T) {
	// A param fed from an attacker-controlled payload must declare either a
	// pattern or an explicit unvalidated opt-out on the target job's input.
	// Validation happens against the referenced job at apply time; this test
	// pins the receiver-side requirement that the mapping is not silently
	// unchecked.
	_, err := ParseWebhookReceiver(strings.NewReader(`apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata: {name: wh}
spec:
  trigger: {job: build}
  auth: {type: github, secretRef: s}
  paramsMapping:
    ref: "{{ .Payload.ref }}"
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ref")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -run TestResolveParams -v && go test ./internal/dsl/ -run TestParseWebhookReceiver_MappedParamNeedsValidation -v`
Expected: FAIL — `Input` has no `Pattern` field; no validation exists.

- [ ] **Step 3: Add the fields**

In `internal/dsl/types.go`, extend `Input`:

```go
type Input struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type" schema:"enum:string,bool,int,array"`
	Required    bool   `yaml:"required,omitempty"`
	Default     any    `yaml:"default,omitempty"`
	Description string `yaml:"description,omitempty"`
	// Pattern is a regular expression every supplied value must match.
	// Param values are interpolated into step shell text, so a param fed from
	// an untrusted source (a webhook payload especially) is a command-injection
	// vector unless constrained. Suggested starting point: ^[A-Za-z0-9._/-]+$
	Pattern string `yaml:"pattern,omitempty"`
	// Unvalidated explicitly opts this input out of the pattern requirement for
	// payload-mapped params. Use only when the value is genuinely free-form and
	// never reaches a shell.
	Unvalidated bool `yaml:"unvalidated,omitempty"`
}
```

- [ ] **Step 4: Enforce in `resolveParams`**

In `internal/controller/params.go`, after a value is resolved for an input and before returning, compile and apply the pattern:

```go
		if in.Pattern != "" {
			re, err := regexp.Compile(in.Pattern)
			if err != nil {
				return nil, fmt.Errorf("param %q: invalid pattern %q: %w", in.Name, in.Pattern, err)
			}
			if !re.MatchString(value) {
				// Do not echo the rejected value: it may carry an injection payload
				// into logs read by an operator.
				return nil, fmt.Errorf("param %q does not match required pattern %q", in.Name, in.Pattern)
			}
		}
```

Apply it to defaults as well as supplied values, so a bad default cannot slip through. Add the `regexp` import.

- [ ] **Step 5: Require validation for payload-mapped params**

In `internal/dsl/webhook_parse.go`, after parsing `paramsMapping`, reject any mapping whose value references `.Payload` unless the receiver's target job declares that input with a `Pattern` or `Unvalidated: true`. The receiver does not hold the job's spec at parse time, so enforce it where the job is known — at run creation / receiver-apply, alongside the existing container-reference validation. Return an error naming the param, e.g.:

```
webhook receiver "wh": param "ref" is mapped from the request payload but job "build" declares no pattern for it (add pattern: to the input, or unvalidated: true to accept it explicitly)
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/dsl/ ./internal/controller/ -count=1`
Expected: PASS.

- [ ] **Step 7: Regenerate schema + document**

Run `go generate ./...` (the `Input` struct feeds `schemas/unified-cd.schema.json` and `docs/field-reference.md` via `cmd/schemagen`/`cmd/docgen`) and commit the regenerated artifacts. Document `pattern:` and `unvalidated:` in `docs/`, with the injection rationale and the suggested `^[A-Za-z0-9._/-]+$` starting point. Update `examples/jobs/webhook-receiver.yaml` to declare a pattern for its payload-mapped `tag`.

- [ ] **Step 8: Commit**

```bash
git add internal/dsl/ internal/controller/params.go internal/controller/params_test.go schemas/ docs/ examples/
git commit -m "feat(dsl): add param pattern validation and require it for payload-mapped params"
```

---

### Task 7: C-1 — Namespace the cache by job and cap `ttlDays`

**Files:**
- Modify: `internal/cache/cache.go` (`objectKey`, `Save`, `Restore`, `findBestMatch`), `internal/agent/backend_host.go:201,212,238,245`, `cmd/unified-sidecar/run.go:80,101`, `internal/dsl/parse.go:563` (ttl bound)
- Test: `internal/cache/cache_test.go`

**Interfaces:**
- Produces: `cache.Save(ctx, store, jobName, path, key string, ttlDays int) error` and `cache.Restore(ctx, store, jobName, path, key string, restoreKeys []string) (bool, error)` — `jobName` is the **qualified** job name (`dsl.QualifiedName`, e.g. `team-a/build`), inserted as a new third parameter.

Today `objectKey` is `sha256(key)` with no owner component, and `findBestMatch` scans every `.meta` in the shared store selecting by prefix on the attacker-writable `OriginalKey`, tie-broken by longest remaining TTL — which the attacker controls. Job A can therefore plant an entry that job B restores and executes.

- [ ] **Step 1: Write the failing test**

Add to `internal/cache/cache_test.go`:

```go
func TestRestore_CannotHijackAnotherJobsEntry(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalObjectStore(t.TempDir())

	// Job A plants an entry with a long TTL under a key job B's restoreKeys prefix-match.
	srcA := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(srcA, "pwned.txt"), []byte("evil"), 0o600))
	require.NoError(t, cache.Save(ctx, store, "attacker/job", srcA, "deps-pwned", 3650))

	// Job B restores with a prefix that would have matched job A's key.
	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "victim/job", dest, "deps-victim", []string{"deps-"})
	require.NoError(t, err)
	assert.False(t, hit, "a job must never restore another job's cache entry")
	_, statErr := os.Stat(filepath.Join(dest, "pwned.txt"))
	assert.Error(t, statErr, "attacker payload must not land in the victim workspace")
}

func TestSaveRestore_SameJobRoundTrips(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("data"), 0o600))
	require.NoError(t, cache.Save(ctx, store, "team-a/build", src, "deps-v1", 7))

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "team-a/build", dest, "deps-v1", nil)
	require.NoError(t, err)
	assert.True(t, hit)
	got, err := os.ReadFile(filepath.Join(dest, "f.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

func TestRestoreKeys_FallbackWorksWithinSameJob(t *testing.T) {
	ctx := context.Background()
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), []byte("data"), 0o600))
	require.NoError(t, cache.Save(ctx, store, "team-a/build", src, "deps-abc123", 7))

	dest := t.TempDir()
	hit, err := cache.Restore(ctx, store, "team-a/build", dest, "deps-nomatch", []string{"deps-"})
	require.NoError(t, err)
	assert.True(t, hit, "prefix fallback must still work within the same job")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cache/ -count=1 -run 'TestRestore_CannotHijack|TestSaveRestore_SameJob|TestRestoreKeys_Fallback'`
Expected: FAIL — `Save`/`Restore` do not take a job name.

- [ ] **Step 3: Namespace the key**

In `internal/cache/cache.go`:

```go
// objectKey converts a job name + cache key to the object name prefix (without
// extension). The job component namespaces every entry: without it the cache is
// one flat global namespace, and a job could plant an entry that another job's
// restoreKeys prefix-match would select and execute. Job identity (not run ID)
// is the right granularity — reuse across runs of the same job is the point of
// a cache.
func objectKey(jobName, key string) string {
	j := sha256.Sum256([]byte(jobName))
	h := sha256.Sum256([]byte(key))
	return "caches/" + base64.RawURLEncoding.EncodeToString(j[:]) + "/" + base64.RawURLEncoding.EncodeToString(h[:])
}

// jobPrefix returns the List prefix containing only this job's entries.
func jobPrefix(jobName string) string {
	j := sha256.Sum256([]byte(jobName))
	return "caches/" + base64.RawURLEncoding.EncodeToString(j[:]) + "/"
}
```

Add `jobName string` as the third parameter of `Save` and `Restore`, thread it into `objectKey`, and change `findBestMatch` to take `jobName` and `List` with `jobPrefix(jobName)` instead of `"caches/"` — so another job's entries are not merely filtered out, they are never even listed. Record the owner in `Meta` (`OwnerJob string \`json:"ownerJob"\``) and, on restore, skip any entry whose `OwnerJob` does not match (defense in depth against a mis-keyed object).

- [ ] **Step 4: Update the call sites**

- `internal/agent/backend_host.go:201, 212, 238, 245` — pass the claim's qualified job name (the same value `claimWorkDir` uses; it is on the claim response as `JobName`).
- `cmd/unified-sidecar/run.go:80, 101` — add a `--job` flag (`flag.String("job", "", "qualified job name owning this cache entry")`), require it to be non-empty for cache operations, and pass it through. Update the sidecar's invocation site so the agent supplies it.

- [ ] **Step 5: Cap `ttlDays`**

In `internal/dsl/parse.go` (around :563, where `ttlDays` is currently only checked for non-negativity), add an upper bound so one entry cannot pin itself indefinitely:

```go
const maxCacheTTLDays = 365

	if c.TTLDays > maxCacheTTLDays {
		return fmt.Errorf("cache ttlDays %d exceeds the maximum of %d", c.TTLDays, maxCacheTTLDays)
	}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/cache/ ./internal/agent/ ./internal/dsl/ -count=1`
Expected: PASS.

- [ ] **Step 7: Document**

In `docs/`, note that cache entries are namespaced per job: `restoreKeys` now only match entries saved by the same job, existing entries are invalidated by the layout change and will simply regenerate on the next run, and `ttlDays` is capped at 365.

- [ ] **Step 8: Commit**

```bash
git add internal/cache/ internal/agent/backend_host.go cmd/unified-sidecar/run.go internal/dsl/parse.go docs/
git commit -m "fix(cache): namespace entries by job and cap ttlDays to stop cross-job poisoning"
```

---

### Task 8: C-2 — Verify and repair the `ucd-sh` shim

**Files:**
- Modify: `internal/agent/agent.go` (near `InstallShim`, :680-701, and the claim loop)
- Test: `internal/agent/shim_integrity_test.go`

**Interfaces:**
- Consumes: `InstallShim(workspaceDir string) (toolsDir string, err error)` (`internal/agent/agent.go:680`).
- Produces: `EnsureShimIntact(toolsDir string) (repaired bool, err error)`.

**Do NOT relocate `toolsDir`.** `internal/agent/agent.go:651-667` documents that it must live under `wsBase`: with a remote container runtime (dind), only `wsBase` is a shared mount, and a `toolsDir` outside it bind-mounts an empty directory at `/.ucd` with no error, breaking every container entrypoint with `exit status 127`. The fix is integrity verification, not relocation.

A native step runs as the agent user with cwd inside `wsBase`, so it can `cp evil ../../.ucd-tools/ucd-sh` and permanently backdoor the default shell of every containerized job later run on that agent.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/shim_integrity_test.go`:

```go
package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureShimIntact_RepairsTamperedShim(t *testing.T) {
	ws := t.TempDir()
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	pristine, err := os.ReadFile(shimPath)
	require.NoError(t, err)

	// Simulate a native step overwriting the shim.
	require.NoError(t, os.WriteFile(shimPath, []byte("#!/bin/sh\nexfiltrate\n"), 0o755))

	repaired, err := EnsureShimIntact(toolsDir)
	require.NoError(t, err)
	assert.True(t, repaired, "tampering must be detected and repaired")

	got, err := os.ReadFile(shimPath)
	require.NoError(t, err)
	assert.Equal(t, pristine, got, "shim must be restored to the embedded bytes")
}

func TestEnsureShimIntact_NoopWhenIntact(t *testing.T) {
	ws := t.TempDir()
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)

	repaired, err := EnsureShimIntact(toolsDir)
	require.NoError(t, err)
	assert.False(t, repaired, "an intact shim must not be rewritten")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/agent/ -run TestEnsureShimIntact -v`
Expected: FAIL — `undefined: EnsureShimIntact`.

- [ ] **Step 3: Implement**

In `internal/agent/agent.go`, beside `InstallShim`:

```go
// EnsureShimIntact re-verifies the on-disk ucd-sh against the embedded bytes
// and rewrites it if they differ, reporting whether a repair was needed.
//
// The shim is the default shell and keep-alive entrypoint for every container
// step, and it necessarily lives under wsBase (see InstallShim's comment on the
// shared-mount invariant), which a native step can reach by relative traversal
// from its workspace. Verifying before each claim bounds tampering to a single
// run instead of letting it persist across every later containerized job on
// this agent.
func EnsureShimIntact(toolsDir string) (bool, error) {
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	// Compare against the same embedded payload InstallShim writes.
	want := shimPayload() // reuse whatever InstallShim uses; extract a helper if it is inline
	got, err := os.ReadFile(shimPath)
	if err == nil && bytes.Equal(got, want) {
		return false, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read shim: %w", err)
	}
	if err := os.WriteFile(shimPath, want, 0o755); err != nil {
		return false, fmt.Errorf("repair shim: %w", err)
	}
	return true, nil
}
```

If `InstallShim` builds the payload inline, extract it into a small `shimPayload() []byte` helper used by both, so the two can never drift.

- [ ] **Step 4: Call it before each claim**

In the agent's claim loop (`runLoop` in `internal/agent/agent.go`, where the workspace is prepared), call `EnsureShimIntact(a.ToolsDir)` before the run executes, when `a.ToolsDir != ""`. On repair, log at Error — shim tampering is a compromise signal, not routine:

```go
			if a.ToolsDir != "" {
				if repaired, err := EnsureShimIntact(a.ToolsDir); err != nil {
					slog.Error("shim integrity check failed", "error", err, "toolsDir", a.ToolsDir)
				} else if repaired {
					slog.Error("ucd-sh shim was modified on disk and has been restored; a previous step on this agent may have tampered with it",
						"toolsDir", a.ToolsDir, "runId", resp.RunID)
				}
			}
```

A check failure must not fail the run — log and continue, since the repair is best-effort hardening.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/agent/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/agent.go internal/agent/shim_integrity_test.go
git commit -m "fix(agent): verify and repair the ucd-sh shim before each claim"
```

---

### Task 9: C-3 — Digest-pin the default runner and pause images

**Files:**
- Modify: `cmd/agent/main.go:73-74`, plus the k8s-agent equivalents (`cmd/k8s-agent/`, `internal/k8sagent/`) and `internal/config/agent.go` defaults if they carry duplicates
- Test: `cmd/agent/main_test.go` (or the nearest existing test for flag defaults)

**Interfaces:** none consumed by later tasks.

`busybox:1.36` and `ghcr.io/eirueimi/unified-cd-runner:v0.0.3` are mutable tags. Whoever controls the runner repository can force-push the tag and execute code in the primary container of every isolated job lacking a `podTemplate` job container. Job-author `image:` values stay untouched — a job author can already run arbitrary code in their own job.

- [ ] **Step 1: Find the real digests**

```bash
docker buildx imagetools inspect ghcr.io/eirueimi/unified-cd-runner:v0.0.3 | grep -i digest
docker buildx imagetools inspect busybox:1.36 | grep -i digest
```

Use the manifest-list digest. Record both in the commit message so the pin is auditable.

- [ ] **Step 2: Write the failing test**

Add to the agent's flag-default test:

```go
func TestDefaultImagesAreDigestPinned(t *testing.T) {
	// A mutable tag lets a registry compromise execute code in every isolated
	// job on the fleet. Defaults must carry an immutable digest.
	for name, img := range map[string]string{
		"runner": defaultRunnerImage,
		"pause":  defaultPauseImage,
	} {
		assert.Contains(t, img, "@sha256:", "default %s image must be digest-pinned", name)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./cmd/agent/ -run TestDefaultImagesAreDigestPinned -v`
Expected: FAIL — defaults are bare tags (and the constants may not exist yet).

- [ ] **Step 4: Implement**

Extract the two defaults into named constants next to the flag definitions in `cmd/agent/main.go`, keeping the tag alongside the digest for readability (the digest is what the runtime enforces):

```go
// Default images are digest-pinned: the tag is retained for readability, but the
// digest is what is pulled. A mutable tag would let a registry compromise execute
// code in the primary container of every isolated job on the fleet.
// Rotate these together with the runner image release (see docs).
const (
	defaultRunnerImage = "ghcr.io/eirueimi/unified-cd-runner:v0.0.3@sha256:<digest-from-step-1>"
	defaultPauseImage  = "busybox:1.36@sha256:<digest-from-step-1>"
)
```

Use them in the two `flag.String` defaults. Apply the same treatment to the k8s-agent defaults — locate them with:

```bash
grep -rn "unified-cd-runner\|busybox:" --include=*.go cmd/ internal/ | grep -v _test
```

- [ ] **Step 5: Run the tests**

Run: `go test ./cmd/... ./internal/agent/ ./internal/k8sagent/ -count=1`
Expected: PASS.

- [ ] **Step 6: Document the rotation procedure**

In `docs/`, add: the default runner/pause images are digest-pinned; when the runner image is rebuilt and re-tagged, the pinned digest **must** be updated in `cmd/agent/main.go` (and the k8s-agent equivalent) as part of the release, otherwise agents keep pulling the old image. Include the `docker buildx imagetools inspect` command.

- [ ] **Step 7: Commit**

```bash
git add cmd/ internal/ docs/
git commit -m "fix: digest-pin the default runner and pause images"
```

---

### Task 10: Docs, migration guide, and full verification

**Files:**
- Create: `docs/migration-2026-07-security-hardening.md`
- Modify: `docs/configuration.md`, `docs/secrets.md`, `docs/authorization.md`, `docs/jobs.md` as needed

- [ ] **Step 1: Write the migration guide**

Create `docs/migration-2026-07-security-hardening.md` covering every breaking change, each with the symptom an operator will see and the exact fix:

1. **Step environment is now an allowlist.** A step no longer inherits the agent's environment. Symptom: a variable a job relied on is empty. Fix: name it in `--expose-env` / `UNIFIED_AGENT_EXPOSE_ENV` / `exposeEnv`. Agent credentials can never be exposed this way, by design.
2. **Legacy shared-token agents are subject to per-run ownership.** Symptom: 403 when an agent writes to a run it did not claim. Fix: migrate to per-agent enrollment (PR #63); compatibility mode remains for connectivity only.
3. **Secret fetch is limited to the run's declared secrets.** Symptom: 403 "secret not needed by this run". Fix: reference the secret in the job spec so it appears in `SecretsNeeded`.
4. **`WebhookReceiver.spec.auth` is required.** Symptom: apply fails. Fix: declare a real auth type, or `auth: {type: none, allowUnauthenticated: true}` to keep it public deliberately.
5. **Payload-mapped params must declare `pattern:` (or `unvalidated: true`).** Symptom: apply fails naming the param. Fix: add a `pattern:` to the job input; suggested `^[A-Za-z0-9._/-]+$`.
6. **Cache entries are namespaced per job and `ttlDays` is capped at 365.** Symptom: a one-time cache miss after upgrade; `restoreKeys` no longer match other jobs' entries. Fix: none needed — caches regenerate.

- [ ] **Step 2: Update the reference docs**

Reflect the new behavior in `docs/configuration.md` (`exposeEnv` now governs step execution), `docs/secrets.md` (the "fetch only what the run needs" guarantee is now enforced — the doc previously overstated it), `docs/authorization.md` (legacy agents are guarded), and the job/param reference for `pattern:`/`unvalidated:`.

- [ ] **Step 3: Full sweep**

Run each; all must be clean:
- `go build ./...`
- `go generate ./...` then `git status --porcelain` — must be drift-free. (Known Windows stat-cache artifact: if a generated file is flagged, confirm with `git diff` that it is byte-identical and `git checkout` it.)
- `go vet ./internal/... ./cmd/...`
- `go test ./... -count=1` — full suite. Known transient `internal/cli` flake: isolate-rerun that package up to 3× to confirm. Any other failure is a real regression.

- [ ] **Step 4: Live verification against the compose stack**

C-2 and C-3 can pass unit tests while breaking the real dind path, so exercise them for real:

```bash
docker compose up -d --build controller agent
export UNIFIED_SERVER=http://localhost:8080 UNIFIED_TOKEN=dev-token-change-me
./bin/unified-cli.exe apply -f examples/jobs/hello.yaml
./bin/unified-cli.exe run trigger hello-docker --follow --wait --timeout 3m   # isolated job: digest-pinned image + shim entrypoint
./bin/unified-cli.exe apply -f examples/jobs/cache.yaml
./bin/unified-cli.exe run trigger cache-demo --follow --wait --timeout 3m     # namespaced cache save/restore
./bin/unified-cli.exe apply -f examples/jobs/artifacts.yaml
./bin/unified-cli.exe run trigger artifact-roundtrip --follow --wait --timeout 3m
```

All three must reach `Succeeded`. A `exit status 127` from a container step means the shim/tools mount broke — revisit Task 8 without relocating `toolsDir`.

- [ ] **Step 5: Commit**

```bash
git add docs/
git commit -m "docs: security hardening migration guide and reference updates"
```

---

## Notes for the executor

- Order: 1 (A-1) → 2 (A-4) → 3 (A-3) → 4 (A-2) → 5 (B-1) → 6 (B-2) → 7 (C-1) → 8 (C-2) → 9 (C-3) → 10 (docs+sweep). Tasks 2-4 all touch controller agent routes; keeping them in this order avoids conflicts. Everything else is independent.
- **Verify every signature against the code before writing tests.** Confirmed at plan time: `RunStep`/`RunStepWithShell`/`RunStepCapture` (`internal/agent/runner.go:98/127/153`), `collectEnv` (`agent.go:152`), `collectSecretNames(tpl string, seen map[string]struct{})` (`api_agent.go:440`), `agentRunGuard(ctx, agentID, runID string, rejectTerminal bool) (runWriteVerdict, error)` (`agent_guard.go:98`), `respondRunWriteVerdict` (`:132`), `resolveParams(inputs []dsl.Input, supplied map[string]string) (map[string]string, error)` (`params.go:22`), `dsl.Input` (`types.go:55-61`), `InstallShim(workspaceDir string) (toolsDir string, err error)` (`agent.go:680`), cache callers (`backend_host.go:201,212,238,245`; `cmd/unified-sidecar/run.go:80,101`).
- Where a task says "reuse the existing helper/pattern", read the surrounding tests first and match them — do not introduce a new mocking framework.
- If a pre-existing test asserts the vulnerable behavior, it encoded the bug: update it and call that out in the commit message and the task report.
- Full-suite gate before finishing; wait for PR CI green and explicit user approval before any admin merge (merge discipline).
