# Migrating: job parameter `pattern:` now matches the whole value

A job input's `pattern:` used to be an unanchored search: `pattern: "[a-z]+"`
accepted `abc; rm -rf /` because a substring matched. Since params are
interpolated into step shell text, this defeated the injection guard that
`pattern:` (and the webhook `paramsMapping` gate) exists to provide.

The pattern is now applied as `^(?:<pattern>)$`: the **entire** resolved value
must match. The non-capturing group keeps alternation correct, so `a|b` accepts
exactly `a` or `b`, not `ab` or `xa`.

**You are affected only if** a job input declares a `pattern:` without both
`^` and `$` anchors AND relied on it matching part of the value. Patterns that
were already fully anchored (`^...$`, the form the docs recommended) behave
exactly as before.

## What changed

| Before | After |
|---|---|
| `pattern: "[a-z]+"` accepts `abc; rm -rf /` | Rejected: the whole value must match |
| `pattern: "v[0-9]+"` accepts `release-v12-final` | Only `v12`-style values are accepted; write `.*v[0-9]+.*` to keep substring behavior |
| `pattern: "a\|b"` accepts `xa` | Accepts only `a` or `b` |
| `pattern: "^[A-Za-z0-9._/-]+$"` | Unchanged |

## What you will see

Runs (CLI `--param`, `call:`/`uses:` `with:`, schedules, webhook mappings)
whose value only partially matched now fail at run creation with:

```
param "ref" does not match required pattern "v[0-9]+"
```

The message shows your original pattern, never the rejected value. Defaults are
checked the same way, so a job whose `default:` only partially matched its
pattern now fails until fixed.

## How to migrate

1. Find unanchored patterns: `grep -rn "pattern:" <your job YAML>`.
2. For each one, decide what the whole value should look like and write the
   pattern for that. Add `.*` on either side only where you truly want to allow
   arbitrary surrounding text (not recommended for values that reach a shell).
3. Re-apply the jobs with `unified-cd apply -f`.
