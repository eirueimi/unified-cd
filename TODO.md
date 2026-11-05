# Active Backlog

This file contains active work only. Completed history is available in Git.
Last audited: 2026-07-26 against `1e46459`.

## Critical

No active items.

## High

### 45. Host agent claim-pod lifecycle parity with the Kubernetes agent

- **Status:** Needs design (brainstorm and spec first)
- **Impact:** The same job behaves differently on the two agents. `podTemplate.reuse` pools the pod on k8s only, so the host never gets the faster pod start. (State reuse is not a reason to add host pooling: the host has no pod-volume equivalent, and persistent state on the host comes from named volumes.) A host claim pod has no start timeout (a hung pull holds an agent slot indefinitely; k8s fails after `podStartTimeout`, default 5m), cannot be cancelled while its images are pulling, and `:latest`/untagged images are never re-pulled (kubelet pulls them every run). Containers of a crashed host agent are never reaped, while k8s runs `runPodGC` every minute.
- **Evidence:** `reuse` is read only by `internal/k8sagent/agent.go:449` and `internal/k8sagent/podbuilder.go:335` (plus apply-time checks in `internal/dsl/volumes.go:125`). `pod.Start` runs before `RunClaim` and its cancel poller (`internal/agent/agent.go:955`); scope containers start the same way (`internal/agent/scope.go:94`); no host start-timeout knob exists in `docs/reference/configuration.md`. k8s side: `PodStartTimeoutDuration` (`internal/k8sagent/config.go`), `startRunCancelWatch` (`internal/k8sagent/backend.go`), `runPodGC` (`internal/k8sagent/podgc.go`). Host orphans are documented as unswept in `docs/operator-manual/agents.md` ("Crash-orphaned claim containers").
- **Done when:** A design spec covers all four parts (part 1 may be declined) and is implemented: (1) optional, start-up latency only: the host honours `podTemplate.reuse` with the same observable semantics as k8s (workspace emptied, other state kept; `reuse` with `spec.volumes` stays rejected at apply), or the item records a decision not to; (2) a host start timeout equal to the k8s default is configurable by flag/config and documented in `docs/reference/configuration.md`, run cancel during start abandons the claim without a second `FinishRun`, and `:latest`/untagged images are pulled like kubelet; (3) a labelled startup sweep removes a crashed agent's claim containers; (4) tests prove each, and the "Crash-orphaned claim containers" and `reuse` docs describe the new behaviour.

## Medium

### 46. Cache and scope-pod configuration gaps between host and k8s agents

- **Status:** Open
- **Impact:** (a) The host cache store cannot use an HTTPS-only endpoint such as AWS S3, and a job with `cache:` on a host agent without `--cache-*` silently does nothing (the run log says nothing; k8s warns). (b) A k8s `uses:` scope pod ignores the job's pull secrets, node placement and service account, so a private or GPU/arch-pinned scope image works on the host and can fail on k8s. (c) k8s puts the first caller's resolved step env, including `{{ .Secrets.X }}` values, into the scope Pod spec (readable with `pods get`, stored in etcd); non-scoped steps send env only on the exec call. (d) Likely, verify first: a host scope container sets `WorkDir: /workspace` with nothing mounted, so a non-root scope image may be unable to write there; k8s mounts a 0777 emptyDir.
- **Evidence:** (a) `cmd/unified-cd-agent/main.go:263` builds `S3Config` with no `UseSSL`/`Region`; `internal/agent/backend_host.go:219,261` return silently when `CacheStore == nil`, while the k8s agent warns via `warnRun` for cache steps without a sidecar secret. (b) `internal/k8sagent/scopepod.go` (`buildScopePod`) takes no podTemplate. (c) `internal/k8sagent/backend.go:848` (`envMap := imageStepEnv(step)` merged with the caller's expanded env, passed to `buildScopePod` at :857). (d) `internal/agent/scope.go:59,94`.
- **Done when:** The host cache store accepts a TLS/region setting and a run with cache steps and no cache store logs one run-log warning; the k8s scope pod gets an operator-level defaults block (`imagePullSecrets`, `nodeSelector`, `serviceAccountName`) or the limitation is documented in `templates-and-reuse.md`; the scope pod spec contains no resolved step env (env travels per exec) with a test asserting it; (d) is reproduced with a non-root scope image and then fixed or closed as not reproducible.

### 47. Strict config loading in the Kubernetes agent

- **Status:** Open
- **Impact:** A misspelled key in the k8s agent config (`sidecarS3SecretName`, `maxConcurrent`, `podStartTimeout`, `workspaceSizeLimit`, `minFreeDisk`) is silently ignored and the default applies, so for example all cache steps no-op with no startup error. The host agent rejects unknown keys. An unparseable `UNIFIED_K8S_MAX_DETACHED` is also silently ignored, unlike the `UNIFIED_K8S_*_TIMEOUT` variables, which abort startup.
- **Evidence:** `internal/k8sagent/config.go` `loadYAML` uses `yaml.Unmarshal` (only `jobPodPolicy` is checked, via `KnownFields`); `config.LoadAgent` (`internal/config/agent.go:188`) uses `KnownFields(true)`. `UNIFIED_K8S_MAX_DETACHED` parse failure is skipped in `Validate` (`config.go:418-422`).
- **Done when:** `loadYAML` decodes the agent config with `KnownFields(true)` (the chart-rendered config and every example still load, and the already tolerated `podTemplates[*].workspace.pvc` handling is kept), a test shows a typo'd key fails startup, and an unparseable `UNIFIED_K8S_MAX_DETACHED` aborts startup; both are documented in `docs/reference/configuration.md`.

### 49. OIDC claim and scope handling for direct IdP setups

- **Status:** Open
- **Impact:** Connecting to some IdPs directly (without Dex) needs workarounds. `rolesClaim` names a top-level claim only, so Keycloak's nested `realm_access.roles` needs a flat protocol mapper. Claims are read from the ID token only, so an Entra groups overage (more than 200 groups) silently yields no groups. `email_verified` is never checked before `userMap` / `permissions.*.users` match on `email`.
- **Evidence:** `extractRoleValues` (`internal/controller/rbac.go:91-117`); claims decoded from the verified ID token only (`internal/controller/auth_oidc.go`). Workarounds are documented in `docs/operator-manual/identity-providers.md`.
- **Done when:** Each gap is either fixed (for example a dotted-path `rolesClaim`, ID-token overage handling, an `email_verified` check for email matches) or recorded as a decision not to; tests cover each change, and `docs/operator-manual/identity-providers.md`, `authorization.md` and `docs/reference/configuration.md` describe the result.

### 50. Prune external agent identities that have not been seen for a long time

- **Status:** Open
- **Impact:** Every Kubernetes pod UID and every cloud VM becomes a new external agent identity, and identity rows are never deleted, so an autoscaling fleet grows the `agent_identities` table (and the agent inventory) without bound.
- **Evidence:** `IssueExternalAgentAccess` in `internal/store/postgres_agent_auth.go` creates one row per `(enrollment method, external subject)` and no code path removes one. The agent ID templates `k8s:{cluster}:{namespace}:{podUID}`, `aws:{account}:{instanceId}`, `gcp:{project}:{instanceId}` and `azure:{subscription}:{resourceGroup}:{vmName}` all include a per-instance value.
- **Done when:** A documented, configurable retention (off by default) removes or archives external identities, and their credentials, whose last use is older than the limit, with a test per enrollment method; a pruned identity that returns enrolls again as a new row; `docs/operator-manual/agents.md` and `docs/operator-manual/operations.md` describe the setting.

### 51. Azure scale set instances and user-assigned managed identities

- **Status:** Open
- **Impact:** Cloud VM enrollment on Azure accepts only a plain virtual machine with a system-assigned managed identity. A VM scale set instance, or any VM that uses a user-assigned identity (shared by several VMs), is refused with `403 enrollment policy rejected`, so autoscaled Azure fleets cannot use it.
- **Evidence:** `parseAzureVMResourceID` in `internal/controller/agent_enrollment_azure.go` accepts only `Microsoft.Compute/virtualMachines`; for a user-assigned identity `xms_mirid` names the identity, not the VM, so the token alone cannot yield a unique per-VM agent ID.
- **Done when:** Scale set instances are accepted with a per-instance agent ID, and user-assigned identities either work through another signed source of the VM identity or are documented as permanently unsupported with the reason; each case has a test against a simulated issuer; `docs/operator-manual/agents.md` ("Limitations") is updated.

### 53. Audit rejected agent enrollment exchanges

- **Status:** Open
- **Impact:** A rejected `POST /api/v1/agents/enroll` (cloud provider or Kubernetes) writes no audit row. The reason (`cloud enrollment rejected`, `cloud enrollment verification failed`) is only in the controller log, so an operator who wants a queryable record of failed exchanges, for example repeated probing of a policy, has nothing in the audit log.
- **Evidence:** `handleCloudAgentEnroll` in `internal/controller/agent_enrollment_cloud.go` calls `slog.Warn` and `recordAgentAuth` (a metric) on every rejection but `auditAgentCredential` only on success.
- **Done when:** Rejected exchanges produce an audit entry with the provider, policy name and a coarse reason (never token or proof contents), subject to the existing rate limit so a flood cannot grow the audit table without bound; a test per provider; `docs/operator-manual/agents.md` and `docs/troubleshooting/agents-and-enrollment.md` say where the reason is recorded.

## Low

### 48. Apply-time validation parity and remaining knob differences

- **Status:** Open
- **Impact:** (a) A `podTemplate` with duplicate container names starts on a host agent (the second is dropped with a log warning) and fails on k8s at pod creation. (b) An absolute `spec.volumes[].mountPath` over a system directory works on the host and is refused at k8s claim time, not at apply. (c) The host has no per-run workspace quota (k8s has `workspaceSizeLimit`), and `maxConcurrent` defaults to 1 on the host and 100 on k8s.
- **Evidence:** (a) `internal/agent/claim_pod.go:213-217`; no duplicate check in `internal/dsl`. (b) `CheckJobMountPaths` runs at `internal/k8sagent/agent.go:392` (`internal/k8sagent/podmountpaths.go`); apply-time `CheckContainerMountPath` (`internal/dsl/mountpath.go:63`) covers only `workspace.mountPath`. (c) `cmd/unified-cd-agent/main.go:101-105`, `internal/k8sagent/config.go:208`.
- **Done when:** `apply` rejects duplicate container names and an absolute volume `mountPath` that k8s would refuse, with tests; for (c) the quota and defaults are either aligned or the difference is documented in `docs/operator-manual/agents.md`, with the decision recorded in the commit message.

### 52. VM tags as a cloud enrollment policy condition

- **Status:** Open (needs design)
- **Impact:** A cloud enrollment policy can match only on account, project or subscription plus role, service account or managed identity. Operators who separate fleets by VM tag or label cannot express that, and must use one role or service account per fleet.
- **Evidence:** No cloud signs VM tags into its identity proof, so the controller cannot read them without cloud read credentials, which the controller deliberately does not hold (`docs/superpowers/specs/2026-11-02-cloud-vm-agent-enrollment-design.md`, non-goals). Tag-write permission is also often broader than IAM-role assignment.
- **Done when:** A design records whether to add tag conditions (and how the controller would get least-privilege read access per cloud), then either implements it with simulated-provider tests and updates `docs/operator-manual/agents.md` ("Limitations"), or records the decision not to.

### 54. Bound the /api/v1/agents/enroll request body before decoding

- **Status:** Open
- **Impact:** The enroll handler decodes the JSON body without a size limit, so an unauthenticated caller can make the controller read an arbitrarily large body. The per-IP rate limit bounds the request count, not the size. This predates cloud enrollment and is shared with the Kubernetes exchange.
- **Evidence:** `handleAgentEnroll` in `internal/controller/api_agent_enrollment.go` passes `r.Body` straight to `json.NewDecoder`; no `http.MaxBytesReader` wraps it.
- **Done when:** The body is wrapped in `http.MaxBytesReader` with a limit well above a signed AWS request (for example 64 KiB) before decoding, an oversized body gets `413`, and a test covers it for both the Kubernetes and cloud paths.
