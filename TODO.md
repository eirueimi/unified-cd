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

### 56. Scope the object-store credential that store-credentials hands to job Pods

- **Status:** Open (needs a design)
- **Impact:** `/api/v1/store-credentials` returns the controller's own bucket-wide, non-expiring S3 key to any Pod that presents a projected token from an allowed namespace and ServiceAccount. Whoever obtains it can read and overwrite every run's artifacts and caches. `RequireRunBinding` only narrows which Pod may ask; it does not narrow what the Pod receives, and turning it on by default risks failed uploads during Pod start-up and rolling upgrades.
- **Evidence:** `internal/controller/api_store_credentials.go` (the passthrough comment above `writeJSON`); `RequireRunBinding` defaults to false in `internal/config/controller.go`. Garage, the bundled store, has no STS and only bucket-scoped keys, so a narrower key cannot be minted there.
- **Done when:** A design is agreed and implemented in which a job Pod can reach only its own run's objects (for example artifact and cache transfer proxied through the controller or the agent, or per-run scoped credentials on stores that support them), with the bundled store covered, a migration path for existing installs, and `docs/operator-manual/kubernetes-integration.md` updated.

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

### 57. GitHub App tokens for runs longer than one hour

- **Status:** Open
- **Impact:** A run receives the credential's cached job token with 50 to 60 minutes left and never gets another one. A step that uses the token after it expires fails with `401`, so a long build, a long-running deploy or a run that waits on an `approval` step cannot rely on a GitHub App credential.
- **Evidence:** `handleAgentSecretsFetch` (`internal/controller/api_secrets.go`) answers a `githubApp` credential once per fetch; `JobMinRemaining` in `internal/gittoken/gittoken.go` is 50 minutes. A run fetches its secrets once, so the token it holds is never replaced.
- **Done when:** A run that is still using a GitHub App token after its expiry gets a fresh one (re-fetch on expiry, or a fetch per step), with the hand-out audited each time and a test that a step started after the first token's expiry receives a valid one; `docs/user-guide/resources/git-credential.md` ("Known drawbacks") and `docs/troubleshooting/steps-and-execution.md` drop the one-hour limit.

### 59. Per-job narrowing of GitHub App token repositories and permissions

- **Status:** Open (needs design)
- **Impact:** Every job covered by `jobAccess.folders` receives a token with the credential's full `jobAccess.repositories` and `jobAccess.permissions`. A job that needs only one repository or `contents: read` still gets the whole reach, so narrower access needs one GitCredential per case.
- **Evidence:** `GitCredentialResolver.JobToken` (`internal/controller/gitcred_resolver.go`) always sends the credential's full request; there is no job-side declaration of what a job needs (the credential name is used like a secret name, with no `gitTokens`-style block).
- **Done when:** A design records how a job (or a template parameter) asks for a subset, bounded by `jobAccess` (a job can never widen it), and how the shared token cache keys on the subset; it is then implemented with tests, or the decision not to is recorded; `git-credential.md` and `docs/operator-manual/authorization.md` describe the result.

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

### 55. Host key verification in the git-checkout and rsync-deploy templates

- **Status:** Open
- **Impact:** `templates/git-checkout.yaml` and `templates/rsync-deploy.yaml` run on agents with `StrictHostKeyChecking=no` and `UserKnownHostsFile=/dev/null`, so any host can impersonate the git or deploy server for jobs that use them. The controller-side fetchers verify host keys (GitCredential `knownHosts`); these templates do not.
- **Evidence:** `templates/git-checkout.yaml` (the `GIT_SSH_COMMAND` export) and `templates/rsync-deploy.yaml` (the `-e "ssh ..."` option).
- **Done when:** Both templates take a `known_hosts` input (or secret) and default to strict checking against it, with an explicit opt-out for the old behaviour, the template tests and `templates/README.md` updated, and a new tag cut because external references are pinned.

### 58. Revoke GitHub App tokens when a run ends

- **Status:** Open (needs per-run tokens)
- **Impact:** A token handed to a run stays valid until it expires (up to one hour), even after the run finished or was cancelled, because the token is shared between runs and revoking it would break concurrent runs.
- **Evidence:** `jobAccess` tokens are cached per credential in `internal/gittoken` and reused across runs; no code calls `DELETE /installation/token`.
- **Done when:** Tokens are issued per run (which gives up the sharing that keeps issuing to about one call per credential per 10 minutes, so the rate-limit effect is measured and documented) and revoked at run end, with a test that a finished run's token is rejected by a simulated GitHub; `git-credential.md` ("Known drawbacks") says so.

### 60. Private CA support for GitHub Enterprise Server

- **Status:** Open
- **Impact:** A GitHub App credential for a GHES instance whose certificate is signed by a private CA works only if the CA is already in the controller's system trust store; there is no per-credential or controller flag for a CA bundle.
- **Evidence:** The token source uses the default transport and the system CA pool (`internal/gittoken/githubapp.go`), like cloud enrollment; `docs/user-guide/resources/git-credential.md` ("GitHub Enterprise Server") states the limit.
- **Done when:** A documented option (controller flag or per-credential setting) supplies an extra CA bundle for the GitHub App API calls, with a test against a TLS server using a private CA, and the docs and `docs/reference/configuration.md` updated.

### 61. Other token providers for GitCredential (Azure DevOps, workload identity)

- **Status:** Open (needs design)
- **Impact:** Only GitHub Apps can mint short-lived Git tokens. Azure DevOps, GitLab and cloud workload identity still need long-lived personal access tokens stored as `type: token`.
- **Evidence:** `TokenSource` in `internal/gittoken` is the extension point and has one implementation (`GitHubApp`); `internal/dsl/gitcredential_parse.go` accepts `token`, `sshKey` and `githubApp` only.
- **Done when:** A design picks the next provider and its credential shape (for example Entra workload identity for Azure DevOps), implemented behind `TokenSource` with simulated-provider tests, or records the decision not to; `git-credential.md` documents it.

### 62. AppSource-scoped Vars and a test AppSource

- **Status:** Open (parked by the user; needs design)
- **Impact:** Vars can only be global or per-job, so per-repository image aliases (such as `GO_IMAGE`) cannot be defined by an AppSource: two AppSources defining the same key collide (`vars X defined by both ...`). A "test" AppSource that syncs the same repository as a production one produces identical job names (a job's folder comes from its directory under `spec.path`, so both AppSources yield the same qualified names), and nothing detects two AppSources writing the same job, so they would overwrite each other on every sync.
- **Evidence:** The Vars collision check (`internal/dsl/vars_parse.go`) rejects a key defined by two manifests. `internal/controller/appsource_apply.go` has no cross-AppSource ownership check (per code reading, not reproduced).
- **Done when:** A design covers AppSource-scoped Vars, a destination folder or prefix for an AppSource's jobs, and conflict detection between AppSources; it is then implemented with tests, or the decision not to is recorded.
