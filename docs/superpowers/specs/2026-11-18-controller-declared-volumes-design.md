# Controller-declared agent volumes

Status: draft for review (2026-11-18)

## 1. Intent

Agent-owned named volumes (`spec.volumes` in a Job, see
`docs/user-guide/writing-jobs/volumes.md`) are declared today only in each
agent's config file (`internal/config/agent.go` `Volumes`, k8s
`internal/k8sagent/config.go` `Volumes`). There is no flag or environment
variable, the file is read once at startup, and any change needs an edit on
every machine plus a restart.

The user runs build machines grouped by purpose: Unity build machines, iOS
build machines, Android build machines, compile-check machines. Each group
should get its own volume (a Unity volume, an iOS volume, ...), and several
jobs on the same machine should share that volume. Adding a machine to a
group, or adding/changing a volume for a group, should be one change in one
place, with no agent restart.

This design moves the declaration of volumes to the controller: a new
resource kind, applied with `unified-cli apply` or synced by an AppSource,
delivered to running agents chosen by their labels.

## 2. Goals

- A volume is declared once, on the controller, as a resource.
- The resource selects the agents that get the volume by agent labels.
- Running agents (host and Kubernetes) pick up additions, changes and
  removals without a restart.
- The agent config file no longer declares volumes (decided by the user:
  one source of truth, the controller).
- Existing data is kept: a directory or PVC that already exists at the
  volume's location is used as-is, never wiped or recreated.

## 3. Non-goals

- **Cross-machine sharing.** A volume is still one independent copy per
  agent. Three Unity machines hold three separate `unity` volumes; nothing
  is synced between machines. Sharing means jobs on the same machine use the
  same directory (decided by the user: same-machine sharing is enough).
- **Per-agent paths.** The host path is always the default
  `<workspaceDir>/volumes/<name>`. The `path:` override of the file config is
  dropped (the user does not use it).
- **Changing agent labels after enrollment.** Labels stay controller-
  authorized and fixed at enrollment, as today. Moving a machine to another
  group means re-enrolling it (or a later, separate feature).
- **Deleting data.** Removing a volume declaration, or an agent leaving the
  selector, never deletes the directory or PVC (same rule as today's
  "dropping a volume from config deletes nothing").

## 4. Anti-goals

- A running job loses its volume, or sees the volume's mode change under it,
  because someone edited the declaration.
- An agent silently stops offering a volume it was meant to have, so jobs sit
  in `Queued` with no explanation.
- An existing volume directory is emptied, moved or recreated during the
  migration.

## 5. Design

### 5.1 The resource

```yaml
apiVersion: unified-cd/v1
kind: AgentVolume
metadata:
  name: unity-cache          # the volume name jobs use in spec.volumes
spec:
  mode: exclusive            # exclusive | shared (same meaning as today)
  size: 200Gi                # required; host: advisory (warn on overrun); k8s: PVC request
  storageClassName: fast-ssd # optional; k8s only; ignored by host agents
  agentSelector:             # labels an agent must all carry; empty = every agent
    - role:unity             # same []string label syntax as a Job's agentSelector
```

- `metadata.name` is the volume name. Its rules are the intersection of the
  host and k8s rules today: a single path segment and a DNS-1123 label, and
  `<agentName>-<name>` must fit the k8s 63-character claim-name limit for
  every k8s agent that receives it (checked agent-side; see 5.4).
- A volume name is now global: one declaration means the same mode and size
  everywhere it is delivered. This removes today's possibility of the same
  name meaning different things on different agents.
- `agentSelector` uses the same matching as a job's `agentSelector`: every
  selector label must be present on the agent (`selector <@ agent labels`).
  One machine may serve several purposes by carrying several labels (for
  example `unity` and `android`) and being selected by
  several volumes.
- Unknown fields are rejected, as for `kind: Vars`.

Name of the kind (`AgentVolume`, to avoid confusion with a Job's
`spec.volumes` and with Kubernetes volumes) is my call.

### 5.2 Controller side

- New table for the declarations, store methods, and API
  (`POST/GET /api/v1/agent-volumes`, `DELETE /api/v1/agent-volumes/{name}`),
  following the `kind: Vars` pattern end to end: dsl types/parse, schema and
  field-reference generation, authz permissions (read for viewer, write for
  admin), audit actions, `unified-cli apply` / `export`, AppSource sync and
  the managed-resource guard.
- The controller computes each agent's **desired volume set**: every
  declaration whose `agentSelector` matches the agent's authorized labels.
  "Authorized labels" means the labels on the authenticated agent principal
  (`AgentPrincipal.AuthorizedLabels`, from the agent's credential/enrollment),
  the same source register and claim already use for routing. It never comes
  from the agent request body, and it does not depend on the `agents` row, so
  a row re-created by `TouchAgent` cannot produce a spurious empty set. A set
  delivered by the controller is therefore authoritative, including an empty
  one (my call).
- The desired set is returned to the agent in the **heartbeat response**
  (today an empty 204), with a revision/etag so an agent can skip unchanged
  sets. Heartbeats run every 15s, so a change reaches agents within about one
  heartbeat interval. Whether the set also rides on register/claim responses
  is left to the builder. The agent sends its first heartbeat immediately at
  startup, so a fresh agent offers its volumes within seconds; a newly applied
  declaration reaches running agents within one heartbeat (about 15s), which
  is acceptable (my call).
- **Agent → controller status.** The agent reports, in the heartbeat request,
  each desired volume that is not ready and why (see 5.6). Like the storage
  reading, the stored value is replaced on every beat, so an agent that sends
  nothing clears it (my call).
- **Routing stays on what the agent reports as ready**, not on the desired
  set: the agent advertises the volumes it has actually provisioned (as it
  does today in register/claim `Volumes`), and the claim query keeps using
  that. This keeps a k8s PVC that failed to provision from attracting runs.
  The claim path must be able to clear the advertised set to empty (today
  `UpsertAgentOnClaim` ignores an empty list, `internal/store/postgres.go`
  ~1698); fix that as part of this work with an explicit "volumes known"
  signal, so an agent that has not yet received its first set does not wipe
  the stored list.
- **Schedulability** (`GET /api/v1/jobs/{name}/schedulability`) can now tell
  "no AgentVolume named X is declared" apart from "declared, but no matching
  agent has it ready". Applying a Job that names an undeclared volume stays
  allowed (GitOps ordering), as today.

### 5.3 Host agent

- The volume manager becomes mutable: it applies the desired set from each
  heartbeat as a diff, under a lock, replacing today's build-once, read-
  without-lock map (`internal/agent/volumes.go`).
- **Add:** validate the same way startup does today (name, GC-reach rules,
  overlap), `MkdirAll` the default path (an existing directory and its
  contents are kept), add the path to the storage sampler, then advertise
  the volume.
- **Remove:** stop offering the volume to new claims at once; runs already
  holding it keep it until they finish; once no run holds it, forget it
  (stop advertising, drop from the sampler). The directory and its contents
  stay on disk. Link-ownership checks (`ownsLinkTarget`, stale-link sweep)
  must keep recognising links into a volume that is pending removal or was
  removed while a run held it, so cleanup of that run's links still works.
- **Mode change** (`exclusive` ↔ `shared`): stop offering the volume to new
  claims; once no run holds it, switch the mode and offer it again. A run
  never sees the mode change while it holds the volume.
- **Size / storageClassName change:** size takes effect immediately for the
  advisory overrun warning; storageClassName is ignored on host.
- **One bad volume does not block the others.** A delivered volume that fails
  validation or provisioning is reported as not ready with its reason; the
  rest of the set is applied (my call).
- If the controller is unreachable, the agent keeps its last known set (it
  never drops volumes because heartbeats fail). At startup, before the first
  successful heartbeat, the agent offers no volumes; once the set arrives it
  provisions and advertises them.
- `minFreeDisk` stays an agent setting, gating each delivered volume as
  today.

### 5.4 Kubernetes agent

- Applies the same diff. **Add:** create the PVC `<agentName>-<name>` if
  missing (an existing PVC is used as-is, with today's mismatch warning),
  then advertise. **Remove:** stop offering, never delete the PVC (log it as
  orphaned as today). **Mode change:** as host, wait until no pod holds it;
  note an existing PVC's access mode (RWO/RWX) does not change, which today
  only warns — keep that behaviour and document it.
- A name that would exceed the 63-character claim-name limit for this agent
  is not provisioned; the agent reports it (log + the reason surfaced where
  the operator looks, see 5.6).
- Several replicas sharing one `agentName` receive the same set and keep
  using the PVC lock annotation as today.
- **Size or storageClassName change on an existing PVC:** warn only, as
  today; no PVC expansion and no new RBAC (my call). Document that resizing
  an existing PVC is done by the operator in Kubernetes.

### 5.5 Agent config file

- `volumes:` is removed from the host and k8s agent config. An agent whose
  config still contains `volumes:` **refuses to start** with an error that
  names the migration guide (my call: ignoring it would leave jobs queued
  with no explanation, which is an anti-goal).
- Path override (`path:`) disappears with it.

### 5.6 Visibility

- `GET /api/v1/agents` already shows each agent's advertised volumes. Add,
  per agent, volumes that are desired but not ready, with the reason
  (pending removal, waiting for holders to finish a mode change, provisioning
  failed). How this is shown in CLI/Web UI is left to the builder, but an
  operator must be able to answer "why does this Unity machine not have
  `unity-cache`?" without reading agent logs.

### 5.7 Migration

- Migration guide under `docs/operator-manual/migrations/`: for each
  file-declared volume, write an `AgentVolume` with the same name, mode and
  size, and a selector matching the agents that declared it (enroll/label
  agents first if needed); apply it; remove `volumes:` from agent configs;
  restart agents onto the new version. Because the default path is
  unchanged, existing directories and PVCs are picked up with their data.
- Release notes mark this as a breaking change for agents.
- **Upgrade order** (my call): controller first, then agents.
  - Old agent against a new controller: it keeps advertising its
    file-declared volumes at register/claim, and routing uses the agent's
    reported set, so it keeps working until it is upgraded. It ignores the
    heartbeat body.
  - New agent against an old controller: the heartbeat returns 204 with no
    set. The agent offers no volumes and logs a clear warning that the
    controller does not deliver volumes and must be upgraded. No version
    negotiation beyond that.

## 6. Constraints

- Volume exclusion stays enforced on the agent (host in-memory holders, k8s
  PVC annotation); the controller still filters only on reported busy
  volumes.
- Ruling 55 of `docs/superpowers/decisions/2026-10-20-agent-named-volumes-rulings.md`
  (the controller holds no volume definitions) and the "Controller-side
  volume state" non-goal of `2026-10-20-agent-named-volumes-design.md` are
  reversed by this design for definitions only; occupancy stays agent-side.
  Record this in the decisions file.
- English only in the repo. Docs updated in the same change (user guide
  `volumes.md`, `docs/reference/configuration.md`, operator `agents.md`,
  `kubernetes-integration.md`, troubleshooting entries, authorization and
  audit docs, AppSource synced-kinds list, CLI reference).

## 7. Left to the builder

- Exact table schema, API payload shapes, revision mechanism.
- How the desired-but-not-ready state is stored and shown.
- Whether register/claim also carry the desired set.
- Task breakdown and ordering (e.g. controller resource first, then agent
  delivery, then removal of the file config).
