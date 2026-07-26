# Fail-Closed Secret Reference Validation Design

## Status

Approved for implementation on 2026-07-26.

## Context

Static secret-name resolution intentionally permits a JobTemplate to select a
secret name from a literal parameter:

```gotemplate
{{ index .Secrets .Params.token_secret }}
```

Before a run starts, the controller resolves that expression to a literal
reference:

```gotemplate
{{ index .Secrets "gitlab-token" }}
```

The controller can then authorize exactly `gitlab-token` during claim and
secret fetch. Runtime-dependent names such as `.Steps`, `.Matrix`, and
`.Foreach` must be rejected before execution.

The first implementation recognized canonical text forms and later added
data-flow tracking for parenthesized and aliased `.Secrets` receivers.
Data-flow tracking is not a safe boundary for this policy. A valid template
expression such as the following can pass the secret map through a built-in
whose return semantics are not modeled:

```gotemplate
{{ $secretMap := or .Secrets .Secrets }}
{{ index $secretMap .Steps.pick.Outputs.name }}
```

Adding per-function return semantics would create an open-ended allowlist that
must be updated whenever template functions change. Missing one function would
reopen the authorization bypass.

## Decision

Use a fail-closed, AST-based context allowlist. The validator proves that every
secret-map use has canonical syntax. It does not try to prove that arbitrary
expressions eventually evaluate to the secret map.

The only permitted index form after static parameter resolution is:

```gotemplate
{{ index .Secrets "literal-secret-name" }}
```

The existing direct static forms remain supported:

```gotemplate
{{ .Secrets.API_TOKEN }}
{{ secrets.API_TOKEN }}
```

`secrets.API_TOKEN` and hyphenated dot forms such as
`.Secrets.unity-license` are normalized to the canonical index form.
Go-template-compatible `.Secrets.API_TOKEN` remains a direct two-segment field
node and is allowed as a static secret-value reference.

The pre-resolution form remains supported:

```gotemplate
{{ index .Secrets .Params.token_secret }}
```

`ResolveSecretNameParams` must replace the parameter operand with a quoted
literal before the fail-closed validator runs. Empty optional parameters become
an empty literal and add no secret dependency, as before.

All other uses of the reserved `Secrets` namespace are rejected, including
parentheses, aliases, function arguments, pipelines, control-action dots,
named-template arguments, and computed index operands. Examples:

```gotemplate
{{ index (.Secrets) "gitlab-token" }}
{{ $secretMap := .Secrets }}
{{ $secretMap := or .Secrets .Secrets }}
{{ with .Secrets }}{{ index . "gitlab-token" }}{{ end }}
{{ template "helper" .Secrets }}
{{ index .Secrets (printf "%s-token" .Params.environment) }}
```

The validator must use the existing error text:

```text
dynamic secret name must be resolved from a parameter before execution
```

## Validator Architecture

### Normalization and resolution order

For run creation and template inlining:

1. Resolve exact `index .Secrets .Params.NAME` operands from the already
   validated literal parameter map.
2. Normalize `secrets.NAME` and hyphenated `.Secrets.NAME` references to
   canonical literal `index` calls. Preserve Go-template-compatible
   `.Secrets.NAME` field references.
3. Parse the normalized template with the same function map used by runtime
   expansion.
4. Validate every parsed template tree, including named definitions.
5. Collect literal secret names only after validation succeeds.

For claim and fetch authorization, the persisted run snapshot already contains
resolved parameter names. The controller performs steps 2 through 5 again. This
defense in depth covers old snapshots and every run-creation path.

### Reserved namespace rule

`Secrets` is a reserved template namespace. An AST node that selects a field
named `Secrets` is never treated as an ordinary user-data field.

After normalization, the validator permits:

- the exact `.Secrets` field node only as the second argument of an `index`
  command that has exactly one string-literal key; and
- an exact two-segment `.Secrets.NAME` field node as a direct static
  secret-value reference.

The validator rejects the `.Secrets` map node in every other context and
rejects longer field chains such as `.Secrets.NAME.More`.

Selections through another receiver, such as `$root.Secrets` or
`.Payload.Secrets`, are rejected rather than analyzed. This conservative rule
prevents root aliases and future context shapes from creating alternate access
paths.

The validator does not need to propagate a "secret map" value through `or`,
`and`, custom functions, assignments, or pipelines. It rejects the source
`.Secrets` use at the point where it is supplied to the unsupported context.
This removes the open-ended function-semantics problem.

### Non-secret expressions

The validator does not restrict `index`, variables, functions, pipelines, or
control actions that do not access the reserved `Secrets` namespace. Existing
expressions over `.Params`, `.Steps`, `.Matrix`, `.Foreach`, and user data keep
their current behavior.

Template comments and string literals that contain text resembling a secret
reference do not create AST secret-reference nodes and must not be rejected.

### Parse failures

AST parsing is part of authorization validation. A parse failure must return an
error rather than fall back to an incomplete textual detector. The same invalid
template cannot execute successfully, and rejecting it before claim/fetch
avoids authorizing secrets from a template the validator could not understand.

## Unity Checkout Compatibility

The Unity build jobs pass a literal secret name to the checkout template:

```yaml
with:
  token_secret: gitlab-token
```

The checkout template uses:

```gotemplate
{{ if .Params.token_secret }}{{ index .Secrets .Params.token_secret }}{{ end }}
```

Template inlining resolves the index operand to:

```gotemplate
{{ index .Secrets "gitlab-token" }}
```

That canonical form is explicitly allowed. The condition may still reference
the template input, but it does not select or expose a secret name. Direct Unity
license references such as `.Secrets.unity-license` are also normalized to a
canonical literal index and remain allowed.

## Integration Points

The shared DSL validation must continue to protect:

- direct API run creation;
- child-run creation;
- replay;
- webhook-triggered runs;
- scheduled runs;
- agent claim construction; and
- secret fetch authorization.

These paths already converge on `ResolveSecretNameParams` during preparation
and `ReferencedSecretNames` during claim/fetch. The change belongs in the shared
DSL helpers, not in duplicated controller call-site checks.

## Testing

### Allowed forms

- canonical literal index;
- `.Params` index resolved to a literal;
- empty optional `.Params` index;
- `.Secrets.NAME` and `secrets.NAME`;
- Unity checkout `token_secret: gitlab-token`;
- ordinary non-secret indexing and pipelines.

### Rejected forms

- runtime key operands from `.Steps`, `.Matrix`, and `.Foreach`;
- parenthesized secret-map receivers;
- direct and chained aliases;
- `or`, `and`, and custom-function arguments containing `.Secrets`;
- assignment and reassignment;
- `if`, `with`, and `range` pipelines containing `.Secrets`;
- named-template calls that pass `.Secrets`;
- `$root.Secrets` and other non-canonical `Secrets` selections;
- computed keys, including parenthesized expressions;
- malformed templates that cannot be parsed for authorization.

### End-to-end authorization

Controller tests must prove that representative disguised forms fail in both
agent claim construction and secret fetch authorization. Existing template
inlining tests must continue to prove that the Unity checkout pattern resolves
to a literal secret name.

All DSL, controller, template, and repository short tests must pass. The final
branch must pass the complete GitHub Actions matrix before merge.

## Documentation

Update user-facing secret-reference documentation and template guidance to say
that only direct static references and the exact
`index .Secrets .Params.NAME` pre-resolution form are supported. Explain that
aliases and function-mediated secret-map access are rejected intentionally.

No schema, example resource kind, or generated artifact changes are required.
Existing examples and templates must be scanned to confirm they use allowed
forms.

## Out of Scope

- A general-purpose Go-template data-flow interpreter.
- Per-function return-value semantics.
- Supporting aliases of the secret map.
- Supporting computed secret names beyond literal `.Params` resolution.
- Changing secret storage, encryption, masking, or agent transport.
