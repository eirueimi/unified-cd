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

- Serialize resolved run snapshots with canonical DSL field names and
  `omitempty` behavior.
- Keep the existing static secret-name resolution behavior unchanged.
- Make the agent panic-recovery test deterministic without changing production
  agent concurrency.
- Restore all PR CI checks.

## Non-goals

- Add JSON tags to every DSL type.
- Preserve the byte-for-byte formatting or key ordering of a stored snapshot.
- Rewrite arbitrary JSON syntax in place.
- Change detached claim behavior in production.

## Resolved Snapshot Serialization

`prepareRunSpec` will continue to clone and resolve the typed `dsl.Spec`.
Instead of marshaling that value directly with `encoding/json`, it will:

1. Marshal the resolved value with `gopkg.in/yaml.v3`, which honors the DSL's
   existing YAML tags and omission rules.
2. Convert the YAML representation to JSON with
   `sigs.k8s.io/yaml.YAMLToJSON`.

The stored snapshot may be normalized, but its decoded JSON structure will use
the same canonical field names and omission behavior as the authored DSL.
This is intentionally narrower than adding JSON tags across the complete DSL
type graph.

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

1. Add a focused `prepareRunSpec` regression that expects lowercase canonical
   DSL keys, omitted zero values, and the resolved literal secret reference.
   Confirm it fails against the current `encoding/json` implementation.
2. Use the existing failed CI executions as the red reproduction for the
   agent test race, then run the corrected test repeatedly with `-race`.
3. Run the replay snapshot integration regression.
4. Run controller, DSL, git-template, and agent package tests.
5. Run the repository's CI-equivalent short and integration suites before
   pushing the fix to PR #99.

## Alternatives Considered

### Patch the original JSON syntax tree

This would preserve unknown fields and formatting more closely, but it would
duplicate the typed DSL walker and make authorization-sensitive resolution
harder to reason about.

### Add JSON tags to every DSL type

This would improve direct `encoding/json` output globally, but it is a broad
schema change spanning many unrelated resource and step types. It is too large
for this CI follow-up.
