# Agent Capability Routing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route each run to an agent that can actually execute it — inferring a `native`/`container`/`pod` capability requirement from the job spec and matching it against capabilities the agents advertise — and warn in the WebUI when no registered agent can run a job.

**Architecture:** Agents advertise a typed `capabilities []string` at registration. At trigger time the controller infers `requiredCaps` from the spec and stores it on the run. `ClaimNextRun` matches `agent.capabilities ⊇ run.required_caps` (ANDed with the existing agentSelector label match; legacy null-caps agents skip the cap check). A schedulability helper + endpoint feeds a JobDetail warning banner when nothing can run a job. Reuses `8ca1567`'s `dsl.PodTemplateNeedsKubernetes` for the pod case.

**Tech Stack:** Go, PostgreSQL (pgx), Svelte, testify.

**Spec:** [docs/superpowers/specs/2026-07-09-agent-capability-routing-design.md](../specs/2026-07-09-agent-capability-routing-design.md)

## Global Constraints

- Base branch: current `origin/main` (already has `dsl.PodTemplateNeedsKubernetes` + `HostSupportedContainerFields` from PR #2 / `8ca1567` — REUSE, do not reimplement).
- Capability vocabulary is exactly three strings: `native`, `container`, `pod`. Reject any other value at registration.
- Prose in English (AGENTS.md). Commits from a worktree, not the main working tree (AGENTS.md).
- Store tests need Docker (`store.NewTestPostgres`); pure Go tests do not.
- Migration files are numbered sequentially; the next number is `009` (highest existing is `008_run_indexes`).
- Run `go build ./...` and the touched package's tests after each task; `go test ./internal/...` before the docs task.
- Legacy safety: an agent row with NULL `capabilities` must still claim label-matched runs (rolling-upgrade). A run with NULL/empty `required_caps` must match any agent.

---

### Task 1: `dsl.RequiredCaps` — infer capability requirement from a spec

**Files:**
- Create: `internal/dsl/capabilities.go`
- Test: `internal/dsl/capabilities_test.go`

**Interfaces:**
- Consumes: existing `dsl.Spec`, `dsl.PodTemplateNeedsKubernetes(pt *PodTemplate) bool` (already on main).
- Produces: `func RequiredCaps(spec Spec) []string` — returns exactly one of `["native"]`, `["container"]`, `["pod"]`. Also exports `const (CapNative = "native"; CapContainer = "container"; CapPod = "pod")` and `func ValidCapability(s string) bool`.

- [ ] **Step 1: Write the failing test**

```go
// internal/dsl/capabilities_test.go
package dsl

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRequiredCaps(t *testing.T) {
	assert.Equal(t, []string{"native"}, RequiredCaps(Spec{Native: true}))
	assert.Equal(t, []string{"container"}, RequiredCaps(Spec{}))

	// host-runnable podTemplate (plain name/image) -> container
	hostPT := &PodTemplate{Spec: map[string]any{"containers": []any{
		map[string]any{"name": "mysql", "image": "mysql:8"},
	}}}
	assert.Equal(t, []string{"container"}, RequiredCaps(Spec{PodTemplate: hostPT}))

	// k8s-only podTemplate (named agent template) -> pod
	k8sPT := &PodTemplate{Name: "golang", Spec: map[string]any{"containers": []any{
		map[string]any{"name": "job", "image": "golang:1.22"},
	}}}
	assert.Equal(t, []string{"pod"}, RequiredCaps(Spec{PodTemplate: k8sPT}))

	// native takes precedence even if a podTemplate is somehow present
	assert.Equal(t, []string{"native"}, RequiredCaps(Spec{Native: true, PodTemplate: hostPT}))
}

func TestValidCapability(t *testing.T) {
	assert.True(t, ValidCapability("native"))
	assert.True(t, ValidCapability("container"))
	assert.True(t, ValidCapability("pod"))
	assert.False(t, ValidCapability("gpu"))
	assert.False(t, ValidCapability(""))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dsl/ -run 'TestRequiredCaps|TestValidCapability' -v`
Expected: FAIL (`undefined: RequiredCaps`)

- [ ] **Step 3: Implement `internal/dsl/capabilities.go`**

```go
package dsl

// Agent capability vocabulary. An agent advertises the subset it can do; the
// controller infers a run's required capability from its spec (RequiredCaps)
// and only an agent whose capabilities are a superset may claim the run.
const (
	CapNative    = "native"    // run a step as a host process (standard agent)
	CapContainer = "container" // run a step in an isolated container (docker/podman/k8s)
	CapPod       = "pod"       // build a Kubernetes Pod (k8s agent only)
)

// ValidCapability reports whether s is a known capability string.
func ValidCapability(s string) bool {
	return s == CapNative || s == CapContainer || s == CapPod
}

// RequiredCaps infers the single capability a run of spec needs from an agent:
//   - native: true                 -> native (host process)
//   - no podTemplate (isolated)    -> container
//   - podTemplate host can't honor -> pod   (PodTemplateNeedsKubernetes, from 8ca1567)
//   - podTemplate host CAN honor   -> container
// native takes precedence: a native job never runs in a container/pod.
func RequiredCaps(spec Spec) []string {
	switch {
	case spec.Native:
		return []string{CapNative}
	case spec.PodTemplate != nil && PodTemplateNeedsKubernetes(spec.PodTemplate):
		return []string{CapPod}
	default:
		return []string{CapContainer}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dsl/ -run 'TestRequiredCaps|TestValidCapability' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/dsl/capabilities.go internal/dsl/capabilities_test.go
git commit -m "feat(dsl): RequiredCaps infers native/container/pod from a job spec"
```

---

### Task 2: store + api — persist capabilities and required_caps, match on claim

**Files:**
- Create: `internal/store/migrations/009_agent_capabilities.up.sql`, `009_agent_capabilities.down.sql`
- Modify: `internal/api/types.go` (AgentRegisterRequest ~line 69, AgentInfo ~line 309)
- Modify: `internal/store/store.go` (CreateRun/ClaimNextRun/UpsertAgent/UpsertAgentOnClaim interface signatures)
- Modify: `internal/store/postgres.go` (CreateRun ~137, ClaimNextRun ~541, UpsertAgent ~867, UpsertAgentOnClaim ~899, ListAgents/GetAgent scans)
- Test: `internal/store/postgres_capabilities_test.go` (create)

**Interfaces:**
- Consumes: `dsl.RequiredCaps` (Task 1), `dsl.ValidCapability`.
- Produces:
  - `api.AgentRegisterRequest.Capabilities []string`, `api.AgentInfo.Capabilities []string`.
  - `CreateRun(ctx, jobName, params, spec, agentSelector, requiredCaps []string, triggeredBy)` — new `requiredCaps` param before `triggeredBy`.
  - `UpsertAgent(ctx, agentID, hostname, os, version string, labels, capabilities []string, env)` — new `capabilities` param after `labels`.
  - `UpsertAgentOnClaim(...)` unchanged signature (claim path does NOT carry capabilities; it must not clobber them).
  - `ClaimNextRun` signature unchanged (reads the agent's caps from the agents table via a join).

- [ ] **Step 1: Write the migration**

```sql
-- internal/store/migrations/009_agent_capabilities.up.sql
ALTER TABLE agents ADD COLUMN capabilities text[];          -- NULL = legacy agent (skip cap check)
ALTER TABLE runs   ADD COLUMN required_caps text[] DEFAULT '{}'::text[] NOT NULL;
```

```sql
-- internal/store/migrations/009_agent_capabilities.down.sql
ALTER TABLE runs   DROP COLUMN required_caps;
ALTER TABLE agents DROP COLUMN capabilities;
```

- [ ] **Step 2: Write the failing store test**

```go
// internal/store/postgres_capabilities_test.go
package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimNextRun_CapabilityMatch(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()

	// a native-cap agent and a pod-cap agent, no label constraints
	require.NoError(t, pg.UpsertAgent(ctx, "host-1", "h1", "linux", "", []string{}, []string{"native", "container"}, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "k8s-1", "k1", "linux/k8s", "", []string{}, []string{"pod", "container"}, nil))

	// a run that needs native must be claimable only by host-1
	nativeRun, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{}, []string{"native"}, "test")
	require.NoError(t, err)

	// k8s-1 cannot claim it
	got, err := pg.ClaimNextRun(ctx, "k8s-1", []string{})
	require.NoError(t, err)
	assert.Nil(t, got, "k8s agent (no native cap) must not claim a native run")

	// host-1 can
	got, err = pg.ClaimNextRun(ctx, "host-1", []string{})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, nativeRun.ID, got.ID)
}

func TestClaimNextRun_LegacyAgentSkipsCapCheck(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	// legacy agent: capabilities NULL (passed as nil)
	require.NoError(t, pg.UpsertAgent(ctx, "legacy-1", "l1", "linux", "", []string{}, nil, nil))
	_, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), []string{}, []string{"native"}, "test")
	require.NoError(t, err)
	got, err := pg.ClaimNextRun(ctx, "legacy-1", []string{})
	require.NoError(t, err)
	require.NotNil(t, got, "a legacy (null-caps) agent must still claim by labels only")
}

func TestUpsertAgentOnClaim_PreservesCapabilities(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	pg := NewTestPostgres(t)
	ctx := context.Background()
	require.NoError(t, pg.UpsertAgent(ctx, "a", "h", "linux", "", []string{"kind:docker"}, []string{"native", "container"}, nil))
	// claim-path upsert (no caps) must NOT wipe the registered caps
	require.NoError(t, pg.UpsertAgentOnClaim(ctx, "a", "h", "linux", "", []string{"kind:docker"}, nil))
	info, err := pg.GetAgent(ctx, "a")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"native", "container"}, info.Capabilities)
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/store/ -run 'TestClaimNextRun_Capability|TestClaimNextRun_Legacy|TestUpsertAgentOnClaim_Preserves' -v`
Expected: FAIL (compile: too few args to UpsertAgent/CreateRun; column does not exist)

- [ ] **Step 4: Implement**

`internal/api/types.go`: add `Capabilities []string \`json:"capabilities,omitempty"\`` to `AgentRegisterRequest` and to `AgentInfo`.

`internal/store/store.go`: update the four interface signatures to match the Produces block above.

`internal/store/postgres.go`:
- `CreateRun`: add the `requiredCaps []string` param; include `required_caps` in the INSERT column list and values (pass `requiredCaps` — nil becomes `{}` via a guard `if requiredCaps == nil { requiredCaps = []string{} }`).
- `UpsertAgent`: add `capabilities []string` param; include `capabilities` in the INSERT/UPDATE (store as-is; nil stays SQL NULL — do NOT coerce to `{}`, so legacy detection works). On conflict, `SET capabilities = EXCLUDED.capabilities`.
- `UpsertAgentOnClaim`: do NOT touch the `capabilities` column in its UPDATE/INSERT (so a claim never overwrites registered caps); if it INSERTs a brand-new row, leave capabilities NULL.
- `ClaimNextRun`: change the picked CTE to also require the agent's caps to cover the run. Read the agent row inside the query:

```sql
WITH me AS (SELECT capabilities AS caps FROM agents WHERE id = $1),
picked AS (
    SELECT r.id FROM runs r, me
    WHERE r.status = 'Queued'
      AND (r.agent_selector = '{}' OR r.agent_selector <@ $2::TEXT[])
      AND (me.caps IS NULL OR r.required_caps <@ me.caps)
    ORDER BY r.created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
UPDATE runs r SET claimed_by = $1, claimed_at = NOW(), updated_at = NOW(), status = 'Running'
FROM picked WHERE r.id = picked.id
RETURNING r.id, r.job_name, r.status, r.params, r.spec, r.created_at, r.updated_at;
```

(`r.required_caps <@ me.caps` = "run's caps are contained in agent's caps" = agent is a superset. A missing agent row makes `me` empty; guard by treating no-row as legacy — but a claiming agent always has a row from the claim-path upsert, which runs before ClaimNextRun in the handler.)
- `ListAgents`/`GetAgent`: add `capabilities` to the SELECT column list and scan into `info.Capabilities` (`&info.Capabilities` — pgx scans a NULL text[] into a nil slice).

Fix every other caller of the changed signatures (grep `CreateRun(` / `UpsertAgent(` across the repo — controller handlers, tests, fakes) to pass the new args (`nil` for requiredCaps/capabilities where a caller has none yet; Task 4 fills the real controller values).

- [ ] **Step 5: Run to verify it passes**

Run: `go build ./... && go test ./internal/store/ -run 'Capabilit|ClaimNextRun|UpsertAgent' -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/store internal/api/types.go
git commit -m "feat(store): agent capabilities + run required_caps with superset claim match"
```

---

### Task 3: agents advertise capabilities at registration

**Files:**
- Modify: `internal/agent/agent.go` (register request build ~line 112; the agent already knows its runtime via `RuntimePref`/`containerRuntime()`)
- Modify: `internal/k8sagent/agent.go` (register ~line 68)
- Test: `internal/agent/agent_capabilities_test.go` (create), `internal/k8sagent/agent_capabilities_test.go` (create)

**Interfaces:**
- Consumes: `dsl.CapNative/CapContainer/CapPod`.
- Produces: the standard agent's register request `Capabilities` = `["native"]` plus `"container"` when a container runtime is available; the k8s agent's = `["pod","container"]`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/agent/agent_capabilities_test.go
package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAgentCapabilities_NativeAlways(t *testing.T) {
	// runtimeAvailable == false -> only native
	assert.Equal(t, []string{"native"}, agentCapabilities(false))
	// runtimeAvailable == true -> native + container
	assert.ElementsMatch(t, []string{"native", "container"}, agentCapabilities(true))
}
```

```go
// internal/k8sagent/agent_capabilities_test.go
package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestK8sAgentCapabilities(t *testing.T) {
	assert.ElementsMatch(t, []string{"pod", "container"}, k8sAgentCapabilities())
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/agent/ -run TestAgentCapabilities -v; go test ./internal/k8sagent/ -run TestK8sAgentCapabilities -v`
Expected: FAIL (undefined helpers)

- [ ] **Step 3: Implement**

`internal/agent/agent.go` — add a helper and use it when building the register request:

```go
// agentCapabilities reports what this standard agent can execute: always
// native (host process), plus container when a container runtime is present.
func agentCapabilities(runtimeAvailable bool) []string {
	caps := []string{dsl.CapNative}
	if runtimeAvailable {
		caps = append(caps, dsl.CapContainer)
	}
	return caps
}
```

At the register-request build site (~line 112), detect runtime availability once (reuse the existing detection — `_, err := a.containerRuntime(); runtimeAvailable := err == nil`, matching how `hostBackend` probes it) and set `Capabilities: agentCapabilities(runtimeAvailable)` on the `api.AgentRegisterRequest`. Import `internal/dsl` if not already.

`internal/k8sagent/agent.go` — add:

```go
func k8sAgentCapabilities() []string { return []string{dsl.CapPod, dsl.CapContainer} }
```

and set `Capabilities: k8sAgentCapabilities()` on the k8s register request (~line 68).

- [ ] **Step 4: Run to verify they pass**

Run: `go build ./... && go test ./internal/agent/ -run TestAgentCapabilities -v && go test ./internal/k8sagent/ -run TestK8sAgentCapabilities -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent internal/k8sagent
git commit -m "feat(agent): standard + k8s agents advertise capabilities at registration"
```

---

### Task 4: controller — infer/persist requiredCaps, validate caps, drop the k8s-label pin

**Files:**
- Modify: `internal/controller/api_runs.go` (handleTriggerRun ~line 37-66: replace the `PodTemplateNeedsKubernetes → appendLabelIfMissing("kubernetes")` block; pass requiredCaps to CreateRun)
- Modify: the call-step child-run creation path (find where a `call:` child run is created — the same `CreateRun` used by the trigger path or a child-specific one; grep `CreateRun(`), so a child also gets requiredCaps.
- Modify: `internal/controller/api_agent.go` (handleAgentRegister ~line 22: validate + thread capabilities into UpsertAgent)
- Test: `internal/controller/api_runs_capabilities_test.go` (create), `internal/controller/api_agent_capabilities_test.go` (create)

**Interfaces:**
- Consumes: `dsl.RequiredCaps`, `dsl.ValidCapability`, the Task 2 store signatures.
- Produces: a run created via trigger or call carries `dsl.RequiredCaps(spec)`; registration rejects unknown capability strings (400) and persists valid ones.

- [ ] **Step 1: Write the failing tests**

```go
// internal/controller/api_runs_capabilities_test.go — handler-level, real PG
func TestTriggerRun_PersistsRequiredCaps(t *testing.T) {
	// Apply a native job, trigger it, assert the created run row's required_caps == ["native"].
	// Apply a plain (isolated) job, trigger, assert ["container"].
	// Apply a host-runnable podTemplate job, trigger, assert ["container"] AND the
	// agentSelector was NOT given a "kubernetes" label (the old pin is gone).
	// Apply a k8s-only podTemplate (Name set), trigger, assert ["pod"].
	// (Use the same newTestServer + apply/trigger harness as the existing api_runs_test.go /
	//  api_runs_podtemplate_routing_test.go; read required_caps via a store.GetRun-style helper
	//  or a direct query in the test.)
}
```

```go
// internal/controller/api_agent_capabilities_test.go
func TestRegister_RejectsUnknownCapability(t *testing.T) {
	// POST /register with Capabilities ["native","gpu"] -> 400.
}
func TestRegister_PersistsCapabilities(t *testing.T) {
	// POST /register with ["native","container"] -> 200; GetAgent shows them.
}
```

Model these on `internal/controller/api_runs_podtemplate_routing_test.go` and `api_agent_test.go` for the harness.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/controller/ -run 'TestTriggerRun_PersistsRequiredCaps|TestRegister_' -v`
Expected: FAIL

- [ ] **Step 3: Implement**

`internal/controller/api_runs.go` — in `handleTriggerRun`, DELETE:

```go
	if dsl.PodTemplateNeedsKubernetes(spec.PodTemplate) {
		agentSelector = appendLabelIfMissing(agentSelector, "kubernetes")
	}
```

and instead compute `requiredCaps := dsl.RequiredCaps(spec)` and pass it to `CreateRun(..., agentSelector, requiredCaps, triggeredBy)`. (`RequiredCaps` internally uses `PodTemplateNeedsKubernetes`, so the k8s case now yields `["pod"]` instead of the label.) If `appendLabelIfMissing` has no other caller after this, delete it too (grep first).

Call-step child path: at the child `CreateRun` site, compute `dsl.RequiredCaps(childSpec)` from the child job's spec (unmarshal the child job's stored spec the same way the trigger path does) and pass it in, so a called job is also routed by capability.

`internal/controller/api_agent.go` — in `handleAgentRegister`, after decoding: validate every entry of `req.Capabilities` with `dsl.ValidCapability`, returning `http.Error(w, "unknown capability: "+c, 400)` on the first invalid one; then pass `req.Capabilities` (nil stays nil) into the extended `UpsertAgent(..., labels, req.Capabilities, req.Env)`.

- [ ] **Step 4: Run to verify they pass**

Run: `go build ./... && go test ./internal/controller/ -run 'TestTriggerRun_PersistsRequiredCaps|TestRegister_' -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/controller
git commit -m "feat(controller): route by inferred capability; validate agent caps; drop the k8s-label pin"
```

---

### Task 5: `EvaluateSchedulability` + `GET /api/v1/jobs/{name}/schedulability`

**Files:**
- Create: `internal/controller/schedulability.go`
- Modify: `internal/controller/server.go` (route registration ~line 274 area) + a handler
- Test: `internal/controller/schedulability_test.go` (create)

**Interfaces:**
- Consumes: `dsl.RequiredCaps`, `api.AgentInfo` (with `.Capabilities`, `.Labels`), `dsl.ExpandAgentSelector` awareness (skip label check when a selector entry contains `{{`).
- Produces:
  - `type Schedulability struct { RequiredCaps []string; Selector []string; Satisfiable bool; Reason string; SelectorDependsOnParams bool }`
  - `func EvaluateSchedulability(spec dsl.Spec, agents []api.AgentInfo) Schedulability`
  - handler `GET /api/v1/jobs/{name}/schedulability` → the struct as JSON.

- [ ] **Step 1: Write the failing test**

```go
// internal/controller/schedulability_test.go
package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

func agent(caps, labels []string) api.AgentInfo { return api.AgentInfo{Capabilities: caps, Labels: labels} }

func TestEvaluateSchedulability(t *testing.T) {
	host := agent([]string{"native", "container"}, []string{"kind:docker", "hostname:h1"})
	k8s := agent([]string{"pod", "container"}, []string{"kind:k8s", "kubernetes"})

	// native job, only a k8s agent online -> unsatisfiable by cap
	s := EvaluateSchedulability(dsl.Spec{Native: true}, []api.AgentInfo{k8s})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "native")

	// native job, host agent online -> satisfiable
	s = EvaluateSchedulability(dsl.Spec{Native: true}, []api.AgentInfo{host, k8s})
	assert.True(t, s.Satisfiable)

	// label no agent has -> unsatisfiable by label
	s = EvaluateSchedulability(dsl.Spec{Native: true, AgentSelector: []string{"kind:macos"}}, []api.AgentInfo{host})
	assert.False(t, s.Satisfiable)
	assert.Contains(t, s.Reason, "kind:macos")

	// param-templated selector -> don't warn; flag it
	s = EvaluateSchedulability(dsl.Spec{Native: true, AgentSelector: []string{"hostname:{{ .Params.agent }}"}}, []api.AgentInfo{host})
	assert.True(t, s.SelectorDependsOnParams)
	assert.True(t, s.Satisfiable) // cap part is satisfiable; label part deferred
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/controller/ -run TestEvaluateSchedulability -v`
Expected: FAIL (undefined)

- [ ] **Step 3: Implement `internal/controller/schedulability.go`**

```go
package controller

import (
	"fmt"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

type Schedulability struct {
	RequiredCaps            []string `json:"requiredCaps"`
	Selector                []string `json:"selector"`
	Satisfiable             bool     `json:"satisfiable"`
	Reason                  string   `json:"reason,omitempty"`
	SelectorDependsOnParams bool     `json:"selectorDependsOnParams,omitempty"`
}

// EvaluateSchedulability reports whether at least one agent can run a job with
// this spec: an agent whose capabilities cover RequiredCaps (a legacy null-caps
// agent counts, matching the claim rule) AND whose labels cover the job's
// agentSelector. Selector entries containing "{{" resolve only at trigger time,
// so the label part is skipped and SelectorDependsOnParams is set.
func EvaluateSchedulability(spec dsl.Spec, agents []api.AgentInfo) Schedulability {
	req := dsl.RequiredCaps(spec)
	sel := spec.AgentSelector
	dependsOnParams := false
	var staticSel []string
	for _, s := range sel {
		if strings.Contains(s, "{{") {
			dependsOnParams = true
			continue
		}
		staticSel = append(staticSel, s)
	}

	for _, a := range agents {
		if !capsCover(a.Capabilities, req) {
			continue
		}
		if !labelsCover(a.Labels, staticSel) {
			continue
		}
		return Schedulability{RequiredCaps: req, Selector: sel, Satisfiable: true, SelectorDependsOnParams: dependsOnParams}
	}

	reason := reasonNoAgent(agents, req, staticSel)
	return Schedulability{RequiredCaps: req, Selector: sel, Satisfiable: false, Reason: reason, SelectorDependsOnParams: dependsOnParams}
}

// capsCover: a nil agent-cap set is legacy and covers anything (matches the
// claim SQL's `me.caps IS NULL` branch).
func capsCover(agentCaps, required []string) bool {
	if agentCaps == nil {
		return true
	}
	set := map[string]bool{}
	for _, c := range agentCaps {
		set[c] = true
	}
	for _, r := range required {
		if !set[r] {
			return false
		}
	}
	return true
}

func labelsCover(agentLabels, selector []string) bool {
	set := map[string]bool{}
	for _, l := range agentLabels {
		set[l] = true
	}
	for _, s := range selector {
		if !set[s] {
			return false
		}
	}
	return true
}

func reasonNoAgent(agents []api.AgentInfo, req, sel []string) string {
	// Distinguish "no agent has the capability" from "no agent matches the labels".
	capOK := false
	for _, a := range agents {
		if capsCover(a.Capabilities, req) {
			capOK = true
			break
		}
	}
	if !capOK {
		return fmt.Sprintf("no registered agent provides capability %v", req)
	}
	return fmt.Sprintf("no registered agent matches labels %v", sel)
}
```

Handler + route in `internal/controller/server.go` (near the other `/api/v1/jobs` routes) — a `GET /api/v1/jobs/{name}/schedulability` that loads the job spec (unmarshal like handleTriggerRun), calls `s.store.ListAgents`, runs `EvaluateSchedulability`, and `writeJSON`s the result. Register it under the same auth middleware as the other read routes.

- [ ] **Step 4: Run to verify it passes**

Run: `go build ./... && go test ./internal/controller/ -run TestEvaluateSchedulability -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/controller
git commit -m "feat(controller): schedulability evaluation + /jobs/{name}/schedulability endpoint"
```

---

### Task 6: WebUI — JobDetail unschedulable warning banner

**Files:**
- Modify: `web/src/routes/JobDetail.svelte`
- Test: `web/src/routes/JobDetail.test.js` (create, or add to an existing web test if the pattern exists — mirror `RunDetail.test.js`)

**Interfaces:**
- Consumes: `GET /api/v1/jobs/{name}/schedulability` returning `{requiredCaps, selector, satisfiable, reason, selectorDependsOnParams}`.
- Produces: a warning banner rendered only when `satisfiable === false`.

- [ ] **Step 1: Write the failing test**

```js
// web/src/routes/JobDetail.test.js — mirror RunDetail.test.js's render+mock-fetch setup
// Case A: schedulability {satisfiable:false, reason:"no registered agent provides capability [native]"}
//   -> the rendered output contains the reason text and a warning role/class.
// Case B: {satisfiable:true} -> no warning banner in the output.
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd web && npm test -- JobDetail`
Expected: FAIL

- [ ] **Step 3: Implement**

In `JobDetail.svelte`, add to the `<script>`:

```js
  let sched = null;
  async function loadSched() {
    try {
      sched = await apiFetch("/api/v1/jobs/" + encodeURIComponent(jobName) + "/schedulability");
    } catch (_) { sched = null; }
  }
```

call `loadSched()` in `onMount`/the reactive `jobName` load alongside the runs fetch, and render near the top of the container (after the header):

```svelte
  {#if sched && !sched.satisfiable}
    <div class="warn-banner" role="alert" style="border:1px solid var(--warn,#b8860b);background:var(--warn-bg,#fff8e1);color:var(--warn-fg,#7a5b00);padding:0.6rem 0.9rem;border-radius:6px;margin-bottom:1rem">
      ⚠ This job can't be scheduled right now: {sched.reason}. Runs will stay Queued until a matching agent registers.
    </div>
  {/if}
```

(Use theme variables consistent with the app; if the app has an existing banner/alert component or class, reuse it instead of inline styles.)

- [ ] **Step 4: Run to verify it passes**

Run: `cd web && npm test -- JobDetail`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add web/src/routes/JobDetail.svelte web/src/routes/JobDetail.test.js
git commit -m "feat(web): unschedulable warning banner on job detail"
```

---

### Task 7: docs

**Files:**
- Modify: `docs/agents.md`, `docs/jobs.md`, `docs/troubleshooting.md`, `docs/migration-2026-07-job-isolation.md` (or a new short migration note)

- [ ] **Step 1: Full sweep**

Run: `go build ./... && go test ./internal/... -count=1 && (cd web && npm test)`
Expected: PASS. Fix stragglers (grep for old `CreateRun(`/`UpsertAgent(` arity, the removed `appendLabelIfMissing`).

- [ ] **Step 2: Write docs**

- `docs/agents.md`: a "Capabilities and routing" section — agents advertise `native`/`container`/`pod`; the standard agent reports `native` (+`container` when a runtime is present), the k8s agent `pod`+`container`; the controller infers a job's requirement and routes automatically, so you usually don't hand-write an agentSelector to keep native jobs off k8s.
- `docs/jobs.md`: note that `native: true` jobs are routed to a host agent automatically (no k8s-excluding selector needed), and a host-runnable podTemplate can now run on a standard agent (claim pod).
- `docs/troubleshooting.md`: "Job stays Queued / unschedulable warning" — what the JobDetail banner means and how to fix (register an agent with the needed capability, or adjust the selector).
- Migration note: the `podTemplate → kubernetes`-label pin (8ca1567) is now expressed as the `pod` capability; agents must be upgraded to advertise capabilities, and un-upgraded (legacy null-caps) agents keep matching by labels only during the rollout.

- [ ] **Step 3: Commit**

```bash
git add docs
git commit -m "docs: agent capability routing and unschedulable-job warnings"
```
