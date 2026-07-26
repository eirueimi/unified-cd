# CI Follow-up for Resolved Run Snapshots

## Context

PR #99 resolves parameterized secret names before execution. Its first CI run
revealed two independent problems:

1. `prepareRunSpec` marshals `dsl.Spec` with `encoding/json`, but most DSL
   fields define YAML tags rather than JSON tags. Replayed snapshots therefore
   use Go field names such as `Steps` and include zero-value fields instead of
   the canonical DSL shape such as `steps`.
2. `TestAgent_RunLoop_PreparePanicIsRecoveredAndFailsRun` starts the default
   detached claim pool in addition to its intended normal claim loop. The
   concurrent loops race to call the process-wide `prepareWorkspaceFn` stub,
   so the injected first-call panic can be assigned to the wrong run.

## Goals

- Preserve each stored snapshot's decoded JSON shape while resolving only the
  approved executable string fields.
- Keep the existing static secret-name resolution behavior unchanged.
- Make the agent panic-recovery test deterministic without changing production
  agent concurrency.
- Restore all PR CI checks.

## Non-goals

- Add JSON tags to every DSL type.
- Preserve the byte-for-byte formatting or object key ordering of a stored
  snapshot.
- Change detached claim behavior in production.

## Resolved Snapshot Serialization

`prepareRunSpec` will accept the original stored JSON bytes and the resolved
parameter map. Its callers will continue to decode the same bytes into a typed
`dsl.Spec` first for validation, parameter definitions, routing, and capability
inference.

The helper will decode the original bytes into a generic JSON object and run a
path-aware walker over only the executable fields approved by the feature:

- `run` and string values in `env` for entries in `steps`;
- the same fields for entries inside `parallel`;
- the same fields for entries in `finally`.

Each selected string will be passed to the existing
`dsl.ResolveSecretNameParams` function. The walker will find DSL keys
case-insensitively so it supports both authored lowercase JSON and existing
snapshots produced from Go structs, while retaining the exact key spelling
present in the input object. If an expected container has the wrong JSON type,
the helper will return an error instead of silently replacing the snapshot.

The modified generic object will be marshaled back to JSON. Whitespace and
object ordering may be normalized, but decoded structure, key spelling,
field presence, and unknown fields will remain unchanged outside the selected
`run` and `env` string values. This avoids both the Go-field-name regression
and the extra empty fields produced by typed reserialization.

Serialization failures remain controller errors wrapped with
`marshal resolved run spec` context. Secret-name resolution errors retain
their current client-error behavior.

## Deterministic Agent Test

`TestAgent_RunLoop_PreparePanicIsRecoveredAndFailsRun` verifies the normal
claim loop. Its test `Agent` will set `MaxDetachedConcurrent` to `-1`, disabling
the detached pool for that test only. The normal slot remains enabled through
`MaxConcurrent: 1`.

This removes the unrelated concurrent callers of `prepareWorkspaceFn` while
preserving the behavior under test: the first normal claim fails through panic
recovery and the same slot continues to claim and complete the second run.

## Testing

The implementation will follow red-green testing:

1. Add a focused `prepareRunSpec` regression that expects the original
   lowercase DSL keys, unchanged field presence and unknown fields, and the
   resolved literal secret reference. Confirm it fails against the current
   typed `encoding/json` implementation.
2. Add coverage for existing Go-style key spelling and wrong JSON container
   types.
3. Use the existing failed CI executions as the red reproduction for the
   agent test race, then run the corrected test repeatedly with `-race`.
4. Run the replay snapshot integration regression.
5. Run controller, DSL, git-template, and agent package tests.
6. Run the repository's CI-equivalent short and integration suites before
   pushing the fix to PR #99.

## Alternatives Considered

### Re-marshal through the DSL's YAML tags

Marshaling with `gopkg.in/yaml.v3` and converting the result to JSON would
restore lowercase DSL keys, but fields without `omitempty`, such as
`Spec.Params`, would still be added when absent from the stored snapshot. That
would continue to violate replay snapshot fidelity.

### Add JSON tags to every DSL type

This would improve direct `encoding/json` output globally, but it is a broad
schema change spanning many unrelated resource and step types. It is too large
for this CI follow-up.
