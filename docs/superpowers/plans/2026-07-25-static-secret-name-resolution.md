# Static Secret Name Resolution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Resolve parameter-selected secret names to literal references before execution so existing reusable templates receive their declared secrets without weakening the agent secret-fetch allowlist.

**Architecture:** Add one DSL utility that rewrites `index .Secrets .Params.NAME` in step `run` and `env` templates using a known parameter map, and another utility that extracts only statically provable secret names. Apply the rewrite when a normal run snapshot is created and when a `uses:` template has assembled its input values; reject any remaining runtime-dependent secret index when the controller builds a claim or authorizes a secret fetch.

**Tech Stack:** Go 1.24, Go `text/template` syntax, regular expressions for the deliberately narrow supported forms, unified-cd Job/JobTemplate DSL, controller claim and secret-fetch APIs, `testify`.

## Global Constraints

- Preserve the current `uses.with.token_secret` YAML contract; do not add `uses.secrets`.
- Support only literal secret names and names resolved from `.Params.NAME` before step execution.
- Treat an empty optional secret-name parameter as no secret dependency.
- Reject `.Steps`, `.Matrix`, `.Foreach`, and other runtime-only operands to `index .Secrets`.
- Never fetch, log, persist, or interpolate a secret value during name resolution.
- Keep claim-time `SecretsNeeded` and secret-fetch authorization on the same extraction function.
- All repository text, code, comments, documentation, and commit messages must be in English.

---

### Task 1: Add the DSL Secret Reference Resolver and Extractor

**Files:**
- Create: `internal/dsl/secretrefs.go`
- Create: `internal/dsl/secretrefs_test.go`

**Interfaces:**
- Produces: `ResolveSecretNameParams(tpl string, params map[string]string) (string, error)`
- Produces: `ResolveSecretNameParamsInSpec(spec *Spec, params map[string]string) error`
- Produces: `ReferencedSecretNames(tpl string) ([]string, error)`
- Consumes: existing `ValidateSecretName(name string) error`

- [ ] **Step 1: Write failing tests for literal and parameter resolution**

Create table-driven tests covering a literal, a populated parameter, an empty
optional parameter, malformed non-empty values, templated values, and
runtime-only operands:

```go
func TestResolveSecretNameParams(t *testing.T) {
	tests := []struct {
		name    string
		tpl     string
		params  map[string]string
		want    string
		wantErr string
	}{
		{
			name: "literal remains literal",
			tpl:  `{{ index .Secrets "gitlab-token" }}`,
			want: `{{ index .Secrets "gitlab-token" }}`,
		},
		{
			name:   "parameter becomes literal",
			tpl:    `{{ index .Secrets .Params.token_secret }}`,
			params: map[string]string{"token_secret": "gitlab-token"},
			want:   `{{ index .Secrets "gitlab-token" }}`,
		},
		{
			name:   "empty optional parameter becomes empty literal",
			tpl:    `{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}`,
			params: map[string]string{"token_secret": ""},
			want:   `{{ if .Params.token_secret }}{{ index .Secrets "" }}{{ end }}`,
		},
		{
			name:    "templated name is rejected",
			tpl:     `{{ index .Secrets .Params.token_secret }}`,
			params:  map[string]string{"token_secret": "{{ .Params.outer_secret }}"},
			wantErr: `secret name parameter "token_secret" must be a literal secret name`,
		},
		{
			name:    "step output is rejected",
			tpl:     `{{ index .Secrets .Steps.detect.Outputs.secret_name }}`,
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
		{
			name:    "matrix value is rejected",
			tpl:     `{{ index .Secrets .Matrix.secret_name }}`,
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
		{
			name:    "foreach value is rejected",
			tpl:     `{{ index .Secrets .Foreach.secret_name }}`,
			wantErr: "dynamic secret name must be resolved from a parameter before execution",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveSecretNameParams(tt.tpl, tt.params)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
```

- [ ] **Step 2: Write failing tests for extraction and spec traversal**

Verify direct spellings, literal index notation, deduplication by callers, and
failure on unresolved dynamic indexes. Also construct a `Spec` with main,
parallel, and finally steps and prove that only `Run` and `Env` strings are
rewritten consistently:

```go
func TestReferencedSecretNames(t *testing.T) {
	got, err := ReferencedSecretNames(
		`echo {{ secrets.API_TOKEN }} {{ .Secrets.DB_PASS }} {{ index .Secrets "gitlab-token" }}`,
	)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"API_TOKEN", "DB_PASS", "gitlab-token"}, got)
}

func TestReferencedSecretNamesRejectsRuntimeOperand(t *testing.T) {
	_, err := ReferencedSecretNames(`{{ index .Secrets .Steps.pick.Outputs.name }}`)
	require.ErrorContains(t, err, "dynamic secret name must be resolved from a parameter before execution")
}

func TestResolveSecretNameParamsInSpec(t *testing.T) {
	spec := Spec{
		Steps: []StepEntry{
			{Name: "main", Env: map[string]string{
				"TOKEN": `{{ index .Secrets .Params.token_secret }}`,
			}},
			{Parallel: []Step{{
				Name: "parallel",
				Run:  `use {{ index .Secrets .Params.token_secret }}`,
			}}},
		},
		Finally: []StepEntry{{
			Name: "cleanup",
			Env: map[string]string{
				"TOKEN": `{{ index .Secrets .Params.token_secret }}`,
			},
		}},
	}

	require.NoError(t, ResolveSecretNameParamsInSpec(
		&spec,
		map[string]string{"token_secret": "gitlab-token"},
	))
	assert.Contains(t, spec.Steps[0].Env["TOKEN"], `"gitlab-token"`)
	assert.Contains(t, spec.Steps[1].Parallel[0].Run, `"gitlab-token"`)
	assert.Contains(t, spec.Finally[0].Env["TOKEN"], `"gitlab-token"`)
}
```

- [ ] **Step 3: Run the DSL tests and verify they fail**

Run:

```bash
go test ./internal/dsl -run 'TestResolveSecretNameParams|TestReferencedSecretNames' -count=1
```

Expected: build failure because the three new functions do not exist.

- [ ] **Step 4: Implement the narrow template resolver**

In `internal/dsl/secretrefs.go`, recognize only a quoted literal or a simple
`.Params.NAME` operand. Preserve surrounding template text and quote resolved
names with `strconv.Quote`:

```go
var (
	secretIndexRe = regexp.MustCompile(
		`index[ \t]+\.Secrets[ \t]+("(?:\\.|[^"\\])*"|[^\s}]+)`,
	)
	secretParamOperandRe = regexp.MustCompile(
		`^\.Params\.([A-Za-z_][A-Za-z0-9_]*)$`,
	)
	directSecretRefRe = regexp.MustCompile(
		`(?:secrets|\.Secrets)\.([A-Za-z_][A-Za-z0-9_-]*)`,
	)
)

func ResolveSecretNameParams(tpl string, params map[string]string) (string, error) {
	var resolveErr error
	out := secretIndexRe.ReplaceAllStringFunc(tpl, func(match string) string {
		if resolveErr != nil {
			return match
		}
		sub := secretIndexRe.FindStringSubmatch(match)
		operand := sub[1]
		if strings.HasPrefix(operand, `"`) {
			name, err := strconv.Unquote(operand)
			if err != nil {
				resolveErr = fmt.Errorf("invalid secret name literal: %w", err)
				return match
			}
			if name != "" {
				if err := ValidateSecretName(name); err != nil {
					resolveErr = fmt.Errorf("secret name %q %w", name, err)
				}
			}
			return match
		}

		param := secretParamOperandRe.FindStringSubmatch(operand)
		if len(param) != 2 {
			resolveErr = fmt.Errorf(
				"dynamic secret name must be resolved from a parameter before execution",
			)
			return match
		}
		paramName := param[1]
		name := params[paramName]
		if name != "" {
			if strings.Contains(name, "{{") || strings.Contains(name, "}}") {
				resolveErr = fmt.Errorf(
					"secret name parameter %q must be a literal secret name",
					paramName,
				)
				return match
			}
			if err := ValidateSecretName(name); err != nil {
				resolveErr = fmt.Errorf(
					"secret name parameter %q %w",
					paramName,
					err,
				)
				return match
			}
		}
		prefix := strings.TrimSuffix(match, operand)
		return prefix + strconv.Quote(name)
	})
	return out, resolveErr
}
```

- [ ] **Step 5: Implement extraction and `Spec` traversal**

Implement `ReferencedSecretNames` by collecting direct references and quoted
literal index references, skipping an empty literal, validating non-empty
names, and returning the same runtime-operand error for every non-literal
index. Implement private entry/step helpers so `ResolveSecretNameParamsInSpec`
rewrites `Run` and every `Env` value in `Steps`, `Parallel`, and `Finally`.

Use this shape for the entry walker:

```go
func resolveSecretNameParamsInEntries(entries []StepEntry, params map[string]string) error {
	for i := range entries {
		entry := &entries[i]
		if len(entry.Parallel) > 0 {
			for j := range entry.Parallel {
				step := &entry.Parallel[j]
				run, err := ResolveSecretNameParams(step.Run, params)
				if err != nil {
					return fmt.Errorf("step %q run: %w", step.Name, err)
				}
				step.Run = run
				for key, value := range step.Env {
					resolved, err := ResolveSecretNameParams(value, params)
					if err != nil {
						return fmt.Errorf("step %q env %q: %w", step.Name, key, err)
					}
					step.Env[key] = resolved
				}
			}
			continue
		}
		run, err := ResolveSecretNameParams(entry.Run, params)
		if err != nil {
			return fmt.Errorf("step %q run: %w", entry.Name, err)
		}
		entry.Run = run
		for key, value := range entry.Env {
			resolved, err := ResolveSecretNameParams(value, params)
			if err != nil {
				return fmt.Errorf("step %q env %q: %w", entry.Name, key, err)
			}
			entry.Env[key] = resolved
		}
	}
	return nil
}
```

- [ ] **Step 6: Run focused and package tests**

Run:

```bash
go test ./internal/dsl -run 'TestResolveSecretNameParams|TestReferencedSecretNames' -count=1
go test ./internal/dsl -count=1
```

Expected: both commands pass.

- [ ] **Step 7: Commit the DSL unit**

```bash
git add internal/dsl/secretrefs.go internal/dsl/secretrefs_test.go
git commit -m "feat: resolve parameterized secret names"
```

---

### Task 2: Resolve JobTemplate Secret Inputs Before `uses:` Rewriting

**Files:**
- Modify: `internal/gittemplate/inline.go`
- Modify: `internal/gittemplate/inline_test.go`
- Create: `internal/gittemplate/secret_refs_test.go`

**Interfaces:**
- Consumes: `dsl.ResolveSecretNameParamsInSpec(spec *dsl.Spec, params map[string]string) error`
- Produces: expanded `uses:` steps whose supported secret indexes contain quoted literal names

- [ ] **Step 1: Write failing `expandUsesStep` tests**

Add tests proving a supplied input and a default input are resolved, while an
empty optional input remains an empty literal:

```go
func TestExpandUsesStepResolvesSecretNameInput(t *testing.T) {
	tpl := dsl.Spec{
		Params: dsl.Params{Inputs: []dsl.Input{{
			Name: "token_secret",
			Type: "string",
		}}},
		Steps: []dsl.StepEntry{{
			Name: "checkout",
			Env: map[string]string{
				"GIT_TOKEN": `{{ index .Secrets .Params.token_secret }}`,
			},
			Run: "true",
		}},
	}

	steps, err := ExpandUsesStep(
		"checkout",
		map[string]string{"token_secret": "gitlab-token"},
		tpl,
		nil,
		"",
		"",
	)
	require.NoError(t, err)
	require.Len(t, steps, 3)
	assert.Equal(
		t,
		`{{ index .Secrets "gitlab-token" }}`,
		steps[1].Env["GIT_TOKEN"],
	)
}
```

Use the same fixture with these exact input/expectation pairs:

```go
tests := []struct {
	name    string
	input   dsl.Input
	with    map[string]string
	want    string
	wantErr string
}{
	{
		name:  "template default",
		input: dsl.Input{Name: "token_secret", Type: "string", Default: "github-token"},
		want:  `{{ index .Secrets "github-token" }}`,
	},
	{
		name:  "empty optional default",
		input: dsl.Input{Name: "token_secret", Type: "string", Default: ""},
		want:  `{{ index .Secrets "" }}`,
	},
	{
		name:    "templated with value",
		input:   dsl.Input{Name: "token_secret", Type: "string"},
		with:    map[string]string{"token_secret": "{{ .Params.outer_secret }}"},
		wantErr: `secret name parameter "token_secret" must be a literal secret name`,
	},
}
```

Add a separate template step whose environment contains
`{{ index .Secrets .Steps.pick.Outputs.name }}` and assert the
`dynamic secret name must be resolved from a parameter before execution`
error.

- [ ] **Step 2: Write a failing resolver test using the documented checkout shape**

Create `internal/gittemplate/secret_refs_test.go` with an inline JobTemplate
that uses the guarded `git-checkout` expression:

```go
const secretRefTemplate = `
apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: checkout
spec:
  params:
    inputs:
      - name: token_secret
        type: string
        default: ""
  steps:
    - name: checkout
      env:
        GIT_TOKEN: "{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}"
      run: "true"
`
```

Resolve a caller spec with `token_secret: gitlab-token` and assert that the
expanded `checkout__checkout` environment contains
`index .Secrets "gitlab-token"` and no longer contains
`index .Secrets .Steps`.

- [ ] **Step 3: Run the git-template tests and verify they fail**

Run:

```bash
go test ./internal/gittemplate -run 'SecretName|SecretRef' -count=1
```

Expected: assertions show the current synthetic
`.Steps.checkout__inputs.Outputs.token_secret` operand.

- [ ] **Step 4: Resolve template inputs before reference rewriting**

In `expandUsesStep`, immediately after `inputsOutputs` has been assembled from
defaults and `with`, resolve secret-name parameters in the local `tplSpec`
copy:

```go
if err := dsl.ResolveSecretNameParamsInSpec(&tplSpec, inputsOutputs); err != nil {
	return nil, podContribution{}, fmt.Errorf(
		"uses %q: resolve secret name parameters: %w",
		usesName,
		err,
	)
}
```

Place this before `renameInnerEntry` processes either body or finally steps.
Do not change ordinary `.Params` rewriting; only the operand of a supported
secret index becomes literal.

- [ ] **Step 5: Run focused and package tests**

Run:

```bash
go test ./internal/gittemplate -run 'SecretName|SecretRef' -count=1
go test ./internal/gittemplate -count=1
```

Expected: both commands pass, including existing `uses:` composition tests.

- [ ] **Step 6: Commit template resolution**

```bash
git add internal/gittemplate/inline.go internal/gittemplate/inline_test.go internal/gittemplate/secret_refs_test.go
git commit -m "fix: resolve template secret name inputs"
```

---

### Task 3: Resolve Normal Job Parameters in Every Run Snapshot

**Files:**
- Create: `internal/controller/run_secret_refs.go`
- Create: `internal/controller/run_secret_refs_test.go`
- Modify: `internal/controller/api_runs.go`
- Modify: `internal/controller/api_webhooks.go`
- Modify: `internal/controller/scheduler.go`

**Interfaces:**
- Consumes: `dsl.ResolveSecretNameParamsInSpec(spec *dsl.Spec, params map[string]string) error`
- Produces: `prepareRunSpec(spec dsl.Spec, params map[string]string) ([]byte, error)`
- Produces: all API, child-call, replay, webhook, and scheduled runs storing a parameter-resolved spec snapshot

- [ ] **Step 1: Write failing tests for run-spec preparation**

Create tests for a populated parameter, an empty optional parameter, and a
runtime-only operand:

```go
func TestPrepareRunSpecResolvesSecretNameParameter(t *testing.T) {
	spec := dsl.Spec{Steps: []dsl.StepEntry{{
		Name: "deploy",
		Env: map[string]string{
			"TOKEN": `{{ index .Secrets .Params.token_secret }}`,
		},
		Run: "true",
	}}}

	raw, err := prepareRunSpec(
		spec,
		map[string]string{"token_secret": "deploy-token"},
	)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `index .Secrets \"deploy-token\"`)
	assert.NotContains(t, string(raw), `.Params.token_secret`)
}
```

The runtime-only case must contain the exact early error:

```text
dynamic secret name must be resolved from a parameter before execution
```

- [ ] **Step 2: Run the focused test and verify it fails**

Run:

```bash
go test ./internal/controller -run TestPrepareRunSpec -count=1
```

Expected: build failure because `prepareRunSpec` does not exist.

- [ ] **Step 3: Implement the shared snapshot helper**

Create `internal/controller/run_secret_refs.go`:

```go
func prepareRunSpec(spec dsl.Spec, params map[string]string) ([]byte, error) {
	if err := dsl.ResolveSecretNameParamsInSpec(&spec, params); err != nil {
		return nil, fmt.Errorf("resolve secret name parameters: %w", err)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return nil, fmt.Errorf("marshal resolved run spec: %w", err)
	}
	return raw, nil
}
```

- [ ] **Step 4: Use the helper for API, child-call, and replay runs**

In `createRunFromJob`, call `prepareRunSpec(spec, params)` after parameter and
selector resolution and pass the returned bytes to `CreateRun`. This covers
both human API triggers and agent child runs because both already share
`createRunFromJob`.

In `handleReplayRun`, run the same helper against the snapshot `spec` and
validated original parameters before creating the replay. Return HTTP 400 with
the helper error for deterministic invalid references.

- [ ] **Step 5: Use the helper for webhook and scheduled runs**

In the webhook handler, prepare the run spec after `resolveParams` and before
`CreateRun`; return HTTP 400 on a deterministic resolution error.

In `checkAndFireSchedules`, prepare the spec only when JSON parsing succeeded.
On resolution failure, log:

```text
checkAndFireSchedules: secret name resolution failed
```

Skip that firing without updating `last_fired_at`, matching existing parameter
and selector validation failures. Preserve the existing best-effort raw-spec
path when the stored JSON itself cannot be parsed.

- [ ] **Step 6: Add an API-trigger snapshot regression test**

Apply a Job with:

```yaml
params:
  inputs:
    - name: token_secret
      type: string
steps:
  - name: deploy
    env:
      TOKEN: "{{ index .Secrets .Params.token_secret }}"
    run: "true"
```

Trigger it with `token_secret=gitlab-token`, load its stored run spec, and
assert that the environment contains `index .Secrets "gitlab-token"`.

- [ ] **Step 7: Run focused and controller tests**

Run:

```bash
go test ./internal/controller -run 'TestPrepareRunSpec|SecretNameParameter' -count=1
go test ./internal/controller -short -count=1
```

Expected: both commands pass.

- [ ] **Step 8: Commit run snapshot resolution**

```bash
git add internal/controller/run_secret_refs.go internal/controller/run_secret_refs_test.go internal/controller/api_runs.go internal/controller/api_webhooks.go internal/controller/scheduler.go
git commit -m "fix: resolve secret names in run snapshots"
```

---

### Task 4: Enforce Static Secret Names in Claims and Fetch Authorization

**Files:**
- Modify: `internal/controller/api_agent.go`
- Modify: `internal/controller/planned_steps.go`
- Modify: `internal/controller/api_agent_test.go`
- Modify: `internal/controller/api_secrets_test.go`

**Interfaces:**
- Consumes: `dsl.ReferencedSecretNames(tpl string) ([]string, error)`
- Produces: `collectSecretNames(tpl string, seen map[string]struct{}) error`
- Produces: identical claim `SecretsNeeded` and secret-fetch allowlists for literal index references

- [ ] **Step 1: Write a failing claim test for literal index collection**

Extend the existing claim-response secret test with:

```json
{
  "name": "checkout",
  "env": {
    "GIT_TOKEN": "{{ index .Secrets \"gitlab-token\" }}"
  },
  "run": "git clone https://example.invalid/repo.git"
}
```

Expect:

```go
assert.ElementsMatch(
	t,
	[]string{"AWS_ACCESS_KEY_ID", "DB_PASS", "gitlab-token"},
	got.SecretsNeeded,
)
```

- [ ] **Step 2: Write failing authorization tests**

Create a run whose spec contains a literal index reference, store
`gitlab-token`, and prove `/secrets/fetch` accepts that name. Add a second run
containing:

```gotemplate
{{ index .Secrets .Steps.pick.Outputs.name }}
```

and prove both claim construction and `secretNamesForRun` return the
runtime-dynamic error rather than an empty allowlist.

- [ ] **Step 3: Run the focused tests and verify they fail**

Run:

```bash
go test ./internal/controller -run 'CollectsSecretsNeeded|LiteralIndex|DynamicSecret' -count=1
```

Expected: the literal name is absent from `SecretsNeeded`, and the dynamic
reference is not rejected.

- [ ] **Step 4: Make collection return extraction errors**

Replace the manual prefix scanner with the DSL extractor:

```go
func collectSecretNames(tpl string, seen map[string]struct{}) error {
	names, err := dsl.ReferencedSecretNames(tpl)
	if err != nil {
		return err
	}
	for _, name := range names {
		seen[name] = struct{}{}
	}
	return nil
}
```

Change `buildStages` to return `([]api.ClaimStage, error)`. Wrap extraction
failures with the step name and field (`run` or `env KEY`). Update:

- `buildClaimResponse` to propagate errors from both main and finally stages.
- `secretNamesForRun` to propagate the same errors.
- `plannedSteps` to ignore the error only for UI planning while preserving the
  successfully constructed stage prefix; claim execution remains the
  enforcement boundary.

Use the same `buildStages` function in both security-sensitive paths.

- [ ] **Step 5: Run focused and controller tests**

Run:

```bash
go test ./internal/controller -run 'CollectsSecretsNeeded|LiteralIndex|DynamicSecret|RejectsNameNotNeededByRun' -count=1
go test ./internal/controller -short -count=1
```

Expected: both commands pass and the existing unauthorized-name rejection
remains unchanged.

- [ ] **Step 6: Commit claim and authorization enforcement**

```bash
git add internal/controller/api_agent.go internal/controller/planned_steps.go internal/controller/api_agent_test.go internal/controller/api_secrets_test.go
git commit -m "fix: authorize literal indexed secrets"
```

---

### Task 5: Update Documentation and Verify Existing Templates

**Files:**
- Modify: `templates/README.md`
- Modify: `docs/jobs.md`
- Modify: `docs/troubleshooting.md`
- Test: `internal/dsl/templates_parse_test.go`
- Test: `internal/dsl/examples_parse_test.go`

**Interfaces:**
- Consumes: the implemented static-resolution and error behavior
- Produces: user documentation describing supported and rejected indirect secret references

- [ ] **Step 1: Update template documentation**

Keep the existing indirect-reference example, but document that
`.Params.NAME` is resolved before execution, an empty optional parameter adds
no dependency, and runtime expressions are rejected:

```markdown
Secret-name parameters are resolved to literal secret references before a run
starts. `index .Secrets .Params.token_secret` is supported when
`token_secret` is a literal `with:` value or template default. Secret names
selected from `.Steps`, `.Matrix`, or `.Foreach` are rejected because the
controller cannot authorize them before execution.
```

- [ ] **Step 2: Update the Job reference and troubleshooting guide**

Add the supported forms to `docs/jobs.md`:

```gotemplate
{{ .Secrets.API_TOKEN }}
{{ index .Secrets "gitlab-token" }}
{{ index .Secrets .Params.token_secret }}
```

Add a troubleshooting entry keyed by this exact error:

```text
dynamic secret name must be resolved from a parameter before execution
```

Explain that users must pass a literal secret name through a Job parameter,
JobTemplate default, or `uses.with`, rather than a normal step output.

- [ ] **Step 3: Run documentation and template hygiene checks**

Run:

```bash
rg -n 'index \.Secrets' docs examples templates README.md
go test ./internal/dsl -run 'Templates|Examples' -count=1
```

Review every match and confirm all shipped templates use only the supported
literal or `.Params.NAME` operands.

- [ ] **Step 4: Run formatting and focused regression tests**

Run:

```bash
gofmt -w internal/dsl/secretrefs.go internal/dsl/secretrefs_test.go internal/gittemplate/inline.go internal/gittemplate/inline_test.go internal/gittemplate/secret_refs_test.go internal/controller/run_secret_refs.go internal/controller/run_secret_refs_test.go internal/controller/api_runs.go internal/controller/api_webhooks.go internal/controller/scheduler.go internal/controller/api_agent.go internal/controller/planned_steps.go internal/controller/api_agent_test.go internal/controller/api_secrets_test.go
go test ./internal/dsl ./internal/gittemplate ./internal/controller -short -count=1
```

Expected: formatting produces no unintended files and all three packages pass.

- [ ] **Step 5: Run the complete short suite**

Run:

```bash
go test -short ./...
```

Expected: exit code 0.

- [ ] **Step 6: Verify the Unity checkout regression shape**

Resolve a fixture matching `git-checkout.yaml` with
`token_secret: gitlab-token`, build a claim from the resolved spec, and confirm
the test observes:

```text
SecretsNeeded contains gitlab-token
GIT_TOKEN contains {{ index .Secrets "gitlab-token" }}
```

Do not print or fetch the actual secret value.

- [ ] **Step 7: Commit documentation**

```bash
git add templates/README.md docs/jobs.md docs/troubleshooting.md
git commit -m "docs: explain static secret name resolution"
```

- [ ] **Step 8: Review the final branch**

Run:

```bash
git status --short
git diff --check main...HEAD
git log --oneline --decorate main..HEAD
```

Expected: clean status, no whitespace errors, and focused commits for the DSL,
template resolver, controller snapshot handling, authorization, and docs.
