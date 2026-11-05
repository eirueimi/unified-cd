# Automated host-agent enrollment: enrollment-only PATs and cloud VM identity

Date: 2026-11-02. Status: design, awaiting approval.

## Intent

Operators want host agents on cloud VMs (including autoscaled ones) to connect to the
controller without a human creating a one-time enrollment token per VM. Today the only way is
to have the provisioning pipeline call `unified-cli agent enrollment create` with a PAT that holds
`agent.manage`. That permission is far broader than the job: it can also disable identities,
revoke credentials, change enrollment policies, and mint tokens carrying any label (which means
creating an agent that can claim any job routed by labels).

Two independent parts, built in this order:

1. **Enrollment-only PATs.** A PAT that can only create host-agent enrollment tokens, and only
   with labels from a list fixed on the PAT. This hardens the existing "provisioner mints one
   token per VM" flow for any platform (on-prem, any cloud).
2. **Cloud VM identity enrollment** for AWS, GCP and Azure. The VM proves who it is with a
   credential its cloud issues and signs; the controller verifies it against an enrollment
   policy and issues an agent access token. No enrollment token and no provisioner PAT are
   needed. This mirrors the existing Kubernetes workload enrollment (projected ServiceAccount
   token + `agent enrollment-policy`).

## Goals

- Part 1: a provisioner credential whose worst case, if stolen, is "can create agents with these
  labels until it expires".
- Part 2: a VM launched with the right cloud identity enrolls itself, with labels bounded by the
  policy, and needs no long-lived secret on disk.
- Both: no change for existing setups unless the new features are used (additive, not breaking).

## Non-goals

- Cloudflare (it has no VM product; the user dropped it).
- VM tags/labels as an authorization condition (decided: not signed into any cloud identity
  document, and tag-write permission is often broader than IAM-role assignment). A later
  version may add it; it would require cloud read credentials on the controller.
- AWS Instance Identity Document (no IAM role in it, and it is replayable forever once leaked).
- Any cloud API call from the controller that needs cloud credentials. The controller holds no
  AWS/GCP/Azure credentials.
- Changing the Kubernetes enrollment behaviour.

## Anti-goals (would make this a failure even if it works)

- A provisioner PAT or a stolen VM identity that can enroll an agent with labels outside its
  bound.
- Being able to take over an existing agent's identity (its agent ID) through either path.
- The controller accepting an identity proof minted for a different service (wrong audience) or
  forwarding a request to an arbitrary URL (SSRF through the AWS path).

## Constraints

- English only, no PII in repo text; docs, examples and templates updated together (AGENTS.md).
- No real cloud accounts are available for verification (user confirmed). All provider
  verification is tested against simulated providers (local JWKS servers, a fake STS endpoint).
  The docs must say plainly that each provider was verified only against simulated responses in
  this release, and ask for reports.
- Reuse what exists: the enrollment-policy model (`store.AgentEnrollmentPolicy` with `Provider`,
  `ProviderConfig`, `SubjectConstraints`, `AllowedLabels`, `RequiredLabels`, `AccessTokenTTL`),
  the `POST /api/v1/agents/enroll` exchange (`internal/controller/api_agent_enrollment.go`,
  `handleKubernetesAgentEnroll`, `authorizedKubernetesRequest`), `IssueExternalAgentAccess`, the
  k8s agent's `KubernetesCredentialSource` (`internal/k8sagent/credentials.go`) as the model for
  an access-token-only source, and `github.com/coreos/go-oidc/v3` (already a dependency) for JWT
  verification.

## Part 1: enrollment-only PATs

Decided with the user: the PAT can only create enrollment tokens, and only with labels from a
fixed range.

- New global-only permission `agent.enroll`: allows `POST /api/v1/agent-enrollments` and nothing
  else. It is in the `admin` bundle. The catalogue goes from 25 to 26 permissions (17
  global-only); every "25" in docs and code comments is updated (`authorization.md`,
  `configuration.md`, `troubleshooting/controller-and-database.md`,
  `contributing/auth-implementation.md`, `internal/authz` catalogue).
- A PAT can carry an **enrollment label bound**: `unified-cli token create <name> --permission
  agent.enroll --enroll-label kind:linux --enroll-label pool:build`. Stored with the PAT
  (new column), shown by `token list`, immutable after creation.
- Authorization of `POST /api/v1/agent-enrollments`:
  - Caller holds `agent.manage`: unchanged behaviour.
  - Caller holds `agent.enroll` but not `agent.manage`: every requested label must be in the
    caller's enrollment label bound, otherwise `403` with an error naming the label. A caller
    with no bound (a session, or a PAT created without `--enroll-label`) may only create tokens
    with no labels. *(My call: fail closed rather than "no bound = any label".)*
  - `--agent-id` is refused for `agent.enroll`-only callers (`403`); the ID is always
    auto-generated. *(My call: an explicit ID could target an existing agent's identity.)*
  - `expiresIn` keeps today's limits (default 10m, max 24h).
- `--enroll-label` is only valid together with `agent.enroll` in the ceiling, and the creator
  must themselves hold `agent.enroll` or `agent.manage`; otherwise `400`, consistent with the
  existing "ceiling above what the creator holds" rule.
- Audit log records the PAT and the labels on each enrollment creation (existing audit entry,
  extended if the labels are not already there).

## Part 2: cloud VM identity enrollment

### Decided with the user

- Runs on VMs in AWS, GCP and Azure.
- A VM is allowed when it matches the policy on **account/project/subscription** together with
  **IAM role / service account / managed identity**. Tags are not used.
- No real-cloud test environment: simulated-provider tests only, documented as such.

### My calls (the builder may refine, not reverse, without coming back)

- Identity proofs:
  - **AWS**: a SigV4-signed `sts:GetCallerIdentity` request made with the instance-profile
    credentials (the Vault "iam" auth method shape). The agent sends method, URL, headers and
    body; the controller checks the URL is an allowed STS endpoint (`sts.amazonaws.com` or
    `sts.<region>.amazonaws.com` for regions listed in the policy; nothing else, to prevent
    SSRF), checks that a header `X-Unified-CD-Audience` is present **and in SignedHeaders** with
    the policy's audience value, forwards the request to STS, and reads the returned ARN
    `arn:aws:sts::<account>:assumed-role/<role>/<session>`. For an EC2 instance profile the
    session name is the instance ID.
  - **GCP**: the metadata server identity token
    (`.../service-accounts/default/identity?audience=<aud>&format=full`), verified as a Google
    ID token (issuer `https://accounts.google.com`, `aud` = policy audience). Claims used:
    `email` (service account), `google.compute_engine.project_id`, `instance_id`, `zone`.
  - **Azure**: a managed-identity access token from IMDS
    (`/metadata/identity/oauth2/token?resource=<aud>`), verified against the tenant's Entra
    signing keys (issuer for the policy's tenant, `aud` = policy audience). Claims used: `tid`,
    `oid` (managed identity object ID), `xms_mirid` (resource ID: subscription, resource group).
- Policy conditions (`SubjectConstraints`), all lists, a match needs one value from each
  non-empty list and at least one identity list must be non-empty:
  - aws: `accountIds`, `roleNames` (or full role ARNs), plus `ProviderConfig`: `audience`,
    `stsRegions`.
  - gcp: `projectIds`, `serviceAccounts`, plus `ProviderConfig`: `audience`.
  - azure: `subscriptionIds`, `resourceGroups` (optional), `managedIdentityObjectIds`, plus
    `ProviderConfig`: `tenantId`, `audience`.
- Agent ID derived from the verified identity, never from the request (Azure: from `xms_mirid` of a system-assigned identity, see "Settled during the builder check"):
  `aws:<account>:<instance-id>`, `gcp:<project>:<instance-id>`, `azure:<subscription>:<vm-id>`.
  It must be unique per VM.
- Labels: same rule as Kubernetes (`authorizedKubernetesRequest`): required labels always,
  requested labels only if in `AllowedLabels`, otherwise reject.
- Credentials: access token only, TTL from the policy (`AccessTokenTTL`, 5m to 4h as for k8s),
  no refresh credential. The agent re-proves its cloud identity before expiry, like
  `KubernetesCredentialSource`. Nothing long-lived is written to disk.
- Agent side: `unified-cd-agent --server ... --enrollment-provider aws|gcp|azure
  --enrollment-policy <name> [--labels ...]`, plus config keys and env vars matching the
  existing naming. Explicit provider; no auto-detection. In this mode `--labels` is the label
  *request* (bounded by the policy), as it is for the k8s agent.
- CLI/API: `agent enrollment-policy create --provider aws|gcp|azure` with provider-specific
  flags; the existing `--cluster` etc. stay for `kubernetes` (the CLI currently hard-codes
  `Provider: "kubernetes"`).
- Errors mirror the k8s path: `403 enrollment policy rejected` for any verification or policy
  mismatch (details only in the controller log and audit, not to the caller); `503 ... identity
  unavailable` when a provider endpoint (JWKS, STS) cannot be reached. Metrics via
  `recordAgentAuth("<provider>", ...)`.
- Rate limiting through the existing `allowAgentCredentialRequest`.

### Known risks to document

- AWS: the session name is chosen by whoever calls `AssumeRole`. The role's trust policy must
  allow only `ec2.amazonaws.com`, or anyone allowed to assume it can enroll with any
  instance-ID-looking session name. The docs must say this; the controller should also require
  the session name to look like an instance ID (`i-[0-9a-f]{8,17}`).
- Azure: with a **user-assigned** managed identity, `xms_mirid` names the identity, not the VM,
  so several VMs share it and a unique per-VM agent ID cannot be derived from the token alone.
  The builder must resolve this (require a system-assigned identity, or use another signed
  source of the VM ID) and document the limitation. This is the least certain provider.
- GCP/Azure tokens are bearer tokens valid up to about an hour; one leaked from a VM can be
  replayed within that hour to act as that VM's agent ID. Same exposure class as the k8s
  projected token; audience binding limits it to this controller.

## Testing

- Unit and handler tests per provider against simulated issuers: a local JWKS/OIDC server for
  GCP and Azure tokens (valid, wrong audience, wrong issuer/tenant, expired, unknown key,
  constraint mismatch), a fake STS server for AWS (valid ARN, wrong account/role, non-instance
  session name, URL not an allowed STS host, audience header missing or unsigned).
- Endpoint overrides needed for tests (STS URL, Google/Entra issuer and JWKS) are test-only
  hooks, not operator configuration.
- Part 1: route tests for `agent.enroll` with labels inside/outside the bound, no bound,
  `--agent-id` refused, ceiling/creator checks, `agent.manage` unchanged.
- An end-to-end test that a host agent in a provider mode enrolls against a simulated provider
  and claims a run.

## Docs to update

`docs/operator-manual/agents.md` (new "Cloud VM enrollment" section and the enrollment-only
PAT for the provisioner flow), `docs/operator-manual/authorization.md` (catalogue, route table,
counts), `docs/reference/cli.md` (`token create --enroll-label`, `enrollment-policy create
--provider`), `docs/reference/configuration.md` (agent flags/env/config), troubleshooting entries
with the exact error strings, `docs/operator-manual/operations.md` if a new operator duty
appears, examples under `examples/config/`, and `TODO.md` for anything deferred (tags, Azure
user-assigned identity if not solved).

## Settled during the builder check

- **ID collisions.** Answered by the project: an explicit VM `--agent-id` must be lowercase
  letters, digits and internal `.`, `_`, `-` (`docs/operator-manual/agents.md`), so it can never
  contain `:` and cannot equal a cloud-derived ID. External identities are bound to
  `(enrollment method, external subject)` and a differing agent ID for the same subject is
  refused (`IssueExternalAgentAccess`, `internal/store/postgres_agent_auth.go:225`). Use the
  provider name as the enrollment method and the derived ID as the subject, exactly like the
  k8s path. Keep it that way; add a test that a cloud-derived ID cannot be requested through
  `enrollment create`.
- **Disabled identities.** Answered by the project: `IssueExternalAgentAccess` returns
  `ErrAgentIdentityDisabled` for a disabled identity; re-proving must keep failing, never
  re-enable. Add a test per provider.
- **Identity churn.** Each new VM is a new identity and identity rows are never pruned. The k8s
  path already has the same growth (one identity per pod UID). Out of scope here *(my call)*;
  add a TODO item covering pruning of external identities unseen for a long time, for both
  k8s and cloud.
- **Azure identity type.** *(My call.)* System-assigned managed identity only in this release;
  the agent ID uses the VM resource from `xms_mirid`. A token whose `xms_mirid` is not a
  `Microsoft.Compute/virtualMachines` (or VM scale set instance) resource is rejected.
  User-assigned identities are deferred to a TODO item.
- **Wire contract.** *(My call.)* Extend the existing `POST /api/v1/agents/enroll`: the body
  already carries `provider` and `policy`; add a provider-specific `proof` object (AWS: method,
  URL, headers, base64 body; GCP/Azure: the token may stay in the `Authorization: Bearer`
  header as for k8s). The k8s request and response stay byte-compatible; older k8s agents keep
  working. An unknown provider is `403 enrollment policy rejected`.
- **AWS replay.** *(My call.)* Accepted, same class as the GCP/Azure bearer tokens: STS itself
  rejects a signature older than about 15 minutes, and the signed audience header binds it to
  this controller. No nonce round-trip.
- **Under-constrained policies.** *(My call.)* Rejected at policy create/update: AWS needs
  `accountIds` and `roleNames`, GCP needs `projectIds` and `serviceAccounts`, Azure needs
  `tenantId`, `subscriptionIds` and `managedIdentityObjectIds`. `audience` is required for all
  three.

## Left to the builder

Exact flag/config/env names beyond those above, the DB migration shape for the PAT label
bound, the internal provider-verifier interface (one per provider behind a common interface is
expected), how the agent fetches each provider's proof (plain HTTP to the metadata endpoints is
fine; avoid pulling a full cloud SDK unless SigV4 signing needs it), and the Azure per-VM ID
resolution described above.
