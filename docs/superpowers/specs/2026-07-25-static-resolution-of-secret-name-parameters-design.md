# Static Resolution of Secret Name Parameters

## Problem

Job templates currently document and use indirect secret references such as:

```gotemplate
{{ index .Secrets .Params.token_secret }}
```

During `uses:` expansion, template parameter references are rewritten to synthetic
input-step outputs:

```gotemplate
{{ index .Secrets .Steps.checkout__inputs.Outputs.token_secret }}
```

The controller determines `SecretsNeeded` before the agent starts the run. Its
collector recognizes direct references such as `.Secrets.gitlab-token`, but it
cannot infer the secret name from the rewritten runtime expression. The secret
is therefore not fetched, the expression expands to an empty value, and the
checkout runs without authentication.

Fetching an arbitrary secret name after a normal step produces an output would
weaken the existing authorization boundary: an agent may fetch only names that
the controller can prove the run declared before execution.

## Scope

Support indirect secret references only when their names are known before step
execution:

- A normal Job may select a secret with a resolved run parameter:
  `index .Secrets .Params.NAME`.
- A JobTemplate may select a secret with an input whose value is fixed by a
  literal `uses.with` value or a literal template default.
- A direct literal reference remains supported:
  `index .Secrets "NAME"`, `.Secrets.NAME`, or `secrets.NAME`.

Reject secret-name expressions that depend on ordinary step outputs, matrix
values, foreach values, or any other runtime-only data.

This change does not add `uses.secrets`, change secret storage, or allow agents
to request undeclared secret names during a run.

## Resolution Model

Introduce a focused template transformation that recognizes secret index
expressions. It accepts the resolved parameter map available at the current
resolution boundary and performs these operations:

1. Leave `index .Secrets "literal-name"` unchanged.
2. Replace `index .Secrets .Params.NAME` with
   `index .Secrets "resolved-value"`.
3. Fail if `NAME` is absent, empty, not a valid secret name, or still contains
   a template expression.
4. Fail if the operand to `index .Secrets` is any other expression.

The replacement must use a correctly quoted Go-template string literal. Secret
values are never read or written by this transformation; it handles secret
names only.

Resolution runs at two boundaries:

- When a normal Job run is created, after run parameters and defaults have been
  resolved and before its spec snapshot is stored.
- When a JobTemplate is expanded for `uses:`, after template inputs have been
  assembled from defaults and `with`, but before `.Params` references are
  rewritten to synthetic input-step outputs.

Consequently, every supported indirect reference becomes a literal reference
in the fully resolved run spec. The agent continues to receive all required
secret values once, before executing the DAG.

## Secret Collection

Extend the controller's secret-name collector to recognize literal index
notation:

```gotemplate
{{ index .Secrets "gitlab-token" }}
```

The claim response and the secret-fetch authorization path must continue to use
the same collector so that `SecretsNeeded` and the server-side allowlist cannot
diverge.

Collection must never evaluate an arbitrary template expression. If a dynamic
secret index reaches claim construction, treat it as an invalid resolved spec
and fail the run with an actionable error instead of executing with an empty
secret.

## Compatibility

The documented `token_secret` inputs in existing templates remain valid, so
callers do not need to change their YAML:

```yaml
uses:
  job: git://example.com/org/repo/templates/git-checkout.yaml@main
  with:
    token_secret: gitlab-token
```

Templates using parameter-based indirect references begin working as
documented. Templates that attempt to choose a secret from a normal step output
will fail early; that behavior was not safely supported before this change.

## Error Handling

Errors must identify the unsupported or invalid reference without exposing a
secret value. Representative messages are:

```text
secret name parameter "token_secret" resolved to an empty value
dynamic secret name must be resolved from a parameter before execution
secret name parameter "token_secret" must be a literal secret name
```

Template-resolution errors fail deterministically in the existing Git template
resolver. Invalid normal Job references are rejected before an agent executes
the run.

## Testing

Add focused tests for:

- Literal `index .Secrets "NAME"` collection.
- A normal Job parameter rewritten to a literal secret name.
- A JobTemplate input supplied by `uses.with` rewritten before input-step
  reference rewriting.
- A JobTemplate default used when `with` omits the input.
- Empty, missing, malformed, and templated secret-name parameter values.
- Rejection of ordinary `.Steps`, `.Matrix`, and `.Foreach` operands.
- Claim and secret-fetch allowlists containing the resolved secret name.
- End-to-end `git-checkout` expansion producing `gitlab-token` in
  `SecretsNeeded`.
- Existing direct secret syntax and masking behavior remaining unchanged.

Run the complete short Go test suite and the template/example parsing tests
after the focused tests pass.
