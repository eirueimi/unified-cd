# Migrate agents from a shared token to per-agent credentials

New unified-cd installations use a separate identity and short-lived credentials
for every agent. `UNIFIED_TOKEN` remains a human/CLI personal-access-token
bootstrap credential; it is not an agent credential. The only shared-agent
compatibility setting is `UNIFIED_AGENT_LEGACY_TOKEN` (or
`agentAuth.legacySharedToken` in controller YAML), and it is temporary.

> The repository-root Compose files are for development only. A production
> controller must be served through HTTPS. mTLS is not implemented in this
> release; the per-agent principal is deliberately transport-neutral so mTLS
> can be added as future work.

## Before and after

| Before | After |
|---|---|
| One runtime secret was shared by every agent. | Each agent has an independently revocable identity. |
| A VM supplied a static token on startup. | A VM exchanges a one-time enrollment credential, then keeps a protected refresh-credential file. |
| A Kubernetes agent received a Secret/static token. | A Kubernetes Pod proves a projected, audience-bound ServiceAccount token to a policy. |
| Agent labels and capabilities came from the caller. | The controller grants labels and capabilities at enrollment; agents cannot escalate them. |

Credentials are opaque values with prefixes `uca_` (access), `ucr_` (refresh),
and `uce_` (enrollment). Plaintext values are shown only at issuance and are
stored only as SHA-256 hashes. Do not log them, add them to URLs, place them in
process arguments, or commit them.

### Run ownership applies to legacy tokens too

Legacy shared-token agents remain able to connect for the duration of the
compatibility window, but they are subject to the same per-run ownership
checks as enrolled agents. Every write to a run — step reports, log lines,
step/run outputs, sidecar status, and finishing the run — is checked against
that run's `claimed_by`, exactly like a bearer credential's write is. An
agent (legacy or enrolled) that writes to a run it did not claim under the
agent ID it presents now receives `403 run <id> is claimed by another agent`
instead of the write silently succeeding.

Artifact uploads (`PUT /api/v1/runs/{runID}/artifacts/{name}`) are stricter
still: that route has no per-agent path segment for a legacy caller to
present an identity on, so a legacy shared token can never match a run's
`claimed_by` there and every legacy artifact upload is rejected with `403`,
regardless of which run it targets. Migrate the uploading agent to a
per-agent credential (see "Enroll a VM agent" / "Enroll Kubernetes agents"
below) to restore artifact uploads.

## Rollout

Perform these steps in order. Keep the old runtime secret available only for
the short compatibility window.

1. Upgrade the controller with `UNIFIED_AGENT_LEGACY_TOKEN` temporarily set.
   Alternatively set `agentAuth.legacySharedToken` in the controller YAML;
   that YAML value overrides the environment. Do not set an agent token from
   `UNIFIED_TOKEN`.
2. Create VM enrollments and restart VM agents with credential files.
3. Create a Kubernetes policy and roll the agent Deployment with projected
   ServiceAccount tokens.
4. Confirm `unifiedcd_agent_legacy_auth_total` does not increase for one
   complete rollout window.
5. Remove `UNIFIED_AGENT_LEGACY_TOKEN` and the old Secret.
6. Revoke leftover enrollment tokens and inspect active identities.

### Enroll a VM agent

Use an administrator CLI credential to create an enrollment file. The CLI
creates the output file once with owner-only permissions and does not repeat
the credential in list/get output. It does not create missing parent
directories, so create a private state directory first. Run this as the
account that will run the agent; if a dedicated service account runs it,
substitute that account's user and group.

```bash
sudo install -d -m 0700 -o "$(id -un)" -g "$(id -gn)" /var/lib/unified-cd-agent

unified-cli agent enrollment create \
  --agent-id build-linux-01 \
  --label kind:linux \
  --capability container \
  --output-file /var/lib/unified-cd-agent/enrollment.token

unified-cli agent install \
  --server https://controller.example.invalid \
  --id build-linux-01 \
  --credential-file /var/lib/unified-cd-agent/credentials.json \
  --enrollment-token-file /var/lib/unified-cd-agent/enrollment.token
```

The first start exchanges the `uce_` enrollment credential at
`POST /api/v1/agents/enroll`. It receives a one-hour `uca_` access credential
and a 30-day `ucr_` refresh credential. The agent rotates its refresh
credential on every use, preserving a five-minute crash-retry overlap. It
starts refreshing with approximately 15 minutes remaining (with jitter).
Access credentials cannot refresh themselves.

The enrollment lifetime defaults to 10 minutes (`--expires-in`); choose a
short positive duration suitable for the installation window. Do not pass an
enrollment credential with `--token` or `UNIFIED_AGENT_TOKEN`: those are
legacy static-token inputs only.

### Enroll Kubernetes agents

Configure a controller cluster verifier and policy, then deploy the k8s agent
with the projected token volume from the supplied manifests:

```yaml
agentAuth:
  kubernetesClusters:
    - name: in-cluster
  kubernetesEnrollmentPolicies:
    - name: unified-cd-k8s-agents
      cluster: in-cluster
      namespaces: [unified-cd]
      serviceAccounts: [unified-cd-k8s-agent]
      allowedLabels: [kind:kubernetes]
      requiredLabels: [kind:kubernetes]
      capabilities: [pod, container]
      accessTokenTTL: 1h
      enabled: true
```

The supported policy access-token range is 5 minutes through 4 hours; the
default is one hour. A Kubernetes agent posts its projected token to the same
enrollment endpoint with `provider: kubernetes` and a policy name. The
controller performs TokenReview, confirms a live Pod UID, namespace, and
ServiceAccount, and derives the canonical ID
`k8s:<cluster>:<namespace>:<podUID>`. The agent receives an access credential
only, keeps it in memory, rereads its projected token after rotation, and
never receives or persists a refresh credential.

The controller ServiceAccount requires only TokenReview creation and bounded
`get` permission for the enrolled agent Pods. Do not grant cluster-admin and
do not create a k8s-agent credential Secret.

### Verify and retire legacy mode

Check the migration state before removing the compatibility setting:

```bash
unified-cli agent enrollment list
unified-cli agent identity get build-linux-01
unified-cli agent identity disable build-linux-01
unified-cli agent identity enable build-linux-01
unified-cli agent identity revoke-credentials build-linux-01
```

`GET /api/v1/agent-enrollments` returns metadata only. Identity administration
uses `GET /api/v1/agent-identities/{agentId}` and the corresponding
`/enable`, `/disable`, and `/credentials/revoke` POST paths. The admin actions
are audited as `agent.enrollment.create`, `agent.enrollment.revoke`,
`agent.policy.create`, `agent.policy.update`, `agent.policy.delete`,
`agent.identity.enable`, `agent.identity.disable`, and
`agent.credentials.revoke`. Credential plaintext and hashes are never audit
fields.

Monitor `unifiedcd_agent_legacy_auth_total`; it increments only on a
successful legacy shared-token authentication. Once it remains unchanged for
one rollout window, remove the legacy environment/YAML setting, delete the
old Secret, and revoke unused enrollment credentials. The ordinary
`unifiedcd_agent_auth_total` metric has bounded `provider`, `result`, and
`reason` labels only; it never labels agent IDs, credential IDs, Pod UIDs,
subjects, or source addresses.

## Rollback and recovery

Rollback means temporarily restoring the explicit legacy setting while you
correct a rollout problem; it does not mean copying `UNIFIED_TOKEN` into agent
configuration. Keep HTTPS in place in production during rollback. After the
affected agents work, resume the rollout and remove legacy mode again.

| Symptom / response | Meaning and recovery |
|---|---|
| `agent identity mismatch` (403) | A non-legacy credential attempted to use another agent's route, body ID, or claimed run. Use the matching identity; do not reuse a credential. |
| `run <id> is claimed by another agent` (403), including from a legacy shared token | The caller (legacy or enrolled) wrote to a run claimed under a different agent ID, or — for artifact uploads specifically — used a legacy token at all (that route cannot bind a legacy identity to any run). This is expected once compatibility mode is under the per-run ownership guard; migrate the affected agent to a per-agent credential rather than widening the shared token's reach. |
| `agent identity disabled` (403 during enrollment/refresh) or `unauthorized` (401 on normal agent routes) | Enable the identity only after investigation, or create a replacement enrollment. Disabling one identity does not disable other agents. |
| `unauthorized` while first starting a VM | The enrollment credential is malformed, expired, used, or revoked. Create a new one-time enrollment; it cannot be recovered from metadata. |
| `enrollment policy rejected` (403) | The Kubernetes policy, audience, ServiceAccount, namespace, requested labels/capabilities, or bound Pod UID did not match. Correct the policy or deployment; do not weaken it with a static token. |
| `kubernetes identity unavailable` (503) | TokenReview or the configured cluster API was unavailable. Retry after the API is healthy; this is retryable. |
| `enrollment unavailable` (503) or `authentication unavailable` (503) | PostgreSQL is unavailable or a credential operation could not complete. The controller fails closed. Restore database connectivity and retry. |
| `unauthorized` after refresh replay | A refresh credential was already rotated outside its five-minute overlap. Treat it as possible theft or a lost credential; revoke the identity's credentials, create a new enrollment, and reinstall the VM agent. |

For a lost VM refresh file, revoke its identity credentials and issue a fresh
one-time enrollment. Replace both the enrollment file and protected
credential-file path, then restart the agent. A controller restart between
enrollment and refresh is safe because identities and credential hashes live
in PostgreSQL, not controller memory.

## API contract

The public credential endpoints are:

| Path | Credential / result |
|---|---|
| `POST /api/v1/agents/enroll` | `uce_` VM credential or projected Kubernetes ServiceAccount bearer; returns short-lived access and (VM only) refresh credentials. |
| `POST /api/v1/agents/token/refresh` | `ucr_` VM credential only; rotates refresh and returns a new access credential. |
| `POST /api/v1/agent-enrollments` | Administrator creates a one-time VM enrollment. |
| `GET` / `DELETE /api/v1/agent-enrollments[/{id}]` | Viewer lists metadata; administrator revokes. |
| `POST`, `PUT`, `GET`, `DELETE /api/v1/agent-enrollment-policies...` | Administrator manages policies; viewers may read/list. |

Use the CLI policy commands instead of placing Kubernetes credentials in a
policy: `unified-cli agent enrollment-policy create|update|get|list|delete`.
The create/update commands take a configured `--cluster`, repeatable
`--namespace` and `--service-account`, labels/capabilities, an access TTL, and
`--enabled`; they do not accept kubeconfig contents.
