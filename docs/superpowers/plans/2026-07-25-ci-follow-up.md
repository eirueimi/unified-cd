# CI Follow-up for Resolved Run Snapshots Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve stored run snapshot JSON shape while resolving parameterized secret names, and remove the unrelated claim-loop race from the agent panic-recovery test.

**Architecture:** `prepareRunSpec` will transform the original stored JSON object rather than re-marshaling `dsl.Spec`. A case-insensitive, path-aware walker will update only `run` and `env` strings under `steps`, `parallel`, and `finally`, preserving all other decoded JSON structure. The agent test will disable its unused detached claim pool so only the intended normal slot can consume the injected first-call panic.

**Tech Stack:** Go 1.26, `encoding/json`, existing `internal/dsl` secret-name resolver, `testify`, PostgreSQL-backed controller tests, GitHub Actions.

## Global Constraints

- All repository code, comments, tests, documentation, and commit messages must be in English.
- Do not add PII, personal paths, credentials, or secret values.
- Do not add JSON tags across the DSL type graph.
- Preserve decoded key spelling, field presence, unknown fields, and values outside the approved executable string fields.
- Resolve only `run` and string values in `env` for main, parallel, and finally steps.
- Do not extend resolution to `post.Run` or `post.Env`.
- Do not change production detached-claim behavior.
- Follow red-green TDD and commit each independently reviewable fix.

---

### Task 1: Preserve Stored Snapshot JSON Shape

**Files:**
- Modify: `internal/controller/run_secret_refs.go`
- Modify: `internal/controller/run_secret_refs_test.go`
- Modify: `internal/controller/api_runs.go`
- Modify: `internal/controller/api_webhooks.go`
- Modify: `internal/controller/scheduler.go`
- Test: `internal/controller/api_replay_test.go`

**Interfaces:**
- Consumes: `dsl.ResolveSecretNameParams(tpl string, params map[string]string) (string, error)`
- Produces: `prepareRunSpec(specJSON []byte, params map[string]string) ([]byte, error)`

- [ ] **Step 1: Replace the typed-helper tests with a failing snapshot-shape regression**

Add a test that passes the stored JSON bytes directly and compares decoded
JSON structures so whitespace and object ordering are irrelevant:

```go
func TestPrepareRunSpecPreservesStoredJSONShape(t *testing.T) {
	const unresolved = `{{ index .Secrets .Params.token_secret }}`
	specJSON := []byte(`{
		"agentSelector":["kind:linux"],
		"steps":[
			{"name":"main","run":"` + unresolved + `","env":{"TOKEN":"` + unresolved + `"}},
			{"parallel":[{"name":"parallel","run":"` + unresolved + `","env":{"TOKEN":"` + unresolved + `"}}]}
		],
		"finally":[{"name":"cleanup","run":"` + unresolved + `","env":{"TOKEN":"` + unresolved + `"}}],
		"x-extension":{"preserve":true}
	}`)
	before := append([]byte(nil), specJSON...)

	got, err := prepareRunSpec(specJSON, map[string]string{"token_secret": "deploy-token"})

	require.NoError(t, err)
	assert.Equal(t, before, specJSON, "the caller-owned JSON bytes must not be mutated")
	var actual any
	require.NoError(t, json.Unmarshal(got, &actual))
	var expected any
	require.NoError(t, json.Unmarshal([]byte(`{
		"agentSelector":["kind:linux"],
		"steps":[
			{"name":"main","run":"{{ index .Secrets \"deploy-token\" }}","env":{"TOKEN":"{{ index .Secrets \"deploy-token\" }}"}},
			{"parallel":[{"name":"parallel","run":"{{ index .Secrets \"deploy-token\" }}","env":{"TOKEN":"{{ index .Secrets \"deploy-token\" }}"}}]}
		],
		"finally":[{"name":"cleanup","run":"{{ index .Secrets \"deploy-token\" }}","env":{"TOKEN":"{{ index .Secrets \"deploy-token\" }}"}},
		"x-extension":{"preserve":true}
	}`), &expected))
	assert.Equal(t, expected, actual)
}
```

Retain focused coverage for an empty optional secret name and a runtime-only
operand, but express their fixtures as raw JSON bytes. Remove the old
caller-owned typed-slice mutation test because the new byte-input test covers
the helper's non-mutation contract.

- [ ] **Step 2: Run the shape regression and verify RED**

Run:

```bash
go test ./internal/controller -run TestPrepareRunSpecPreservesStoredJSONShape -count=1
```

Expected: compilation fails because the existing helper accepts `dsl.Spec`, or
the decoded comparison fails with Go-style keys and additional zero-value
fields. The failure must demonstrate the CI regression rather than a malformed
fixture.

- [ ] **Step 3: Add casing and invalid-container regressions**

Add these focused tests before production changes:

```go
func TestPrepareRunSpecPreservesExistingGoStyleKeySpelling(t *testing.T) {
	specJSON := []byte(`{
		"Steps":[{"Name":"deploy","Run":"{{ index .Secrets .Params.token_secret }}",
		"Env":{"TOKEN":"{{ index .Secrets .Params.token_secret }}"}}],
		"Finally":null
	}`)

	got, err := prepareRunSpec(specJSON, map[string]string{"token_secret": "deploy-token"})

	require.NoError(t, err)
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(got, &snapshot))
	assert.Contains(t, snapshot, "Steps")
	assert.NotContains(t, snapshot, "steps")
	assert.Contains(t, snapshot, "Finally")
}

func TestPrepareRunSpecRejectsWrongStepsContainerType(t *testing.T) {
	_, err := prepareRunSpec([]byte(`{"steps":"not-an-array"}`), nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, `field "steps" must be an array`)
}

func TestPrepareRunSpecRejectsNullRoot(t *testing.T) {
	_, err := prepareRunSpec([]byte(`null`), nil)

	require.Error(t, err)
	assert.ErrorContains(t, err, "stored run spec must be an object")
}
```

Run:

```bash
go test ./internal/controller -run 'TestPrepareRunSpec(PreservesStoredJSONShape|PreservesExistingGoStyleKeySpelling|RejectsWrongStepsContainerType|RejectsNullRoot)' -count=1
```

Expected: FAIL against the typed re-marshaling implementation.

- [ ] **Step 4: Implement the raw JSON walker**

Change the helper signature and replace typed cloning with a generic object
walker. The implementation should follow this structure:

```go
func prepareRunSpec(specJSON []byte, params map[string]string) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(specJSON, &root); err != nil {
		return nil, fmt.Errorf("decode stored run spec: %w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("stored run spec must be an object")
	}
	for _, field := range []string{"steps", "finally"} {
		if err := resolveRunSpecEntries(root, field, params); err != nil {
			return nil, fmt.Errorf("resolve secret name parameters: %w", err)
		}
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("marshal resolved run spec: %w", err)
	}
	return raw, nil
}

func findJSONField(object map[string]any, wanted string) (string, any, bool, error) {
	var foundKey string
	var foundValue any
	for key, value := range object {
		if !strings.EqualFold(key, wanted) {
			continue
		}
		if foundKey != "" {
			return "", nil, false, fmt.Errorf(
				"ambiguous fields %q and %q", foundKey, key,
			)
		}
		foundKey, foundValue = key, value
	}
	return foundKey, foundValue, foundKey != "", nil
}
```

Implement `resolveRunSpecEntries`, `resolveRunSpecStep`,
`resolveRunSpecStringField`, and `resolveRunSpecEnv` with these rules:

```go
// resolveRunSpecEntries:
// - absent or null field: no-op
// - present non-array field: error `field "<key>" must be an array`
// - each non-object entry: error with `<key>[<index>]`
// - resolve the entry's run/env
// - if parallel is present, require an array and resolve each child run/env

// resolveRunSpecStringField:
// - absent or null field: no-op
// - present non-string field: return a path-qualified error
// - call dsl.ResolveSecretNameParams and write the result to the original key

// resolveRunSpecEnv:
// - absent or null env: no-op
// - present non-object env: return a path-qualified error
// - resolve every string value; reject non-string, non-null values
// - retain the original env key spelling and variable names
```

Do not recurse into `post`, nested unknown objects, or arbitrary strings.
Delete `cloneRunSpecExecutableFields`, `cloneRunSpecEntries`, and
`cloneRunSpecEnv`; raw JSON decoding already provides isolated mutable values.

- [ ] **Step 5: Pass the original snapshot bytes at every call site**

Make these exact call-site substitutions:

```go
// api_runs.go: createRunFromJob
runSpec, err := prepareRunSpec(job.Spec, params)

// api_runs.go: handleReplayRun
runSpec, err := prepareRunSpec(specJSON, params)

// api_webhooks.go
runSpec, err := prepareRunSpec(job.Spec, params)

// scheduler.go, only when the typed parse succeeded
runSpec, err = prepareRunSpec(job.Spec, params)
```

Keep the preceding typed `json.Unmarshal` operations. They remain responsible
for rejecting malformed or type-invalid stored specs before run creation.
Keep the schedule's explicit malformed-spec raw fallback unchanged.

- [ ] **Step 6: Verify focused controller behavior is GREEN**

Run:

```bash
gofmt -w internal/controller/run_secret_refs.go internal/controller/run_secret_refs_test.go internal/controller/api_runs.go internal/controller/api_webhooks.go internal/controller/scheduler.go
go test ./internal/controller -run 'TestPrepareRunSpec|TestAPI_ReplayRun_UsesSnapshotSpecNotLatestJob|TestAPI_ReplayRunStoresResolvedSecretNameParameter|TestCheckAndFireSchedules' -count=1
go test ./internal/controller -short -count=1
```

Expected: all commands exit 0. The replay snapshot comparison must pass
without weakening `assert.JSONEq`.

- [ ] **Step 7: Commit the snapshot fix**

```bash
git add internal/controller/run_secret_refs.go \
  internal/controller/run_secret_refs_test.go \
  internal/controller/api_runs.go \
  internal/controller/api_webhooks.go \
  internal/controller/scheduler.go
git commit -m "fix: preserve resolved run snapshot shape"
```

---

### Task 2: Isolate the Agent Panic-Recovery Test

**Files:**
- Modify: `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: `Agent.MaxDetachedConcurrent`, where a negative value disables the detached claim pool
- Produces: a deterministic normal-claim panic-recovery regression

- [ ] **Step 1: Record the existing RED evidence**

The failed PR CI run and two earlier `main` runs are the required red
reproduction:

```text
PR run 30154850255:
expected run-panic-prepare = Failed, actual Succeeded
expected run-after-panic = Succeeded, actual Failed

main runs 30145957613 and 30145956112:
the same two statuses are reversed
```

The test starts one normal claim loop and the default sixteen detached loops,
all of which race to consume the global first-call `prepareWorkspaceFn` panic.

- [ ] **Step 2: Disable the unrelated detached pool in this test**

Update only the test's `Agent` literal:

```go
a := &Agent{
	ID:                    "a5",
	Client:                NewClient(srv.URL, "tok"),
	MaxConcurrent:         1,
	MaxDetachedConcurrent: -1,
	WorkspaceDir:          wsDir,
}
```

Do not change `Agent.Run`, default detached concurrency, or any production
code.

- [ ] **Step 3: Verify the test repeatedly with the race detector**

Run:

```bash
gofmt -w internal/agent/agent_test.go
go test ./internal/agent -run TestAgent_RunLoop_PreparePanicIsRecoveredAndFailsRun -race -count=20
go test ./internal/agent -short -race -count=1
```

Expected: both commands exit 0 with no reversed statuses and no race report.

- [ ] **Step 4: Commit the deterministic test fix**

```bash
git add internal/agent/agent_test.go
git commit -m "test: isolate prepare panic recovery claim loop"
```

---

### Task 3: Verify CI Equivalence and Update PR #99

**Files:**
- Verify only: `.github/workflows/ci.yml`
- Verify only: all changed files from Tasks 1 and 2

**Interfaces:**
- Consumes: commits from Tasks 1 and 2
- Produces: a green PR #99 branch at `origin/dynamic-secret-resolution`

- [ ] **Step 1: Run formatting and diff checks**

```bash
gofmt -w internal/controller/run_secret_refs.go \
  internal/controller/run_secret_refs_test.go \
  internal/controller/api_runs.go \
  internal/controller/api_webhooks.go \
  internal/controller/scheduler.go \
  internal/agent/agent_test.go
git diff --check
git status --short
```

Expected: no formatting diff, no whitespace errors, and no uncommitted files.

- [ ] **Step 2: Run the CI unit command**

Run the workflow-equivalent command:

```bash
go test ./... -short -race -count=1
```

Expected: exit 0. On Windows, ensure Git for Windows `bash`, `cat`, and `ls`
precede the WSL launcher on `PATH`; this changes only the local test
environment.

- [ ] **Step 3: Run the CI integration command**

With the repository's PostgreSQL test dependency available, run:

```bash
go test ./... -race -count=1
```

Expected: exit 0, including
`TestAPI_ReplayRun_UsesSnapshotSpecNotLatestJob`.

- [ ] **Step 4: Push the reviewed commits**

```bash
git push origin dynamic-secret-resolution
```

Expected: `origin/dynamic-secret-resolution` advances to the local HEAD and PR
#99 receives a new CI run.

- [ ] **Step 5: Watch PR checks to a terminal state**

```bash
gh pr checks 99 --repo eirueimi/unified-cd --watch
```

Expected: unit tests on Ubuntu, Windows, and macOS; Linux/PostgreSQL
integration; and Kubernetes integration all pass. If any check fails, inspect
that job's first root error before making another change.
