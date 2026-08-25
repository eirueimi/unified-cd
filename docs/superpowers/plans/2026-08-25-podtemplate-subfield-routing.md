# Sub-field Granularity in podTemplate Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route a podTemplate carrying `resources.requests` or a non-literal `env` entry to a Kubernetes agent instead of letting it land on a standard agent that silently drops it.

**Architecture:** One predicate, `dsl.PodTemplateNeedsKubernetes`, decides whether a run needs the `pod` capability. It compares container field *names* against `HostSupportedContainerFields`, which cannot express that the host honours `resources.limits` but not `resources.requests`, or a literal `env` value but not `valueFrom`. A helper adds sub-field rules for exactly those two fields; the field-name set is unchanged.

**Tech Stack:** Go, testify.

**Spec:** [`docs/superpowers/specs/2026-08-25-podtemplate-subfield-routing-design.md`](../specs/2026-08-25-podtemplate-subfield-routing-design.md)

## Global Constraints

- `HostSupportedContainerFields` keeps `"env": true` and `"resources": true`. Removing either would push limits-only and literal-env jobs onto Kubernetes agents they do not need.
- The predicate's env test must match the host builder's own test exactly: `internal/agent/claim_pod.go` skips an entry whose `name` is empty **before** looking at its value, and treats a missing `value` **key** as unresolvable while honouring `value: ""` as a literal empty string.
- The host-side warnings in `internal/agent/claim_pod.go` stay. They become unreachable through the normal path and must be commented as defence in depth, so nobody deletes them as dead code.
- This is a breaking change: affected jobs stop scheduling where no `pod`-capable agent exists, reporting `no eligible agent available to claim it`.
- `go build ./...` and `go test ./... -short -count=1` pass at the end of every task.
- Commit messages follow Conventional Commits.

---

## Task 1: Sub-field rules in the routing predicate

**Files:**
- Modify: `internal/dsl/podtemplate.go` (add a helper; call it from `PodTemplateNeedsKubernetes`'s container loop)
- Modify: `internal/dsl/podtemplate_needs_k8s_test.go` (extend the existing table)
- Modify: `internal/agent/claim_pod.go` (comment only, on the two warnings)

**Interfaces:**
- Consumes: nothing.
- Produces: `containerNeedsKubernetes(c map[string]any) bool` in package `dsl`, unexported. No later task calls it directly.

- [ ] **Step 1: Write the failing test cases**

`internal/dsl/podtemplate_needs_k8s_test.go` already has a table-driven `TestPodTemplateNeedsKubernetes` with two local helpers — `container(fields map[string]any) map[string]any` and `tmpl(pt PodTemplate) *PodTemplate`. Append these cases to the existing `cases` slice, using those helpers:

```go
		{
			"resources.requests forces kubernetes (host drops it)",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"requests": map[string]any{"cpu": "500m", "memory": "1Gi"},
					}}),
			}}}),
			true,
		},
		{
			"resources.limits only is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"limits": map[string]any{"cpu": "1", "memory": "2Gi"},
					}}),
			}}}),
			false,
		},
		{
			"resources with both requests and limits forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{
						"limits":   map[string]any{"cpu": "1"},
						"requests": map[string]any{"cpu": "500m"},
					}}),
			}}}),
			true,
		},
		{
			"empty requests map requests nothing and is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"resources": map[string]any{"requests": map[string]any{}}}),
			}}}),
			false,
		},
		{
			"env without a value key forces kubernetes (host cannot resolve valueFrom)",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{
						"name": "TOKEN",
						"valueFrom": map[string]any{"secretKeyRef": map[string]any{
							"name": "api", "key": "token",
						}},
					}}}),
			}}}),
			true,
		},
		{
			"literal env value is host-OK",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{"name": "MODE", "value": "fast"}}}),
			}}}),
			false,
		},
		{
			"empty-string env value is a literal the host honors",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{"name": "MODE", "value": ""}}}),
			}}}),
			false,
		},
		{
			"one valueFrom among literals still forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{
						map[string]any{"name": "MODE", "value": "fast"},
						map[string]any{"name": "TOKEN", "valueFrom": map[string]any{
							"fieldRef": map[string]any{"fieldPath": "metadata.name"},
						}},
					}}),
			}}}),
			true,
		},
		{
			"nameless env entry is skipped, matching the host builder",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim",
					"env": []any{map[string]any{"valueFrom": map[string]any{
						"fieldRef": map[string]any{"fieldPath": "metadata.name"},
					}}}}),
			}}}),
			false,
		},
		{
			"a second container's requests forces kubernetes",
			tmpl(PodTemplate{Spec: map[string]any{"containers": []any{
				container(map[string]any{"name": "job", "image": "python:3.12-slim"}),
				container(map[string]any{"name": "cache", "image": "redis:7",
					"resources": map[string]any{
						"requests": map[string]any{"memory": "256Mi"},
					}}),
			}}}),
			true,
		},
```

The nameless-entry case is the one most likely to be got wrong. `internal/agent/claim_pod.go:64-68` reads the entry's `name`, and `continue`s when it is empty — *before* it looks at the value. Such an entry is dropped by the host without a warning and without a `valueFrom` ever being considered, so the predicate must skip it too. A predicate that returned `true` here would route a job to Kubernetes over an entry that changes nothing on either backend.

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./internal/dsl/ -run TestPodTemplateNeedsKubernetes -count=1`

Expected: FAIL. Five of the new cases (`resources.requests`, both, `valueFrom`, one-among-literals, second container) expect `true` and get `false`, because `resources` and `env` are both in `HostSupportedContainerFields` and nothing looks inside them. The four expecting `false` already pass.

- [ ] **Step 3: Add the sub-field helper**

In `internal/dsl/podtemplate.go`, add below `PodTemplateNeedsKubernetes`:

```go
// containerNeedsKubernetes reports whether a container whose every field NAME is
// host-supported still uses a sub-key the host cannot honor.
//
// HostSupportedContainerFields compares field names. Two of those fields have
// sub-keys that split across the backend boundary:
//
//   - resources: the host maps resources.limits but drops resources.requests,
//     because docker/podman have no request concept.
//   - env: the host honors a literal value but cannot resolve valueFrom, which
//     needs a Kubernetes API to dereference.
//
// Without this check a container carrying only the unsupported half is
// classified host-runnable, routes to a standard agent, and is silently
// degraded there — see the two ignore warnings in
// internal/agent/claim_pod.go, whose remedy ("route to a Kubernetes agent")
// is advice at a point where routing has already been decided.
//
// Any future addition to HostSupportedContainerFields must state whether the
// host honors the whole field or only part of it.
func containerNeedsKubernetes(c map[string]any) bool {
	if res, ok := c["resources"].(map[string]any); ok {
		// An empty requests map requests nothing, so it is not a reason to
		// force kubernetes.
		if reqs, ok := res["requests"].(map[string]any); ok && len(reqs) > 0 {
			return true
		}
	}
	env, _ := c["env"].([]any)
	for _, raw := range env {
		e, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		// Mirror the host builder's order exactly (claim_pod.go): an entry with
		// no name is skipped before its value is ever considered, so it is not
		// a valueFrom the host drops — it is an entry neither backend uses.
		if name, _ := e["name"].(string); name == "" {
			continue
		}
		// A missing "value" KEY is unresolvable on the host; value: "" is a
		// literal empty string it honors. claim_pod.go makes the same
		// distinction with `rawVal, present := e["value"]`.
		if _, present := e["value"]; !present {
			return true
		}
	}
	return false
}
```

Then call it from `PodTemplateNeedsKubernetes`'s existing container loop, immediately after the field-name loop and before the loop's closing brace:

```go
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for field := range c {
			if !HostSupportedContainerFields[field] {
				return true
			}
		}
		if containerNeedsKubernetes(c) {
			return true
		}
	}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./internal/dsl/ -run TestPodTemplateNeedsKubernetes -count=1 -v`

Expected: PASS, including every pre-existing case. If a pre-existing case flipped, the change is too broad — no field outside `resources` and `env` may change classification.

- [ ] **Step 5: Comment the host-side warnings as defence in depth**

The two warnings in `internal/agent/claim_pod.go` — the `resources.requests` one around line 92 and the `env without a literal value` one around line 71 — are now unreachable for any run created after this change, because such a run requires the `pod` capability and a standard agent never advertises it (`internal/agent/agent.go:140-142`).

Add a short comment above each saying so: that routing now keeps these templates away from this agent, that the warning remains as defence in depth for a run created before this change or reaching the host by some other path, and that it should not be deleted as dead code.

Do not change the warning text. Operators may have alerts matching it.

- [ ] **Step 6: Verify the whole build**

Run: `go build ./... && go test ./... -short -count=1`

Expected: PASS. Pay attention to `internal/controller` — `RequiredCaps` feeds run creation, and a controller test asserting the capability for a `resources`- or `env`-bearing spec would change. If one fails, read it before touching it: a test that asserted the old routing was asserting the defect.

- [ ] **Step 7: Commit**

```bash
git add internal/dsl/podtemplate.go internal/dsl/podtemplate_needs_k8s_test.go internal/agent/claim_pod.go
git commit -m "fix(routing): route podTemplate requests and valueFrom env to Kubernetes

The routing predicate compared container field NAMES, so it could not see
that the host maps resources.limits but drops resources.requests, or that
it honors a literal env value but cannot resolve valueFrom. A container
carrying only the unsupported half was classified host-runnable and routed
to a standard agent, which dropped that half with a warning and ran the job
anyway — the author asked for a resource guarantee, or an injected secret,
and silently did not get one.

The field-name set is unchanged: the host really does honor limits and
literal env values, and removing the fields would push jobs onto Kubernetes
agents they do not need."
```

---

## Task 2: Documentation and migration guide

**Files:**
- Modify: `docs/operator-manual/kubernetes-integration.md` (the intentional-differences entries for requests and for host-unsupported fields)
- Create: `docs/operator-manual/migrations/podtemplate-subfield-routing.md`
- Modify: `mkdocs.yml` (nav entry under Operator Manual → Migrations)

**Interfaces:**
- Consumes: the predicate behaviour from Task 1.
- Produces: nothing later depends on.

- [ ] **Step 1: Rewrite the intentional-differences entries**

`docs/operator-manual/kubernetes-integration.md` lists the deliberate host/Kubernetes differences under "The remaining intentional differences are:". Two entries describe the old behaviour:

- The `resources` entry says requests are "applied only here" and that the standard agent "logs one WARN when `resources.requests` is present".
- The `container:` entry says other host-unsupported podTemplate fields "are ignored with a WARN rather than applied".

Both are now routing statements, not degradation statements. Rewrite them to say that a podTemplate declaring `resources.requests`, or an `env` entry without a literal value, requires a Kubernetes agent — the run is pinned there rather than degraded on a host agent.

Keep the explanation of *why* the host cannot honour them; that part is still true and is the reason the routing exists. Do not delete the sentence about docker/podman having no request concept.

- [ ] **Step 2: Write the migration guide**

Create `docs/operator-manual/migrations/podtemplate-subfield-routing.md`. Read `docs/operator-manual/migrations/agent-id-scoped-credentials.md` first and match its structure — it is the house style for a migration guide: what changed, a before/after table, exact commands, exact error strings.

Content it must carry:

1. **What changed.** A podTemplate declaring `resources.requests`, or an `env` entry using `valueFrom` instead of a literal `value`, now requires a Kubernetes agent. Previously such a job ran on a standard agent with that part of the template dropped.

2. **Who is affected**, and how to find out before upgrading. The symptom appears only after, so give the search:

```bash
grep -rn -A5 "podTemplate:" <your job definitions> | grep -E "requests:|valueFrom:"
```

A job already pinned to a Kubernetes agent by `agentSelector` is unaffected.

3. **The symptom.** A run that used to schedule stays Pending and reports:

```
no eligible agent available to claim it
```

Cross-link the existing troubleshooting entry for that message in `docs/troubleshooting/runs-and-scheduling.md`.

4. **The two ways out**, and only two: run a Kubernetes agent, or remove the part the host cannot honour — `resources.limits` instead of `requests`, a literal `value` instead of `valueFrom`. State plainly that removing it means the job never had that guarantee in the first place; it was being dropped silently.

- [ ] **Step 3: Add the guide to the nav**

In `mkdocs.yml`, add the new page under `Operator Manual` → `Migrations`, beside the existing entry.

- [ ] **Step 4: Verify the site builds**

Run: `python -m mkdocs build --strict`

Expected: PASS with zero warnings. A page missing from `nav`, or a broken cross-link to the troubleshooting entry, fails here.

- [ ] **Step 5: Commit**

```bash
git add docs/ mkdocs.yml
git commit -m "docs(routing): document the podTemplate sub-field routing change

The intentional-differences entries described degradation on the host;
they are routing statements now. Adds a migration guide with the search
that finds affected jobs before upgrading, since the symptom only appears
after."
```

---

## Final acceptance

Against spec section 6:

- [ ] The predicate is sub-field aware for both fields, with every case in Task 1 Step 1 passing.
- [ ] No pre-existing `PodTemplateNeedsKubernetes` case changed classification.
- [ ] `go build ./...` and `go test ./... -short -count=1` pass.
- [ ] `docs/operator-manual/kubernetes-integration.md` no longer describes requests or non-literal env as ignored-with-a-WARN on the host.
- [ ] The migration guide exists, is in the nav, and `mkdocs build --strict` is clean.
