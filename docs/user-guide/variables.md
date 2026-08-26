# Variables

> **Variables are plain text.** They are stored in the clear, returned by the
> API in the clear, and printed in step logs like any other environment
> variable. A value that must not appear in a log is a secret, not a variable —
> see [Secrets](secrets.md).

A variable lets you say a value once and use it everywhere: a registry host, a
language version, an application name. There are two layers, a global one and a
per-job one, and a variable reaches a step by two routes at the same time — as
an environment variable, and as `{{ .Vars.KEY }}` in a template.

---

## The two manifests

### Global: `kind: Vars`

A `Vars` manifest is applied and synced like any other resource, so its values
live in Git:

```yaml
apiVersion: unified-cd/v1
kind: Vars
metadata:
  name: org-defaults
spec:
  vars:
    REGISTRY: ghcr.io/myorg
    GO_VERSION: "1.24"
```

```bash
unified-cli apply -f org-defaults.yaml
# vars applied: org-defaults (2 keys)
```

Its keys are added to **every** job's steps, in every run claimed after the
apply. You may have more than one `Vars` manifest; the effective set is their
union (see [Collisions](#cross-manifest-collisions) for what happens when two
of them define the same key).

Values are literals. `REGISTRY: {{ .Params.x }}` is **not** resolved — the
value is stored and delivered exactly as written.

### Per-job: `spec.vars`

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  vars:
    APP_NAME: myapp
  steps:
    - name: build
      run: docker build -t {{ .Vars.REGISTRY }}/$APP_NAME .
```

That example is deliberate: `{{ .Vars.REGISTRY }}` (from the global manifest)
and `$APP_NAME` (from the job) are the two different routes, used side by side
in one command.

`spec.vars` is a field on a `Job`. A `JobTemplate` has no `vars:` field —
steps inlined by `uses:` run inside the calling job's run and therefore see the
calling job's variables.

---

## Precedence

Nearest scope wins:

```
step env:   >   job spec.vars:   >   global Vars
```

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  vars:
    REGISTRY: ghcr.io/myteam     # overrides the global org-defaults value
  steps:
    - name: publish
      env:
        REGISTRY: localhost:5000 # overrides both, for this step only
      run: echo $REGISTRY        # localhost:5000
```

A job overriding a global key is **normal and silent**. That is what "nearest
wins" means, and warning about it would only train operators to ignore
warnings.

Under the hood this is not merge logic but ordering: the agent lays the run's
variables into the step environment first and lets the step's own `env:`
entries follow, so the later duplicate wins.

---

## The two routes

Both routes carry the same merged value. Which one reads better depends on
where you are:

| Route | Looks like | Reads better when |
|---|---|---|
| Environment variable | `$REGISTRY`, `%REGISTRY%` | The value is used **inside the shell script** — passed to a command, tested, interpolated into a longer string. The script stays a normal script, and it still works if you paste it into a terminal with the same variables exported. |
| Template | `{{ .Vars.REGISTRY }}` | The value is needed in a **YAML field that is not a shell script** — a `cache:` key or path, a `call:` step's `with:`, an `outputs:` expression — or where you want the value fixed before the shell ever sees it (no quoting or word-splitting surprises). |

Where `{{ .Vars.KEY }}` is expanded: a step's `run:`, its `env:` values, its
`outputs:` expressions, a `cache:` step's key and path, and a `call:` step's
`with:` values. These are all expanded by the agent, per step, against the
run's variables.

An **undefined** key in a template expands to the empty string, not an error
(templates are evaluated with `missingkey=zero`). A misspelt
`{{ .Vars.REGISTY }}` therefore produces an empty string rather than a
complaint.

---

## Where variables do **not** reach

These are the places a variable will not be there when you reach for it. None
of them fails loudly, so read this section before you write one.

### `post:` hooks receive no variables

A step's `post:` hook does not get the run's variables. This is not a
vars-shaped hole — a post hook gets **no context at all**: no
`UNIFIED_WORKSPACE`, no `UNIFIED_AGENT_OS`, and its `post.env:` values are not
template-expanded either. Its environment is exactly the literal `post.env:`
map you wrote and nothing else.

```yaml
- name: build
  run: make build
  post:
    run: echo $REGISTRY          # empty — variables do not reach a post hook
    env:
      R: "{{ .Vars.REGISTRY }}"  # literal text; post.env is not expanded
```

If a post hook needs a value, pass it as a literal in `post.env:`, or do the
work in the step body or a `finally:` step instead.

### `agentSelector:` and `concurrency:` cannot use variables

```yaml
spec:
  agentSelector:
    - "pool:{{ .Vars.POOL }}"      # expands to "pool:" — empty, silently
  concurrency:
    mutex: "deploy-{{ .Vars.ENV }}" # expands to "deploy-" — empty, silently
```

Both fields are expanded by the **controller when the run is created**, and
that expansion is given the run's `params` only. Variables are merged later, at
claim time, so they do not exist yet. `{{ .Params.NAME }}` works in both fields
and is the supported way to make either dynamic.

There is no error and no warning: the reference simply expands to the empty
string, and you get a selector or a lock name with a hole in it.

---

## Variables in `if:`

`if:` is [CEL](https://github.com/google/cel-go), not a Go template, so the
spelling there is lowercase `vars.KEY` — no `{{ }}`, no leading dot:

```yaml
- name: deploy
  if: vars.ENVIRONMENT == "production"
  run: ./deploy.sh
```

**An undefined key reads as the empty string.** `vars.EVN == "production"` is
therefore `false`, and the step is **skipped**. This is deliberate: an
evaluation error in an `if:` fails *open* (the step runs), so a typo that
raised an error would run a production-gated step on every trigger with the
author's intent inverted. Reading empty keeps the gate shut.

It is not silent. Every undefined key that a condition actually consulted is
written into the **run's own log** as a `System` line naming the key:

```
unified-cd: step "deploy": if: expression "vars.EVN == \"production\"" referenced
undefined vars.EVN — an undefined key reads as the empty string, so the condition
was evaluated as though it were "" (check the spelling, or define it in a
kind: Vars manifest or the job's spec.vars)
```

If a step you expected to run was skipped, that line is where to look.

### Testing presence

Use `"NAME" in vars`. Do **not** use `has(vars.NAME)`:

```yaml
if: '"DEPLOY_TARGET" in vars'   # correct — answers truthfully
if: has(vars.DEPLOY_TARGET)     # WRONG — always true
```

`has()` and a plain value read go through the same lookup inside cel-go, and
that lookup is what returns the empty string for an undefined key. It therefore
always succeeds, and `has()` is always true. The `in` operator goes through a
different path and still answers truthfully.

### `params` behaves differently, on purpose

| Reference | Undefined key | Step |
|---|---|---|
| `vars.MISSING` | reads as `""` | **skips** (the gate stays shut) |
| `params.MISSING` | raises `no such key` | **runs** (evaluation errors fail open) |

This asymmetry is deliberate, and the reason is that unified-cd genuinely
supports parameters a job never declares. `resolveParams` passes undeclared
parameters through unchanged, and five paths can introduce one: `--param` on
`unified-cli run trigger`, a re-trigger of an earlier run, a webhook's
`paramsMapping`, a schedule's `params:`, and a `call:` step's `with:`. On top of
that, `spec.concurrency.orLocks` synthesises `{NAME}_LOCK_VALUE` keys. So
`if: params.DEPLOY_TARGET == "x"` against a `--param DEPLOY_TARGET=x` is
documented, supported usage — and statically indistinguishable from a typo.
Making `params` behave like `vars` would silently change the meaning of every
existing `params`-gated condition; refusing undeclared references at apply time
would break supported usage. Neither was worth it.

The decision is recorded in the code at
`TestEvalCondition_ParamsUndefinedKeyStillFailsOpen`, alongside `resolveParams`'
own doc comment, so you can check the reasoning rather than take it on trust.

The practical rule: in an `if:`, reference only parameters your job declares
under `spec.params.inputs`.

---

## Validation

All of it happens at apply time, where the author is present to read the error.
`unified-cli apply --dry-run -f vars.yaml` runs everything except the two
checks that need the server (collisions, and the managed-resource guard).

### Key syntax

A key must match `[A-Za-z_][A-Za-z0-9_]*` — the environment-variable name rule.
The values become environment variables, so a key that is not a valid variable
name has nothing useful to fall back on. Every offending key in one manifest is
reported at once, so a large manifest does not have to be re-applied once per
mistake.

The same rule applies to a job's `spec.vars`.

### Reserved names

Two sets are refused, **case-insensitively** (`path`, `Path` and `PATH` are all
the same name):

| Name | Why |
|---|---|
| `UNIFIED_CACHE_KEY`, `UNIFIED_CACHE_SECRET`, `UNIFIED_TOKEN`, `UNIFIED_AGENT_CREDENTIAL_FILE`, `UNIFIED_AGENT_ENROLLMENT_TOKEN_FILE` | The agent's own credentials. A job author who could set these could act as the agent — and, via the cache credentials, write directly to the shared object store, bypassing every controller-side check. |
| `PATH`, `HOME` | A global `Vars` manifest applies to *every step of every job*. A `PATH` that shadows the agent's baseline breaks all of them at once, in a way whose cause is not visible in the failure. The cost of refusing is that nobody can set `PATH` globally, which nobody should want to do; the cost of allowing it is a footgun with the widest possible blast radius. |

The agent enforces the same set again at run time, so a run created before this
validation existed cannot inject one either: a variable with a reserved name is
dropped from the step environment rather than delivered.

### Cross-manifest collisions

Two **different** `Vars` manifests defining the same key is an error naming the
key and both manifests:

```
vars REGISTRY defined by both "org-defaults" and "team-defaults"
```

The alternative — last writer wins — makes the effective value depend on apply
order, which is a debugging problem disguised as a feature.

This is a *global-versus-global* rule only. A job's `spec.vars` overriding a
global key is the [precedence rule](#precedence) and is fine. Re-applying the
same manifest under the same name is an update, not a collision.

See [Two ways the collision check can be
missed](#two-ways-the-collision-check-can-be-missed) below — it is not an
absolute guarantee.

### Manifest shape

`apiVersion: unified-cd/v1` and `kind: Vars` are both required and both
checked. `metadata.name` follows the usual DNS-1123 rule (lowercase
alphanumerics, `-` and `.`, starting and ending alphanumeric). Unknown fields
are rejected rather than ignored, so a typo'd key in the manifest itself fails
the apply instead of quietly doing nothing.

---

## Applying, and manifests managed by an AppSource

A `Vars` manifest can be applied two ways:

- **Interactively**, with `unified-cli apply -f`. This path runs the
  cross-manifest collision check.
- **By an AppSource**, which syncs it from Git along with everything else in
  the repository.

A `Vars` manifest owned by an AppSource is **rejected by interactive apply with
a `409 Conflict`**, exactly like a managed Job or Schedule — the error names the
AppSource and its repository. Change it in Git instead, or set the AppSource's
`syncPolicy.allowManualOverride` if you genuinely want both paths writing to it.
The same guard applies to deleting it.

---

## Deleting a manifest

Delete a global manifest by removing its file from the AppSource-tracked
repository, or with the API directly:

```bash
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://controller:8080/api/v1/vars/org-defaults
```

There is no `unified-cli vars` command; `apply` is the only CLI verb that
knows about `kind: Vars` today.

What happens next follows from **when the merge happens**: variables are merged
once, by the controller, as it builds a claim. So:

- Runs **claimed after** the delete are built without those keys.
- A run **already in flight** is unaffected — its variables travelled with its
  claim and do not change underneath it.
- The same is true of an edit: changing a value affects the next claim, not a
  running job.

In a job that still references a deleted key, nothing raises: the environment
variable is simply not set, `{{ .Vars.KEY }}` expands to the empty string, and
`vars.KEY` in an `if:` reads as empty (and logs the `System` line described
[above](#variables-in-if)).

Deleting a manifest that does not exist is not an error.

---

## Operational notes

### Two ways the collision check can be missed

The check that two manifests cannot define the same key is **loud but not
absolute**. An operator should not assume a collision is always stopped at
apply.

1. **It is not atomic.** Reading the existing manifests and writing the new one
   are two separate queries. Two administrators applying colliding manifests at
   the same moment can both pass the check and both persist.
2. **The AppSource sync path does not run it.** A Git sync applies many
   documents in one pass; rejecting the second of two colliding manifests would
   leave the sync half-applied, with an error naming a manifest the operator may
   never have touched, and the next reconcile would hit it again. So the check
   lives on the interactive apply path, where an author is present to read the
   error and fix it.

Neither case corrupts anything. Variables are merged in sorted manifest-name
order, so the winning value is deterministic and stable — it does not depend on
apply order or on which row the database returned first. What is lost is the
early, loud catch. If two globals disagree about a key, the manifest whose name
sorts last wins, quietly.

### When variables cannot be loaded at claim time

Every claim reads every `Vars` manifest, so a failure here is a fleet-wide
concern rather than a single job's. The two kinds are handled differently on
purpose:

- **A transient failure** — the database call itself erroring — puts the run
  back on the queue. It is claimed again shortly, losing nothing: no step has
  started, no log line exists, and its concurrency slot was taken earlier and is
  untouched.
- **A stored manifest that cannot be decoded** fails the claiming run
  **visibly**, with the manifest named in that run's own log:

  ```
  vars manifest "org-defaults" is stored corrupt and cannot be decoded
  (fix or delete it — every claim reads it): ...
  ```

  This one is deterministic — every later claim reads the same bytes and fails
  the same way — so requeueing it would put *every* claim in the fleet into a
  silent retry loop in which no run ever finishes and nothing looks broken. One
  run fails loudly instead, the operator gets the manifest's name, and the rest
  of the fleet keeps working.

The corrupt manifest is never skipped. Running jobs without the variables they
expect, silently, is the failure this feature exists to avoid.

---

## Variable or secret?

| | Variable | Secret |
|---|---|---|
| Storage | Plain text | AES-256-GCM encrypted |
| Readable back | Yes, via the API | No — names and metadata only |
| In step logs | Printed like any other env var | Masked automatically |
| Reference | `$KEY`, `{{ .Vars.KEY }}`, `vars.KEY` | `{{ secrets.NAME }}` |
| Scope | Global manifest, or one job | Global |

If you would mind the value appearing in a log, it is a
[secret](secrets.md).
