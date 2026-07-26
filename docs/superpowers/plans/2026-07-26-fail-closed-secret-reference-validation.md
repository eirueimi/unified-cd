# Fail-Closed Secret Reference Validation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace open-ended secret-map data-flow tracking with a fail-closed AST context allowlist that preserves static Unity checkout and license references while rejecting every non-canonical secret-map use.

**Architecture:** Keep textual `.Params` secret-name resolution and literal-name extraction in `internal/dsl/secretrefs.go`. Move authorization parsing and AST policy enforcement into a focused `internal/dsl/secretrefs_ast.go` validator shared by run preparation and claim/fetch extraction. Permit only canonical literal indexes and direct two-segment `.Secrets.NAME` values; reject reserved `Secrets` namespace access everywhere else without modeling function return values.

**Tech Stack:** Go 1.26, `text/template`, `text/template/parse`, `testify`, existing unified-cd DSL/controller/gittemplate tests, GitHub Actions.

## Global Constraints

- All code, comments, tests, documentation, and commit messages must be in English.
- Do not add PII, personal filesystem paths, secret values, tokens, or decrypted data to repository files, logs, fixtures, or assertions.
- Work only in the existing `dynamic-secret-resolution` worktree and branch.
- Preserve the existing error text exactly: `dynamic secret name must be resolved from a parameter before execution`.
- Preserve exact pre-resolution support for `index .Secrets .Params.NAME`; resolve it to a quoted literal before AST validation.
- Preserve canonical `index .Secrets "literal-name"`, `.Secrets.NAME`, and `secrets.NAME` references.
- Preserve empty optional secret-name parameters as `index .Secrets ""`, with no extracted dependency.
- Treat `Secrets` as a reserved namespace. Reject parenthesized receivers, aliases, functions, pipelines, control-action dots, named-template arguments, computed keys, and non-canonical field selections.
- Do not implement per-function return-value semantics or a general Go-template data-flow interpreter.
- Parse failures during secret authorization must return an error; do not fall back to regex-only authorization.
- Apply the shared policy to run preparation and to agent claim/fetch authorization without duplicating controller checks.
- Preserve the run snapshot shape, unknown fields, number precision, casing behavior, schedule malformed-spec fallback, and the test-only detached-claim-loop isolation from the earlier CI follow-up.
- Keep the Unity checkout flow valid: `token_secret: gitlab-token` must resolve the template's `index .Secrets .Params.token_secret` to `index .Secrets "gitlab-token"`.
- Keep direct Unity license references such as `.Secrets.unity-license` valid.
- Update `docs/`, `templates/`, and repository examples consistently. Generated schema and field-reference artifacts are out of scope.

---

### Task 1: Replace Data-Flow Tracking with a Fail-Closed AST Validator

**Files:**
- Create: `internal/dsl/secretrefs_ast.go`
- Create: `internal/dsl/secretrefs_ast_test.go`
- Modify: `internal/dsl/secretrefs.go`
- Test: `internal/dsl/secretrefs_test.go`

**Interfaces:**
- Consumes: `normalizeSecretsRefs(string) string` and `funcMap` from `internal/dsl/template.go`.
- Consumes: the existing textual parameter rewrite performed by `ResolveSecretNameParams(string, map[string]string) (string, error)`.
- Produces: `validateSecretReferences(string) error`, called after parameter resolution and before literal-name extraction.
- Produces: `errDynamicSecretName`, whose text is exactly `dynamic secret name must be resolved from a parameter before execution`.
- Preserves: `ReferencedSecretNames(string) ([]string, error)` as the shared claim/fetch authorization entry point.

- [ ] **Step 1: Add a failing policy table for allowed and rejected AST forms**

Create `internal/dsl/secretrefs_ast_test.go` with table-driven tests that exercise both public helpers:

```go
package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSecretReferencePolicyAllowsOnlyStaticForms(t *testing.T) {
	tests := []struct {
		name      string
		tpl       string
		params    map[string]string
		wantNames []string
	}{
		{
			name:      "canonical literal",
			tpl:       `{{ index .Secrets "gitlab-token" }}`,
			wantNames: []string{"gitlab-token"},
		},
		{
			name:      "parameter resolves before validation",
			tpl:       `{{ index .Secrets .Params.token_secret }}`,
			params:    map[string]string{"token_secret": "gitlab-token"},
			wantNames: []string{"gitlab-token"},
		},
		{
			name:   "empty optional parameter",
			tpl:    `{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}`,
			params: map[string]string{"token_secret": ""},
		},
		{
			name:      "direct underscore name",
			tpl:       `{{ .Secrets.API_TOKEN }}`,
			wantNames: []string{"API_TOKEN"},
		},
		{
			name:      "normalized hyphen name",
			tpl:       `{{ .Secrets.unity-license }}`,
			wantNames: []string{"unity-license"},
		},
		{
			name:      "normalized no-dot form",
			tpl:       `{{ secrets.API_TOKEN }}`,
			wantNames: []string{"API_TOKEN"},
		},
		{
			name:      "static secret value nested in a non-secret function",
			tpl:       `{{ printf "%s" (index .Secrets "gitlab-token") }}`,
			wantNames: []string{"gitlab-token"},
		},
		{
			name: "ordinary non-secret index",
			tpl:  `{{ index .Params.values 0 }}`,
		},
		{
			name: "secret-looking text in string and comment",
			tpl:  `{{ printf ".Secrets" }}{{/* index .Secrets .Steps.pick.Outputs.name */}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveSecretNameParams(tt.tpl, tt.params)
			require.NoError(t, err)

			names, err := ReferencedSecretNames(resolved)
			require.NoError(t, err)
			assert.ElementsMatch(t, tt.wantNames, names)
		})
	}
}

func TestSecretReferencePolicyRejectsNonCanonicalSecretsNamespaceUses(t *testing.T) {
	tests := []struct {
		name string
		tpl  string
	}{
		{
			name: "runtime key",
			tpl:  `{{ index .Secrets .Steps.pick.Outputs.name }}`,
		},
		{
			name: "parenthesized receiver",
			tpl:  `{{ index (.Secrets) "gitlab-token" }}`,
		},
		{
			name: "direct alias",
			tpl:  `{{ $secretMap := .Secrets }}{{ index $secretMap "gitlab-token" }}`,
		},
		{
			name: "or alias",
			tpl:  `{{ $secretMap := or .Secrets .Secrets }}{{ index $secretMap .Steps.pick.Outputs.name }}`,
		},
		{
			name: "and argument",
			tpl:  `{{ and .Params.enabled .Secrets }}`,
		},
		{
			name: "function argument",
			tpl:  `{{ printf "%v" .Secrets }}`,
		},
		{
			name: "with dot",
			tpl:  `{{ with .Secrets }}{{ index . "gitlab-token" }}{{ end }}`,
		},
		{
			name: "range source",
			tpl:  `{{ range .Secrets }}{{ . }}{{ end }}`,
		},
		{
			name: "named template argument",
			tpl:  `{{ define "helper" }}{{ index . "gitlab-token" }}{{ end }}{{ template "helper" .Secrets }}`,
		},
		{
			name: "root alias selection",
			tpl:  `{{ $root := . }}{{ index $root.Secrets "gitlab-token" }}`,
		},
		{
			name: "nested reserved selection",
			tpl:  `{{ index .Payload.Secrets "gitlab-token" }}`,
		},
		{
			name: "long direct field chain",
			tpl:  `{{ .Secrets.API_TOKEN.Value }}`,
		},
		{
			name: "computed key",
			tpl:  `{{ index .Secrets (printf "%s-token" .Params.environment) }}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, resolveErr := ResolveSecretNameParams(tt.tpl, nil)
			require.ErrorIs(t, resolveErr, errDynamicSecretName)

			_, extractErr := ReferencedSecretNames(tt.tpl)
			require.ErrorIs(t, extractErr, errDynamicSecretName)
		})
	}
}

func TestSecretReferencePolicyRejectsUnparseableTemplate(t *testing.T) {
	tpl := `{{ index .Secrets "gitlab-token" `

	_, resolveErr := ResolveSecretNameParams(tpl, nil)
	require.ErrorContains(t, resolveErr, "parse secret references")

	_, extractErr := ReferencedSecretNames(tpl)
	require.ErrorContains(t, extractErr, "parse secret references")
}
```

- [ ] **Step 2: Run the new tests and verify the existing analyzer fails**

Run:

```powershell
go test ./internal/dsl -run 'TestSecretReferencePolicy' -count=1
```

Expected: FAIL. At minimum, `or alias` must not return
`errDynamicSecretName`, and the malformed-template test must demonstrate the
old regex fallback.

- [ ] **Step 3: Add the focused AST validator**

Create `internal/dsl/secretrefs_ast.go`. Use the following structure and keep
the visitor independent of parameter rewriting and name collection:

```go
package dsl

import (
	"errors"
	"fmt"
	"text/template"
	"text/template/parse"
)

var errDynamicSecretName = errors.New(
	"dynamic secret name must be resolved from a parameter before execution",
)

func validateSecretReferences(tpl string) error {
	parsed, err := template.New("").
		Funcs(funcMap).
		Option("missingkey=zero").
		Parse(normalizeSecretsRefs(tpl))
	if err != nil {
		return fmt.Errorf("parse secret references: %w", err)
	}
	for _, defined := range parsed.Templates() {
		if defined.Tree == nil || defined.Tree.Root == nil {
			continue
		}
		if err := validateSecretReferenceNode(defined.Tree.Root); err != nil {
			return err
		}
	}
	return nil
}

func validateSecretReferenceNode(node parse.Node) error {
	if node == nil {
		return nil
	}
	switch node := node.(type) {
	case *parse.ListNode:
		for _, child := range node.Nodes {
			if err := validateSecretReferenceNode(child); err != nil {
				return err
			}
		}
	case *parse.ActionNode:
		return validateSecretReferenceNode(node.Pipe)
	case *parse.IfNode:
		return validateSecretReferenceBranch(&node.BranchNode)
	case *parse.WithNode:
		return validateSecretReferenceBranch(&node.BranchNode)
	case *parse.RangeNode:
		return validateSecretReferenceBranch(&node.BranchNode)
	case *parse.TemplateNode:
		return validateSecretReferenceNode(node.Pipe)
	case *parse.PipeNode:
		for _, command := range node.Cmds {
			if err := validateSecretReferenceCommand(command); err != nil {
				return err
			}
		}
	case *parse.CommandNode:
		return validateSecretReferenceCommand(node)
	case *parse.FieldNode:
		if isAllowedDirectSecretValue(node) {
			name := node.Ident[1]
			if err := ValidateSecretName(name); err != nil {
				return fmt.Errorf("secret name %q %w", name, err)
			}
			return nil
		}
		if containsReservedSecrets(node.Ident) {
			return errDynamicSecretName
		}
	case *parse.VariableNode:
		if containsReservedSecrets(node.Ident) {
			return errDynamicSecretName
		}
	case *parse.ChainNode:
		if containsReservedSecrets(node.Field) {
			return errDynamicSecretName
		}
		return validateSecretReferenceNode(node.Node)
	}
	return nil
}

func validateSecretReferenceBranch(branch *parse.BranchNode) error {
	if err := validateSecretReferenceNode(branch.Pipe); err != nil {
		return err
	}
	if err := validateSecretReferenceNode(branch.List); err != nil {
		return err
	}
	return validateSecretReferenceNode(branch.ElseList)
}

func validateSecretReferenceCommand(command *parse.CommandNode) error {
	if isCanonicalSecretIndex(command) {
		return nil
	}
	for _, argument := range command.Args {
		if err := validateSecretReferenceNode(argument); err != nil {
			return err
		}
	}
	return nil
}

func isCanonicalSecretIndex(command *parse.CommandNode) bool {
	if len(command.Args) != 3 {
		return false
	}
	identifier, ok := command.Args[0].(*parse.IdentifierNode)
	if !ok || identifier.Ident != "index" {
		return false
	}
	receiver, ok := command.Args[1].(*parse.FieldNode)
	if !ok || len(receiver.Ident) != 1 || receiver.Ident[0] != "Secrets" {
		return false
	}
	_, ok = command.Args[2].(*parse.StringNode)
	return ok
}

func isAllowedDirectSecretValue(field *parse.FieldNode) bool {
	return len(field.Ident) == 2 && field.Ident[0] == "Secrets"
}

func containsReservedSecrets(fields []string) bool {
	for _, field := range fields {
		if field == "Secrets" {
			return true
		}
	}
	return false
}
```

This validates a direct two-segment field with `ValidateSecretName` before
accepting it and returns the same name-validation error shape already produced
by `ReferencedSecretNames`.

- [ ] **Step 4: Replace the incomplete analyzer in the public helpers**

In `internal/dsl/secretrefs.go`:

1. Remove `text/template` and `text/template/parse` imports.
2. Remove `secretTemplateValue`, `secretTemplateAnalyzer`, all analyzer
   methods, `hasUnsupportedSecretIndex`, `hasSecretPipelineIndex`, and
   `actionHasSecretPipelineIndex`.
3. Keep the quote/comment-aware textual functions used for exact `.Params`
   replacement and literal extraction.
4. Replace the boolean checks with the shared validator:

```go
if err := validateSecretReferences(out); err != nil {
	return out, err
}
```

and:

```go
if err := validateSecretReferences(tpl); err != nil {
	return nil, err
}
```

Use `errDynamicSecretName` instead of constructing duplicate errors in exact
dynamic-operand branches. Preserve the more specific invalid-literal and
invalid-parameter messages.

- [ ] **Step 5: Format and run focused green tests**

Run:

```powershell
gofmt -w internal/dsl/secretrefs.go internal/dsl/secretrefs_ast.go internal/dsl/secretrefs_ast_test.go
go test ./internal/dsl -run 'SecretReference|ResolveSecretNameParams|ReferencedSecretNames' -count=1
go test ./internal/dsl -count=1
git diff --check
```

Expected: all commands exit 0. Confirm the new `or alias` test is green because
the `.Secrets` source is rejected at the `or` command argument, without
tracking the return value.

- [ ] **Step 6: Commit the validator**

```powershell
git add internal/dsl/secretrefs.go internal/dsl/secretrefs_ast.go internal/dsl/secretrefs_ast_test.go internal/dsl/secretrefs_test.go
git commit -m "fix: enforce canonical secret references"
```

---

### Task 2: Prove Authorization and Unity Checkout Compatibility

**Files:**
- Modify: `internal/controller/api_secrets_test.go`
- Modify: `internal/gittemplate/secret_refs_test.go`
- Modify: `docs/jobs.md`
- Modify: `docs/troubleshooting.md`
- Modify: `templates/README.md`
- Test: `internal/controller/api_secrets_test.go`
- Test: `internal/gittemplate/secret_refs_test.go`
- Test: `internal/dsl/templates_parse_test.go`
- Test: `internal/dsl/examples_parse_test.go`

**Interfaces:**
- Consumes: `ReferencedSecretNames(string) ([]string, error)` through both `buildClaimResponse` and `secretNamesForRun`.
- Consumes: `ResolveSecretNameParams` through git template inlining.
- Produces: regression evidence that claim and fetch reject the same non-canonical forms.
- Produces: regression evidence that `token_secret: gitlab-token` becomes `index .Secrets "gitlab-token"`.
- Produces: user documentation that describes the exact allowlist and fail-closed rejection.

- [ ] **Step 1: Extend claim and fetch authorization tests**

Expand the table in
`TestDynamicSecretNameRejectedByClaimAndFetchAuthorization` in
`internal/controller/api_secrets_test.go` with these cases:

```go
{
	name: "built-in mediated alias",
	run:  `echo {{ $secretMap := or .Secrets .Secrets }}{{ index $secretMap .Steps.pick.Outputs.name }}`,
},
{
	name: "root alias selection",
	run:  `echo {{ $root := . }}{{ index $root.Secrets "gitlab-token" }}`,
},
{
	name: "with secret map",
	run:  `echo {{ with .Secrets }}{{ index . "gitlab-token" }}{{ end }}`,
},
```

For every row, keep both existing assertions:

```go
_, claimErr := buildClaimResponse(&store.ClaimedRun{
	Run:  api.Run{ID: runID, JobName: "dynamic-secret-job"},
	Spec: specJSON,
})
require.ErrorContains(t, claimErr, `step "deploy" run`)
assert.ErrorContains(t, claimErr, errDynamicSecretNameText)

_, fetchErr := srv.secretNamesForRun(t.Context(), runID)
require.ErrorContains(t, fetchErr, `step "deploy" run`)
assert.ErrorContains(t, fetchErr, errDynamicSecretNameText)
```

Define the test-local constant as:

```go
const errDynamicSecretNameText = "dynamic secret name must be resolved from a parameter before execution"
```

- [ ] **Step 2: Run the controller authorization test**

Run:

```powershell
go test ./internal/controller -run TestDynamicSecretNameRejectedByClaimAndFetchAuthorization -count=1
```

Expected: PASS for ordinary, parenthesized, direct alias, `or`, root-alias,
and `with` forms in both claim and fetch.

- [ ] **Step 3: Strengthen the checkout template compatibility assertion**

In `internal/gittemplate/secret_refs_test.go`, keep
`TestResolveSpecResolvesCheckoutSecretReference` and add extraction assertions
after unmarshalling the expanded spec:

```go
names, err := dsl.ReferencedSecretNames(expanded.Steps[1].Env["GIT_TOKEN"])
require.NoError(t, err)
assert.Equal(t, []string{"gitlab-token"}, names)
```

The existing assertions must remain:

```go
assert.Contains(t, expanded.Steps[1].Env["GIT_TOKEN"], `index .Secrets "gitlab-token"`)
assert.NotContains(t, expanded.Steps[1].Env["GIT_TOKEN"], "index .Secrets .Steps")
```

- [ ] **Step 4: Run checkout and template parsing tests**

Run:

```powershell
go test ./internal/gittemplate -run TestResolveSpecResolvesCheckoutSecretReference -count=1
go test ./internal/dsl -run 'TestTemplatesParse|TestExamplesParse' -count=1
```

Expected: all commands exit 0. The checkout test must extract only
`gitlab-token`.

- [ ] **Step 5: Update user-facing policy documentation**

In `docs/jobs.md`, next to the static secret-name parameter documentation, add:

```markdown
The secret map itself cannot be aliased, parenthesized, passed to a function,
used as a control-action value, or passed to a named template. Only direct
static references and the exact pre-resolution
`index .Secrets .Params.NAME` form are supported. These restrictions let the
controller authorize the complete secret-name set before an agent claims the
run.
```

In the leading dynamic-secret troubleshooting entry in
`docs/troubleshooting.md`, add:

```markdown
Do not work around the validation by assigning `.Secrets` to a variable or
passing it through `or`, `and`, `with`, `range`, a pipeline, or a named
template. Rewrite the job so the secret name is a literal `with:` value that
feeds the exact `index .Secrets .Params.NAME` form.
```

In `templates/README.md`, extend the indirect-reference guidance with:

```markdown
The exact expression is intentional. Do not alias or transform `.Secrets`;
non-canonical secret-map access is rejected before claim and secret fetch.
```

- [ ] **Step 6: Scan examples and templates for incompatible syntax**

Run:

```powershell
rg -n '\$[A-Za-z_][A-Za-z0-9_]*\s*:?=\s*.*\.Secrets|(?:or|and|with|range|template).*\.Secrets|index\s+\(\s*\.Secrets' docs examples templates README.md
rg -n 'index\s+\.Secrets\s+\.(Steps|Matrix|Foreach)' docs examples templates README.md
```

Expected: no active examples or templates use a rejected form. Historical
design/plan documents may contain rejection examples and do not require
rewriting.

- [ ] **Step 7: Run task-level verification**

Run:

```powershell
gofmt -w internal/controller/api_secrets_test.go internal/gittemplate/secret_refs_test.go
go test ./internal/dsl ./internal/gittemplate ./internal/controller -short -count=1
git diff --check
```

Expected: all commands exit 0.

- [ ] **Step 8: Commit integration coverage and documentation**

```powershell
git add internal/controller/api_secrets_test.go internal/gittemplate/secret_refs_test.go docs/jobs.md docs/troubleshooting.md templates/README.md
git commit -m "test: cover fail-closed secret authorization"
```

---

### Task 3: Verify the Whole Branch and Update PR #99

**Files:**
- Verify only: no tracked file changes expected.
- Inspect: every file changed from the merge base through `HEAD`.

**Interfaces:**
- Consumes: the complete `dynamic-secret-resolution` branch.
- Produces: local verification evidence, an independent whole-branch review, a pushed PR revision, and terminal GitHub Actions results.

- [ ] **Step 1: Run the full short suite from a clean worktree**

Run:

```powershell
go test ./... -short -count=1
```

Expected: exit 0 for every package, including `internal/agent`,
`internal/controller`, `internal/dsl`, `internal/gittemplate`, and `test/e2e`.

- [ ] **Step 2: Re-run the race-sensitive agent regression in Linux with CGO**

Use the existing local Go container image and read-only source/module mounts:

```powershell
docker run --rm `
  -v "${PWD}:/workspace:ro" `
  -v "${env:USERPROFILE}\go\pkg\mod:/go/pkg/mod:ro" `
  -w /workspace `
  golang:1.26 `
  go test ./internal/agent -run TestAgent_RunLoop_PreparePanicIsRecoveredAndFailsRun -race -count=20
```

Expected: exit 0. Do not modify production concurrency settings.

- [ ] **Step 3: Check formatting, diff hygiene, and repository state**

Run:

```powershell
git diff --check
git diff --check main...HEAD
git status --short --branch
git log --oneline --decorate -10
```

Expected: no whitespace errors, no uncommitted tracked changes, and the branch
contains the fail-closed validator and documentation commits.

- [ ] **Step 4: Perform the required independent whole-branch review**

Generate a review package from the merge base through `HEAD`. Review the full
branch for:

- the exact static-reference allowlist;
- no function-semantics or alias bypass;
- parse-error fail-closed behavior;
- create/replay/webhook/schedule/child paths;
- claim/fetch authorization;
- Unity checkout and license compatibility;
- raw run-snapshot preservation;
- documentation consistency; and
- the test-only detached-claim-loop change.

Expected: no open Critical or Important findings. Fix and re-review any
load-bearing finding before pushing.

- [ ] **Step 5: Push the branch and monitor PR #99**

Run:

```powershell
git push origin dynamic-secret-resolution
gh pr checks 99 --repo eirueimi/unified-cd --watch
```

Expected: Integration Linux/PostgreSQL, Kubernetes integration, macOS unit,
Ubuntu unit, and Windows unit checks all reach `pass`.

- [ ] **Step 6: Record the terminal state**

Report:

- final commit SHA;
- local test commands and exit status;
- independent review verdict;
- GitHub Actions run URL and all five job results;
- clean worktree status; and
- that the PR worktree remains available for feedback.
