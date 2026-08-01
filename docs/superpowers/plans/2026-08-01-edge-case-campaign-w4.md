# Edge-Case Campaign: Wave W4 (Kubernetes Agent) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Wave W4 (Kubernetes agent scenarios) of the edge-case campaign: three scenarios against a k8s agent running in **kind**, recording findings.

**Architecture:** Same recording pattern as W0-W3 — per-scenario runbooks under `test/edgecase/scenarios/`, findings appended to `test/edgecase/FINDINGS.md`, raw captures to the session scratchpad and copied to `<project parent>/edgecase-evidence/w4/` at the checkpoint. **The execution environment changes completely**: kind instead of docker compose, `kubectl` instead of `inject.sh` for most faults.

**Tech Stack:** kind, `kubectl`, the existing `test/ha` compose stack (controllers stay there), a controller-side config file with `kubernetesClusters` + `kubernetesEnrollmentPolicies`, and a two-way network bridge between the kind and compose docker networks.

## Read this before planning anything: two chartered premises are false

**1. The spec's kind-wiring sentence is wrong.** `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:115-117` says "the controller stays on the compose stack, reachable from kind via host networking — **only the enrollment URL** changes". That understates the work by a large margin:
- The k8s agent **cannot use a static token at all** — no `Token` field exists in `k8sagent.Config` (`internal/k8sagent/config.go:16-48`), `enrollmentPolicy` is mandatory (`config.go:190-192`), and static k8s auth was deliberately removed in PR #75 (`4e8f315`).
- **Enrollment is bidirectional.** `kubernetesEnrollmentVerifier.Verify` (`internal/controller/agent_enrollment_kubernetes.go:60-104`) makes two live Kubernetes API calls — `TokenReviews().Create` with audience `unified-cd-agent-enrollment` (`:70`) and `Pods(ns).Get` to confirm the pod binding (`:93`, `:100-102`). **The controller must reach into the cluster**, so it needs a kubeconfig and RBAC.
- **The compose controllers cannot do this today.** The verifier map is built only from a YAML config file loaded via `-f` (`cmd/controller/main.go:324-338`, `internal/config/controller.go:16-21`), and `test/ha/docker-compose.ha.yaml:78-101` configures its controllers with **env vars only — no `-f`, no config file mount**. With an empty verifier map, `api_agent_enrollment.go:346-351` returns **503 "kubernetes identity unavailable"**. Enrollment fails closed.
- **HTTPS is enforced agent-side** unless `allowInsecureHTTP: true` or a loopback host (`config.go:228-248`); the compose nginx serves plain HTTP on `18080`.

**2. W4-2's chartered mechanism is dead.** The charter is "verify the known missing `update`/`patch` verbs finding — is reuse still silently degraded?" **It was fixed in PR #50 (`6b0bf8f`, 2026-07-15)** — the same day the campaign memory recorded the pre-fix diagnosis. Every manifest now grants `update, patch` on pods (`manifests/base/k8s-agent/rbac.yaml:8-13`, and the three generated bundles). Running the scenario as chartered would consist of reading four YAML files and confirming a fix. **W4-2 is re-chartered below.**

## Task ordering — deliberate, do not reorder

**All three scenarios are blocked by the same thing: there is no deployed k8s agent anywhere in this repo's automation.** CI's "Kubernetes integration tests" job (`.github/workflows/ci.yml:80-89`) runs `go test -tags k8s ./internal/k8sagent/...` **on the runner host** against kind's API with the default kubeconfig — it never applies a manifest, never enrolls, never involves a controller. It proves kind can be created; it proves nothing about the W4 surface.

So: **Task 1 is a spike on the single highest-uncertainty item (enrollment), and it is allowed to fail.** If enrollment cannot be made to work, the rest of the wave changes shape and we will have spent an hour, not a day. Infrastructure is Tasks 1-3; scenarios are 4-6, ordered cheapest-first.

## Global Constraints

- All committed text is English (AGENTS.md).
- Work on branch `plan/edge-case-w4` in worktree `wt-edge-spec` — never commit on the main checkout.
- **No production-code changes** (spec §8). Test-only files under `test/edgecase/` and docs. **`manifests/` must NOT be modified** — W4 needs its own overlay or a scenario-local copy, because `manifests/` is shipped product. Say which you chose and why.
- Findings record problems; they do not fix them. Classification: **violation** = contradicts an invariant (I1-I7) **or a documented contract** (`docs/*.md`); **observation** = as-designed but reveals risk. Third bucket for defects in the campaign's own assets, outside both tallies.
- Observation entries say "observation" in the **title** and repeat it in the Severity line (`FINDINGS.md:481`).
- **Quote the invariant verbatim** from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:44-55`.
- Every number traceable to a capture whose window covers it. Label derived / inferred / code-read. Annotate uncaptured live observations. **Do not write "never" for a window you ended yourself.**
- Kill every sampler before teardown and **capture** it. Scrub credentials from captures — W3 leaked tokens twice.

## Scope rules this campaign paid for — all six were real

1. **Never `head` a docs survey; always report the hit count.** W3-3's contract limb was found only on the untruncated re-run.
2. **Check whether this branch has already ruled on the passage you cite** — and **grep `FINDINGS.md` for the finding itself, not just the doc text**. W3-5 filed a re-file of W2-2 because its check covered doc passages only.
3. **An inline comment inside a function body is not a contract.** `FINDINGS.md:479` enumerates "an exported API field, a schema column, or a statement in `docs/`".
4. **When you claim a class is fully enumerated, verify the enumeration.** W3-6 claimed two producers while a third sat unevaluated in its own capture.
5. **An invariant must be contradicted by its own text, not its spirit** (`FINDINGS.md:1509`).
6. **A verb/arm is verified when some capture measures its effect**, not when its comment carries a measurement. W3's `s3-slow` passed all three of its own load-confirmations while doing nothing.

## Verified code facts (do not re-derive)

**Read at HEAD `c98d229`. Per the W1/W2/W3 lessons these are claims, not givens — seven of eight W3 tasks corrected something here, and the pattern is that the block's *mechanism* claims fail while its `file:line` claims hold. Two W3 scenarios' planned mechanisms proved unreachable outright. If execution contradicts one, that contradiction is itself a finding.**

### Pod GC (W4-1)

- `internal/k8sagent/podgc.go`, started at `internal/k8sagent/agent.go:139`: `go a.runPodGC(runCtx, time.Minute)`. **Interval hardcoded**; `runPodGC` accepts an interval with a `<= 0 → time.Minute` guard (`podgc.go:102-104`) but the only non-test caller passes a literal. No DSL field, flag, env var or config field.
- **Not leader-elected, no advisory lock** — zero non-test hits for `leader` in `internal/k8sagent/`. `manifests/base/k8s-agent/deployment.yaml:9` sets `replicas: 2`, so **two agent processes each run an independent unsynchronised sweep** over the same namespace.
- `listRunPods` (`podgc.go:125-141`) lists `app=unified-cd-agent` in `cfg.Namespace` — **not scoped to this agent's own runs**.
- Predicate `podGCDecision` (`podgc.go:19-24`): `if poolManaged { return false }; return !found || isTerminalRunStatus(runStatus)`. `poolManaged` is `pod.Annotations["unified-cd/pool-status"] != ""` (`podgc.go:137`) — deliberately pool-*status*, not pool-template (rationale at `:119-124`). `isTerminalRunStatus` = `Succeeded|Failed|Cancelled` (`:27-34`).
- **Unresolvable run → skip and retry next sweep** (`podgc.go:76-81`). Only a definitive **HTTP 404** counts as gone — `isRunNotFound` (`:57-60`) requires `errors.As(err, &*agentlib.HTTPError)` **and** `StatusCode == 404`. Connection refused, timeout, 500, 502, 503 all skip.
- **"Skip-not-delete" IS a documented contract.** `docs/high-availability.md:431-433`: *"Pods still owned by the Pod-reuse pool are never touched. Any other error resolving the Run (a transient controller/network blip) causes that Pod to be skipped for the cycle rather than deleted, since deleting the Pod for a Run that's actually still live would spuriously kill it."* Plus the ~1 minute interval at `:428` and a threshold row at `:444`. **Three documented limbs.**
- Signals are `slog` only — `podgc.go:79` (skip), `:87` (delete failed), `:90` (deleted, with a `runFound` field). **`internal/k8sagent/` emits no Prometheus metrics at all** (zero hits for `metrics.`/`prometheus` excluding tests).
- **NOT ESTABLISHED:** what terminal status a run lands in if its pod *is* deleted mid-run. `agent.go:340-349` has no re-create path and `podgc.go:51-52` says pod-per-run does not resume, but the step-error handling inside `agentlib.RunClaim` (`agent.go:384`) was not traced. Two plausible readings: `Failed` via step error, or stuck `Running` until the controller's stuck-run reaper fires at `staleAfter` 90 s. **Settle this cheaply before writing an expectation into a runbook.**

### Pod reuse (W4-2, re-chartered)

- **RBAC is fixed at HEAD.** `manifests/base/k8s-agent/rbac.yaml:8-13` and all three generated bundles grant `create, get, list, delete, watch, update, patch` on pods. No Helm charts exist. The only other pods rule is the controller's enrollment Role (`manifests/base/controller/rbac.yaml:30`, `pods: [get]`).
- Three writers need the verb: `PodPool.ClaimPod` → `PodManager.UpdatePodAnnotations` (`pool.go:194-197` → `podmanager.go:77-93`), `PodPool.ReleasePod` (`pool.go:246-259`, calls `Update` **directly**, bypassing `PodManager`), and `PodPool.Restore` (`pool.go:336`). **All three use `Update`; none uses `Patch`** — so the `patch` verb is granted but unexercised (inferred from grepping for `.Patch(`).
- **The swallow is real and still present.** Release path (`pool.go:255-259`): on `Update` error → `slog.Warn("pool: failed to mark pod idle, deleting", ...)` → `DeletePod`. Claim path (`pool.go:198-205`): on error → `slog.Warn("pool: pod claim conflict, creating new pod", ...)` → `DeletePod` → `createPoolPod`. **The claim-path message says "claim conflict" regardless of cause** — a 403 Forbidden is reported under a message naming optimistic concurrency. Same class as the already-filed misleading-suffix finding at `FINDINGS.md:826`.
- **The run is unaffected** — `ReleasePod`'s error is swallowed by its caller too (`agent.go:316-318` logs and discards). The run succeeds; only reuse silently does not happen. **No metric anywhere.**
- **`podTemplate.reuse` IS a documented promise.** `docs/kubernetes-integration.md:293`: *"With `reuse: true`, the Pod is returned to a pool after the run and reused by the next run."* Plus `docs/jobs.md:1657` (field table) and `docs/field-reference.md:342` (published schema field).
- **A free finding available with no cluster:** `docs/configuration.md:458` glosses `poolIdleTimeout: 0` as "(no reuse)", but 0 disables **eviction**, not reuse — `Config.PoolIdleTimeoutDuration()` returns 0 when unset (`config.go:122-131`), `StartEviction` is a no-op at 0 (`pool.go:131-133`), and `ReleasePod` still pools the pod. Verify and file if the wave gets squeezed.

### `podStartTimeout` (W4-3)

- Config field `podStartTimeout` (`config.go:44`), env override **`UNIFIED_K8S_POD_START_TIMEOUT`** (`config.go:168-170`), **no flag** (`cmd/k8s-agent/main.go` defines only `--config` and `--log-level`). Default **5m** (`defaultPodStartTimeout`, `config.go:138`); unset/unparseable/non-positive → 5m (`config.go:142-151`).
- **Deliberate asymmetry:** `Validate` **rejects** an unparseable value at boot (`config.go:215-219` → `os.Exit(1)`) while `PodStartTimeoutDuration()` **falls back to 5m** for the same input. Both documented (`docs/configuration.md:456`).
- `awaitPodRunning` (`agent.go:415-456`) wraps `pm.WaitForPodRunning` (`podmanager.go:96-115`, a 500 ms `Get` poll) in `context.WithTimeout`. On timeout `executeRun` (`agent.go:346`) calls `failRun` (`:392-404`), which writes the reason into the run's own log at `stepIndex -1` via `AppendLogBulk` then `RetryUntilSuccess(FinishRun(RunFailed))` — **bounded and reported**; the run never sits stuck `Running`. The deferred cleanup (`agent.go:333-337` / `:307-319`) deletes the wedged pod.
- **Second exit:** a concurrent goroutine (`agent.go:426-446`) polls `GetRun` at `agentlib.CancelPollInterval` and cancels the wait if the controller marks the run terminal, returning `masterTerminal=true`; the caller then **abandons without writing status** (`:342-345`), deliberately not overriding the controller. Documented at `docs/kubernetes-integration.md:207`.
- **Pooled-pod not-ready uses the SAME timeout — there is no separate one.** `awaitPodRunning` is called once at `agent.go:340`, after the pooled/fresh branch converges. The difference is cleanup only, via the `podReady` bool (`:292`, set at `:349`): a never-ready pooled pod is **deleted, not returned to the pool** (`:307-319`), so the pool self-heals. Both defers use `context.Background()` so cleanup survives claim-context cancellation.
- **A third, distinct timeout site** shares the knob: the `uses:`-scope pod's Ready wait (`backend.go:167`), added by PR #90. `config.go:133-137` states this is deliberate. **Do not conflate the three sites.**

### Environment facts

- **No kind config, no Makefile kind target, no kind runbook in `docs/`.** The only k8s Makefile target is `manifests:` (`Makefile:51-54`), which regenerates bundles via `kubectl kustomize`.
- **No local k8s-agent image build.** `docker/k8s-agent.Dockerfile` exists and is self-contained (`RUN CGO_ENABLED=0 GOOS=linux go build -o /k8s-agent ./cmd/k8s-agent`, `:7`) and also ships `/ucd-sh` (`config.go:31-34`), so one image covers agent + shim. It is built **only** by `release-docker.yml` on tag push, never by `ci.yml` or `make`.
- **`Config.Kubeconfig` (`config.go:36`) + `buildRestConfig` (`cmd/k8s-agent/main.go:97-104`) fall back to `~/.kube/config`** — so the agent binary **can run on the host against kind**, with no image build, no `kind load`, no Deployment, and the process's stdout directly available. That matters because W4-1 and W4-2's only signals are log lines.
- Minimum manifest change set if the in-cluster route is needed: `server:` URL, **`allowInsecureHTTP: true`**, `image:` + **`imagePullPolicy: IfNotPresent`** (`:latest` otherwise resolves to `Always` and fails against a registry-less kind image — `ci.yml:83-87` documents this trap), `shimImage:` (defaults to the agent's own image at GHCR `:latest`, `config.go:58`), `podImage:` (`golang:1.24-alpine`), `replicas: 2 → 1` unless the scenario wants two GCs, and `namespace: ci` — **the base only creates `unified-cd` (`manifests/base/namespace/`), so `ci` may not exist. Check.**
- Job fixtures must target **`kind:kubernetes`**, not `kind:linux` (`config-configmap.yaml:14`). The k8s agent enrolls with capabilities `["pod","container"]`.
- `sidecarS3SecretName` is unset → cache steps become silent no-ops and artifact steps fail. Fine for W4-1/W4-3; do not let a fixture depend on artifacts.
- **`inject.sh` transfer:** `kill-soft`/`kill-hard`/`pause`/`unpause`/`partition`/`heal` are **useless as-is** against kind (they take compose service names and hardcode `unified-cd-ha_default` / `unified-cd-ha-$svc-1`). `nginx-block`'s **concept** transfers and is exactly what W4-1 needs, but it resolves the agent's IP via `docker inspect` on a compose container — for a kind-hosted agent the source IP nginx sees is the kind node or docker gateway, so it needs rework or a blunter substitute (`kill-hard` all three controllers briefly). The `s3-*` family is not needed. `bulk-submit.sh`, the `workloads/*.payload.json` fixtures, the `FINDINGS.md` format and the runbook house style all transfer unchanged.
- **kind's default CNI (kindnet) does NOT enforce NetworkPolicy** — a NetworkPolicy-based partition needs Calico or Cilium installed. Inferred, not tested.
- **`podcap-job.payload.json` is 90% of W4-3's fixture.** It sets a pod-level `nodeSelector: disktype: ssd` (which makes `dsl.RequiredCaps` infer capability `pod`) with `agentSelector: kind:linux`. Change the selector to `kind:kubernetes` and the run becomes claimable by the k8s agent, then sticks in `Pending` because no kind node matches — exactly W4-3's trigger. **Copy it to a new name; do not edit in place** — leaving `edge-podcap-job` claimable invalidates W2-4 Part D's premise on any shared rig (`scenarios/w2-4-queued-reaper.md:207`).
- **No fixture exercises `podTemplate.reuse`.** W4-2 and W4-3's pooled arm both need a new one.

## Facts NOT established — open questions, not givens

- What terminal status a run reaches if its pod is deleted mid-run (see Pod GC above). **Task 4 or 5 must settle it.**
- Whether the `ci` namespace exists in any applied manifest.
- Whether `kubectl create token --bound-object-kind Pod` satisfies `parseBoundPodClaims` (`agent_enrollment_kubernetes.go:128-130`) and the verifier's UID re-check (`:93`, `:100`). **Task 1 settles this.**
- kind↔compose network bridging specifics, and whether the controller→kind TLS path needs `insecure-skip-tls-verify` (kind's API cert covers `127.0.0.1`, `localhost` and the node IP — **not** `host.docker.internal`).
- Whether `patch` is exercised by any code path (all three writers use `Update`).

---

### Task 1: Enrollment spike — the go/no-go

**This task is allowed to fail, and failing it early is the point.** Enrollment is the highest-uncertainty item in the wave and everything else depends on it. Budget ~1 hour. Do not build an image, do not write a manifest, do not touch the compose stack's steady state until this answers.

**Files:** Create `test/edgecase/scenarios/w4-0-enrollment-spike.md` (a spike record, not a scenario runbook)

- [ ] **Step 1: Stand up kind** and record the exact commands and versions. Confirm the `ci` and/or `unified-cd` namespaces, and confirm which the agent config expects.
- [ ] **Step 2: Get the controller a kubeconfig and the config file it needs.** Write a controller config YAML with `agentAuth.kubernetesClusters` (name + kubeconfig path) and `agentAuth.kubernetesEnrollmentPolicies` (with `subjectConstraints` naming the exact namespace + ServiceAccount), mount it and a kubeconfig into all three compose controllers, and add `-f` to their command. **This is a `test/ha/` change — use an overlay under `test/edgecase/compose/`, not an edit to `test/ha/docker-compose.ha.yaml`.** Confirm the controllers boot and that `bootstrapKubernetesEnrollmentPolicies` (`cmd/controller/main.go:156-169`) seeded the policy row.
- [ ] **Step 3: Bridge the networks.** kind's nodes are Docker containers on the `kind` bridge; the compose stack is on `unified-cd-ha_default`. `docker network connect` is the idiom already proven at `inject.sh:88-89`. Verify **both directions**: agent → controller (HTTP 18080) and controller → kind API (TokenReviews + Pods.Get). Record the addresses used and any TLS workaround.
- [ ] **Step 4: Run the agent on the host against kind** — `Config.Kubeconfig` pointing at kind, `allowInsecureHTTP: true`, `server:` at the reachable controller URL. Obtain a pod-bound token: `kubectl create token <sa> -n <ns> --audience=unified-cd-agent-enrollment --bound-object-kind Pod --bound-object-name <a live pod>` written to `serviceAccountTokenFile`, where that pod's `spec.serviceAccountName` matches. **Report whether enrollment succeeds, and if it fails, the exact error and which side produced it.**
- [ ] **Step 5: Report the verdict and its consequences.** If enrollment works host-side, say what that route can and cannot support — note it runs with an admin kubeconfig, which makes it **unusable for W4-2's RBAC-denial arm**. If it does not work, say what would be needed and stop; the wave is re-planned rather than pushed.
- [ ] **Step 6: Commit** the spike record and any overlay/config assets.

---

### Task 2: The rig — whichever route Task 1 licensed

**Files:** Create `test/edgecase/compose/k8senroll.override.yaml`, `test/edgecase/k8s/` (kind config, agent config, any manifest overlay), `test/edgecase/workloads/w4-pending.payload.json`, `w4-reuse.payload.json`; Modify `test/edgecase/README.md`

- [ ] **Step 1: Decide and record the route** — host-run agent vs in-cluster Deployment — with the reason, and say which scenarios each route can serve. **W4-2's RBAC arm requires in-cluster**; W4-1 and W4-3 do not.
- [ ] **Step 2: If in-cluster is needed**, build and load the image (`docker build -f docker/k8s-agent.Dockerfile -t ucd-k8s-agent:w4 . && kind load docker-image ucd-k8s-agent:w4`) and produce a **scenario-local manifest overlay** — do NOT modify `manifests/`, which is shipped product. Apply the minimum change set from the facts block, and **verify each of the seven changes took** rather than assuming.
- [ ] **Step 3: Build the fixtures**, verified through the real `dsl.Parse` with `KnownFields(true)` — paste the output; W1 shipped two payloads that 400'd on a wrong key path.
  - `w4-pending.payload.json` — a copy of `podcap-job.payload.json` with `agentSelector: kind:kubernetes`. Confirm it is claimable by the k8s agent and then sticks `Pending`.
  - `w4-reuse.payload.json` — `podTemplate.reuse: true`, a step that writes and reads a `/workspace` marker and prints `hostname`, so reuse is observable as marker-persistence **and** pod-name identity.
- [ ] **Step 4: Write the k8s fault-injection helpers** the wave needs — there is no k8s fault tooling in this repo, so this is the first. At minimum: delete a run's pod by label, make the controller unreachable from the agent (the W4-1 lever), and read pod annotations (`unified-cd/pool-status`, `pool-key`, `pool-run-id` — `pool.go:20-31`). **Every arm needs an effect measurement in its own capture** — the `s3-slow` lesson: three load-confirmations passed while the arm did nothing.
- [ ] **Step 5: End-to-end baseline** every later task cites: agent enrolled and claiming; a trivial job runs to `Succeeded` on the k8s agent; a pod is created and cleaned up. **Any limb failing blocks the dependent task — say which.**
- [ ] **Step 6: Commit** in increments.

---

### Task 3: W4-3 — `podStartTimeout` and pooled-pod not-ready

**Cheapest scenario: no fault injection, fixture already 90% written, and `UNIFIED_K8S_POD_START_TIMEOUT=30s` collapses the 5m default so trials are fast. Run it first.**

**Files:** Create `test/edgecase/scenarios/w4-3-pod-start-timeout.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I5

- [ ] **Step 1: Write the runbook.**
  - **Part A — cold start.** `w4-pending` with the unsatisfiable `nodeSelector`. Confirm the pod is created, stays `Pending`, and at the timeout `failRun` writes the reason into the run's own log at `stepIndex -1` and the run reaches `Failed` — bounded and reported. Measure the actual elapsed time against the configured timeout.
  - **Part B — the pooled arm.** Same trigger with `reuse: true`. Confirm the **same** timeout governs (there is no separate one) and that the difference is cleanup: the never-ready pooled pod is **deleted, not returned to the pool** (`agent.go:307-319`). Verify by pod name and by the absence of an idle pooled pod afterwards.
  - **Part C — the second exit.** Cancel the run while the pod is `Pending`. Confirm the concurrent poller (`agent.go:426-446`) cancels the wait and the agent **abandons without writing status**, deliberately not overriding the controller. This is documented behaviour (`docs/kubernetes-integration.md:207`) — record it as conformance.
  - **Part D — the boot/runtime asymmetry.** `Validate` rejects an unparseable `podStartTimeout` at boot while `PodStartTimeoutDuration()` falls back to 5m for the same input. Both are documented; confirm both and record whether the pairing is discoverable by an operator.
  - Recording: a run stuck `Running` past the timeout = major (I5 — the bound is documented at `docs/configuration.md:456`). A wedged pod returned to the pool = major. Bounded, reported failure = conformance observation.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown + commit** (scenario id w4-3).

---

### Task 4: W4-1 — Pod GC racing a live run

**Files:** Create `test/edgecase/scenarios/w4-1-podgc-skip-not-delete.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I1, I2

- [ ] **Step 1: Settle the open question first** — what terminal status does a run reach if its pod is deleted mid-run? Trace `agentlib.RunClaim`'s step-error handling (`agent.go:384`) or produce it live with a direct `kubectl delete pod`. **Record the answer with `file:line` or a capture before writing any expectation.** Both readings (Failed via step error; stuck `Running` until the stuck-run reaper at 90 s) are plausible and they imply different findings.
- [ ] **Step 2: Write the runbook.**
  - **Part A — the contract.** Make `GetRun` return a **non-404** error during a sweep (controllers unreachable) while a run is live, and confirm the pod is **skipped, not deleted**, and that the next sweep retries. `docs/high-availability.md:431-433` is the contract; quote it verbatim. **Note the 60 s hardcoded interval means every trial costs up to a full minute and the window cannot be phase-locked** — budget accordingly and report the attempt count either way.
  - **Part B — the 404 branch.** Confirm that a definitive 404 *does* delete, so Part A is a discrimination and not a null result.
  - **Part C — pool-managed pods are never touched** (`docs/high-availability.md:431`). Use the `w4-reuse` fixture and confirm a pooled pod survives a sweep that would otherwise collect it.
  - **Part D — two unsynchronised sweeps.** The GC is not leader-elected and `replicas: 2` is the shipped default. If the rig runs two agents, measure whether both sweep the same namespace and what that costs. If it runs one, record the code-read and say so.
  - Recording: a live run's pod deleted on a transient error = major (contract at `:431-433` plus I1/I2 depending on what Step 1 established). Note the campaign's rule — an inline comment is not a contract, but here you have a real `docs/` statement, so lead with it.
- [ ] **Step 3: Commit runbook.** **Step 4: Execute.** **Step 5: Findings + teardown + commit** (scenario id w4-1).

---

### Task 5: W4-2 (RE-CHARTERED) — the swallow, not the verbs

**The chartered mechanism is dead (PR #50 fixed it on the day the memory was written). This task targets what survives the fix.**

**Files:** Create `test/edgecase/scenarios/w4-2-reuse-swallow.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I5

- [ ] **Step 1: Record the re-charter in the runbook's opening section**, in the house style — state what the spec asked for, prove with `file:line` why that mechanism no longer exists (`manifests/base/k8s-agent/rbac.yaml:8-13`, PR #50 `6b0bf8f`), and declare the substitute while holding the scenario ID, invariant and recording rules fixed. **Also record the campaign-process correction:** the memory that motivated this scenario carries a `RESOLVED` header that the wave's briefing dropped, and the explorer caught it independently. That is a process finding worth one paragraph.
- [ ] **Step 2: Write the runbook.**
  - **Part A — deny the verb.** Apply the agent Role minus `update`/`patch` (scenario-local, not an edit to `manifests/`). **This requires the in-cluster route** — a host-run agent uses an admin kubeconfig and cannot be denied.
  - **Part B — the swallow.** Run `w4-reuse` twice. Confirm: reuse silently does not happen; the run still **succeeds**; there is **no metric** (`internal/k8sagent/` has no Prometheus instrumentation at all — verify the enumeration rather than asserting it); and the only signal is a `slog.Warn`.
  - **Part C — the misleading log.** Confirm the claim path reports a 403 Forbidden under `"pool: pod claim conflict, creating new pod"` (`pool.go:198-205`) — a message naming optimistic concurrency for an authorization failure. Same class as `FINDINGS.md:826`.
  - **Part D — the contract.** `docs/kubernetes-integration.md:293` promises reuse. Judge whether silent non-reuse contradicts it. Run the **untruncated** docs survey with hit counts, and grep `FINDINGS.md` for the finding itself.
  - **Part E — the free finding, no cluster needed.** `docs/configuration.md:458` glosses `poolIdleTimeout: 0` as "(no reuse)", but 0 disables **eviction** while `ReleasePod` still pools the pod (`pool.go:131-133`, `:263-268`). Verify and file separately.
  - Recording: silent non-reuse against a documented promise = judge on the contract limb. The misleading log = diagnosability, likely minor. **Do not inflate** — three of W2's nine scenarios produced only observations and that was right each time.
- [ ] **Step 3: Commit runbook.** **Step 4: Execute.** **Step 5: Findings + teardown + commit** (scenario id w4-2).

---

### Task 6: W4 checkpoint

**Files:** Modify `test/edgecase/FINDINGS.md`, `test/edgecase/README.md`, `<project parent>/edgecase-evidence/README.md`

- [ ] **Step 1: Append `## Checkpoint: W4 complete`** following the W3 checkpoint's format and classification rule. State at minimum:
  - (a) **The wave's two false premises** — the spec's "only the enrollment URL changes" and W4-2's dead charter — and what a future wave should take from them. This is the fourth consecutive wave whose facts block was corrected by execution; say whether the mechanism-vs-`file:line` pattern held again.
  - (b) **What the rig cost and what survives it.** kind wiring, the controller config file, the network bridge, the k8s fault helpers — say what a later wave can reuse and what was scenario-local.
  - (c) **The RBAC blind spot:** this repo's RBAC is never exercised by any test or CI job (CI's k8s tests authenticate as cluster-admin and never apply the agent's Role). That is a coverage gap independent of any finding.
  - (d) **`internal/k8sagent/` has no Prometheus instrumentation at all** — every W4 signal is a log line. Note it as an input to W6.
  - (e) Carry-forwards still open: `RunGitResolver` (from W2-1, still needs a git fixture), the `LogPusher` quadratic amplification (W6-S2), and the campaign's two invariant-set coverage gaps (no secret-store integrity clause, no cache-integrity clause — W3).
- [ ] **Step 2: Archive the evidence** to `<project parent>/edgecase-evidence/w4/`, verify with `diff -r`, **scrub credentials** (W3 leaked tokens twice — projected SA tokens and kubeconfigs are both credential material), and update both READMEs.
- [ ] **Step 3: Commit** (`test(edgecase): record W4 checkpoint`).

---

## CORRECTED AFTER EXECUTION (W4 checkpoint, 2026-08-01)

**Appended at the end rather than inline, deliberately: `scenarios/w4-3-pod-start-timeout.md:44` cites `plan:71-84` and `:74` by line, so an in-place banner would have silently invalidated those citations. W3's merge gate (`FINDINGS.md:2112`) requires the propagation, not a particular placement.**

**What this plan got right, and it is worth stating first because it is the exception in this campaign:** the two chartered false premises were caught and refuted **before** the wave began, at `:11-19`, each with its `file:line` at HEAD. No previous wave's plan corrected its charter pre-execution. `:23`'s CI statement also held under check.

**What was NOT propagated: three post-execution corrections to the "Verified code facts" block (`:47-99`), each recorded in a runbook and none of which reached this file until now.** This is the recorded-versus-propagated distinction `FINDINGS.md:739` draws.

- **`:53` understates the pod-GC interval finding.** "the only non-test caller passes a literal" — `runPodGC` has **no test call site at all**, so the `<= 0 → time.Minute` guard is unreachable code at HEAD. Corrected at `scenarios/w4-1-podgc-skip-not-delete.md:93-100` (CORRECTION 1); the entry filed on it is `FINDINGS.md:2374`.
- **`:59`'s metrics claim is scoped narrower than what was measured.** It reads "zero hits … **excluding tests**". The measurement is stronger and is an enumeration: `internal/k8sagent/` is **53 files** (flat, no subdirectories; `ls -1` and `find -type f` agree), and `prometheus|promauto|metrics\.|Metric` returns **0** across all 53, **tests included**. Verified twice by execution — `w4-1-podgc-skip-not-delete.md:109-115` (CORRECTION 2) and `w4-2-reuse-swallow.md:810-827` — and both record that a pre-execution figure of **60** was wrong. Note `scenarios/w4-rig.md:55` asserts the same conclusion flatly, with no enumeration and no capture; the 53 is what backs it.
- **A reachability claim built on `:66` is wrong in general — and the attribution matters, so it is stated precisely: the claim is NOT in this block.** `:66` describes the two swallows accurately and says nothing about reachability. What was wrong is the **Task 5 brief's reconnaissance**, which concluded from a code read that `ClaimPod`'s "pod claim conflict" branch is unreachable under an RBAC denial (because a denied `ReleasePod` never appends to `p.pods`). It is reached on **any agent restart that inherits an idle pooled Pod**, which at the default configuration is every restart once such a Pod exists — `Restore` re-adopts without calling `Update`, so the denial is never exercised at restore and the next `ClaimPod` meets the 403. Corrected at `w4-2-reuse-swallow.md:125-132` (CORRECTION 1) and then executed; the entry is `FINDINGS.md:2409`. **This is the one that had a whole scenario part resting on it, and it is recorded here even though the plan is not the document that carried it, because a reader checking the block will otherwise not find it anywhere.**
- **`:74` states a docs fact that is the opposite of what `docs/configuration.md:456` says.** The plan asserts the boot-rejection/accessor-fallback asymmetry is "Both documented (`docs/configuration.md:456`)". Only the fallback is documented; the boot rejection is documented **nowhere operator-facing**, and `:456` promises the fallback for the very input that refuses to boot. Corrected at `scenarios/w4-3-pod-start-timeout.md:49-85` (CORRECTION 1); it is the wave's W4-3 violation, `FINDINGS.md:2282`.

**Environment facts (`:80-99`) that execution settled, listed because two of them are refutations and the rest close open questions.** No `kind` CLI is needed and none is installed — Docker Desktop's Kubernetes **is** kind, so the topology came free (`w4-0-enrollment-spike.md:53-79`); `insecure-skip-tls-verify` was **not** required, because the node certificate's SANs cover the name the kubeconfig uses (`:252-272`, refuting `:98`); `:96`'s open question is resolved — **`ci` and `unified-cd` both already exist**; `:97`'s is resolved **YES** — `kubectl create token --bound-object-kind Pod` does satisfy `parseBoundPodClaims`, and the enrollment failure is downstream of it; `:99`'s is resolved — `patch` is exercised by **nothing** in the package (`.Patch(` → 0), and `watch` likewise, so two of the seven granted verbs are dead grants. One environment fact the block did not anticipate at all: a **pre-#75 k8s-agent Deployment has been crash-looping on this machine for 14 days** (`w4-0-enrollment-spike.md:100-126`), left untouched.

**The pattern, judged rather than restated:** W3 refined the rule to "the block's `file:line` claims hold while its *mechanism* claims fail". W4 is the fifth consecutive wave and the `file:line` half held again — every line number, constant and span in `:47-99` survived checking, with only the `:53` scope adjustment above. The mechanism half held too, three times over. **W4 adds a third class the earlier waves did not separate: *environment* claims, which failed at a higher rate than mechanism claims did** — four of the block's environment assertions were wrong or moot, and unlike a mechanism error they cost bring-up time rather than a scenario design. The instruction that follows is narrow: an environment claim is checkable in one command at the start of the first task, and W4-0's Step 1 is the model — it spent the first ten minutes checking them and recovered an hour it had budgeted for a TLS workaround that was never needed.
