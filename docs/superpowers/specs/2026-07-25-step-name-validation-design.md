# Step-name validation: reject non-identifier names (breaking)

Date: 2026-07-25
Status: Proposed (design)

## Problem

Step names are referenced in Go templates via dot-notation, e.g.
`{{ .Steps.<name>.ChildRunID }}` and `{{ .Steps.<name>.Outputs.x }}`. Go
template dot-notation can only address a field whose name is a valid Go
identifier. A hyphenated step name (`build-app`) parses as subtraction and
cannot be referenced with dot-notation at all; the only workaround is
`{{ index .Steps "build-app" }}`. This is a silent footgun: the job applies
fine, then a template reference fails confusingly at run time (or is authored
with the awkward `index` form).

## Decision

Reject invalid step names at **apply/parse time** (`dsl.Validate`), so the
mistake surfaces immediately with a clear error instead of at run time. This is
a **breaking change** (the user has accepted it): jobs with step names that are
not valid Go identifiers will fail validation on their next apply.

### The rule

A non-empty step name must match:

```
^[A-Za-z_][A-Za-z0-9_]*$
```

This is exactly the pattern already enforced for the two other DSL names that
are surfaced into Go templates — `orLockNameRe` and `matrixDimNameRe`
(`internal/dsl/parse.go`). Reusing it keeps the DSL internally consistent and
fixes the whole class of dot-notation-breaking names (hyphens, leading digits,
dots, spaces), not just hyphens.

### Scope

- Applies to every named step: top-level steps, steps inside `parallel:`
  blocks, and `finally:` steps — they all flow through `validateStepFull`.
- Only **non-empty** names are checked. Top-level entries already require a
  name (`name is required`); `parallel:` children may be anonymous, and an
  anonymous step is unreferenceable anyway, so empty names are left untouched to
  keep the change minimal.
- Job names, orLock names, matrix dimension names, and artifact names are
  already validated and are out of scope.

## Design

In `validateStepFull` (`internal/dsl/parse.go`), after the existing duplicate
check, add:

```go
if name != "" && !stepNameRe.MatchString(name) {
    return fmt.Errorf("%s: step name %q must match %s", path, name, stepNameRe.String())
}
```

Add `stepNameRe` next to `orLockNameRe`/`matrixDimNameRe` (or reuse a shared
`identifierNameRe` variable if consolidating the three reads cleanly). The error
message names the offending step and the required pattern, and — to be helpful —
points to underscores or `index .Steps "name"` as the fix.

## Testing

- Valid names pass: `build_app`, `deploy`, `_hidden`, `step0`.
- Invalid names fail with a clear error: `build-app` (hyphen), `0step` (leading
  digit), `my.step` (dot), `my step` (space).
- Invalid names are rejected in a `parallel:` block and in `finally:`, not only
  top-level.
- Empty parallel-child names still validate (no new rejection).
- A table-driven test mirroring the existing `parse_test.go` style.

## Documentation

- `docs/jobs.md`: state that step names must be valid identifiers
  (`^[A-Za-z_][A-Za-z0-9_]*$`) so they are referenceable via
  `{{ .Steps.<name> }}`; drop/downgrade the previous hyphen workaround note
  (hyphens are now rejected outright).
- `docs/field-reference.md`: note the constraint on the step `name` field if the
  generated reference has a place for it.

## Migration note

Because this is breaking, the PR description must call out that existing jobs
with hyphenated (or otherwise non-identifier) step names will fail `apply` and
must be renamed (underscores) — a one-time rename with a clear error pointing
the way.
