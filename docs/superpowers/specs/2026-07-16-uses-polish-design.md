# `uses:` Polish — `if:` Propagation, Template `finally:`, Apply-Time Validation, Name-Shape Validation — Design

**Status:** Approved 2026-07-16 (design decisions locked via Q&A; part of the 4-branch hardening program A→B→C→D).

**Goal:** Close the remaining `uses:` silent-drop and validation gaps: an `if:` on a `uses:` step now gates the whole expansion; a JobTemplate may declare `finally:` (spliced into the caller's finally); plain jobs get container-reference validation at apply time; podTemplate container/volume names get DNS-1123 shape validation (also killing case/whitespace reserved-name evasion).

## 1. `if:` on a `uses:` step propagates to the expansion (bug fix)

**Problem (verified):** `expandUsesStep` (`internal/gittemplate/inline.go:160`) never reads the outer step's `If`; `resolveSteps` (`resolve.go:205`) doesn't pass it. A `uses:` step with `if: failure()` expands into steps that run unconditionally. Silent drop.

**Design:**
- Thread the outer `If` string into `expandUsesStep` (new parameter, exported wrapper updated).
- Apply it to **every** produced step: the synthetic `__inputs` step, each renamed template step (concrete and parallel sub-steps), and the output-capture step — so the whole expansion is gated as one unit.
- Combine with an inner step's own `if:` via CEL `&&`: `combineIf(outer, inner)` returns `""` if both empty, the non-empty one if only one set, else `(outer) && (inner)` (parenthesized; both operands are already independently-valid CEL). The inner operand is the **already-`rewriteRefs`-rewritten** inner if (references template-local steps/params); the outer operand references caller context and is **not** rewritten.
- **Status-function semantics (`condition.go:113` trap):** the evaluator ANDs `success()` into any main-DAG expression containing no status function. Combining preserves this correctly: if the outer if is `failure()`, the combined text contains a status token, so the expansion runs on failure as the author intends (overriding implicit skip). Document this in the `uses:` docs: the outer `if:` semantics match a plain step's `if:`.
- Scope mode: same propagation (scope-tagged steps also carry the combined if; the orchestrator evaluates if: before scope provisioning, so a false condition skips scope creation naturally — verify in tests).

## 2. JobTemplate `finally:` splice (feature)

**Problem:** a template's cleanup steps had nowhere to live — `finally:` was first silently dropped (pre-#54), then schema-rejected (#54).

**Design:**
- `JobTemplateSpec` gains `Finally []StepEntry` (`internal/dsl/jobtemplate_types.go`); `JobTemplate.Validate` validates it with the same helpers (`validateStepEntries(finally, "spec.finally", nameSet, false, false)` — shared nameSet with steps, mirroring `Job.Validate`); `ToSpec` carries it.
- `expandUsesStep` returns the template's finally steps as a separate list (extend the return: `([]dsl.StepEntry, []dsl.StepEntry /*finally*/, podContribution, error)` or fold the finally list into a widened contribution struct — implementer's choice, keep it internal). Finally steps get the same `usesName__` prefix, the same `rewriteRefs` rewriting, the same shell stamping, and the same combined `if:` from §1 **except**: a template finally step's `if:` semantics follow finally rules (implicitSuccess=false at runtime — no extra handling needed; the combined `(outerIf) && (innerIf)` text is still correct).
- Bubbling: `resolveSteps` accumulates template-finally steps alongside the pod contribution (nested `uses:` templates' finally steps bubble too); `ResolveSpec` **appends** them to the caller's `spec.Finally` (after the caller's own finally steps, in uses-step declaration order). This works uniformly whether the `uses:` step sits in the caller's `Steps` or `Finally` list.
- Ordering contract (documented): template finally steps run in the caller's finally phase, after the caller's own declared finally steps.
- **Scope mode rejects a template `finally:`** (`runsIn.image` on the uses step + template declares finally → resolution error): a scope pod is torn down after the template body; a scope-tagged finally step's environment lifetime is undefined. Consistent with the scope-mode podTemplate rejection.
- Name collisions: `checkGlobalNameCollisions` (`resolve.go:305`) already walks Steps+Finally and is the backstop; expanded finally names are prefix-namespaced like everything else.
- Step indices: `plannedSteps`/`buildStages` share one index counter across Steps then Finally, so spliced steps get correct indices automatically (splice happens before `UpdateRunSpec`).

## 3. Apply-time container-reference validation for plain jobs

**Problem:** a `uses:`-less job with `container: typo` passes apply and fails only at exec (run-creation validation lives in the git-resolution sweeper, which only sees `uses:` jobs).

**Design:** in `dsl.Job.Validate` (used by controller apply, CLI `apply --dry-run`, and the AppSource reconciler's parse path), call `ValidateContainerReferences(j.Spec)` **only when no step in `Spec.Steps`/`Spec.Finally` (including `parallel:` sub-steps) has `Uses != nil`**. A uses-bearing spec defers to the existing post-resolve sweeper check (a caller's `container:` may be satisfied by a template's pod-shape merge). Behavior change: previously-registrable jobs with dangling refs now fail apply with the existing clear message — they were broken at runtime anyway.

Named agent-side podTemplates (`podTemplate.name`) ALSO defer — at apply time AND at resolution time (the controller sweeper skips `ValidateContainerReferences` for them too). Their containers live in agent config, invisible to the controller, so container references are validated only at pod build time on the agent. The gate helper is `specDefersContainerValidation` (`internal/dsl/parse.go`).

## 4. podTemplate container/volume name-shape validation + reserved-name normalization

**Problem:** container/volume names in `podTemplate.spec` are completely unvalidated until pod build (k8s API error), and `IsReservedContainerName`/`IsReservedVolumeName` are exact-match — `" job "`/`"JOB"` evade the reserved-name injection guard (then fail later as invalid k8s names, but opaquely).

**Design:**
- Add `dsl.ValidateDNS1123Label(name) error` (lowercase alphanumeric + `-`, start/end alphanumeric, ≤63 chars — k8s container/volume name rules; distinct from the existing `ValidateName` subdomain rule).
- `Job.Validate` and `JobTemplate.Validate` validate every `podTemplate` container and volume name (via the `PodTemplateContainers`/`PodTemplateVolumes` accessors; skip empty names — other validation paths own "name required" semantics where applicable, but a nameless container def is also rejected with a clear message).
- `IsReservedContainerName`/`IsReservedVolumeName` additionally normalize (`strings.TrimSpace` + `strings.ToLower`) before comparing — defense in depth for any path that skips shape validation.
- Behavior change: previously-accepted malformed names (would have failed at pod build) now fail at parse/apply with the field named.

## Components / files

- `internal/gittemplate/inline.go` — `If` threading, `combineIf`, finally expansion; `resolve.go` — finally bubbling + splice, scope-mode finally rejection.
- `internal/dsl/jobtemplate_types.go` / `jobtemplate.go` — `Finally` field, validation, `ToSpec`.
- `internal/dsl/parse.go` (`Job.Validate`) — gated `ValidateContainerReferences` + name-shape validation.
- `internal/dsl/container.go` / `name.go` — `ValidateDNS1123Label`, normalized reserved checks.
- Docs: `docs/jobs.md` (uses if: semantics, template finally contract + ordering), `docs/resources.md`/generated schema+field-reference (JobTemplate gains finally — **regenerate via go generate**, schemagen picks up the `*_types.go` change automatically), migration/troubleshooting notes for the two behavior changes (§3, §4).

## Testing

- if: propagation: outer-only, inner-only, both (AND), empty-empty; synthetic steps gated; `failure()` outer overrides implicit skip (orchestrator-level or condition-level test); scope-mode expansion carries it.
- finally splice: template finally lands appended+prefixed in caller Finally; refs rewritten; nested-uses finally bubbles; collision with existing finally name errors; scope-mode template finally errors; uses-in-finally with template-finally works.
- apply-time validation: plain job with dangling ref fails `Job.Validate`; uses-bearing job with the same ref passes Validate (deferred); resolved-spec sweeper still catches it.
- name shape: `"JOB"`, `" job "`, `"My_Container"`, 64-char names rejected at Job+JobTemplate validate; normalized reserved check rejects `" job "` injection in a template even if shape validation were bypassed.
- Full-suite gate: `go test ./...` (templates/examples corpus + schema regen diffs committed).

## Out of scope
- Evaluating the outer `if:` once-per-expansion at runtime (a gate step) — per-step propagation is semantically equivalent for CEL's pure expressions and needs no engine changes.
- `parallel:`-level uses (already parse-rejected).
