# AgentVolume

## AgentVolume

Declares a named volume that agents provision for jobs to mount. Agents whose labels match `spec.agentSelector` receive the volume. Jobs refer to it by name in `spec.volumes`.

```yaml
apiVersion: unified-cd/v1
kind: AgentVolume
metadata:
  name: <string>                  # required — DNS-1123 label
spec:
  mode: exclusive | shared        # required
  size: <quantity>                # required — Kubernetes quantity, e.g. 200Gi
  storageClassName: <string>      # optional — storage class for Kubernetes agents
  agentSelector: [<string>]       # optional — labels an agent must all carry, e.g. role:unity
  reclaimPolicy: Retain | Delete  # optional — default Retain
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Volume name. Must be a DNS-1123 label (lowercase letters, digits and `-`, at most 63 characters), which also makes it a single path segment. Jobs refer to the volume by this name. |
| `spec.mode` | string | Yes | `exclusive` allows one run at a time to hold the volume; `shared` allows concurrent runs. |
| `spec.size` | string | Yes | Volume size as a Kubernetes quantity such as `200Gi`. Must be positive. |
| `spec.storageClassName` | string | No | Storage class used for the PVC on Kubernetes agents. |
| `spec.agentSelector` | []string | No | Labels in the same `key:value` syntax as a Job's `agentSelector`. An agent offers the volume only when it carries every listed label. Empty means every agent. Entries must not be empty strings. |
| `spec.reclaimPolicy` | string | No | `Retain` (default) keeps the volume's data when the volume leaves an agent's desired set. `Delete` removes the data after the volume leaves the set and no run holds it. |

> **Warning:** with `reclaimPolicy: Delete`, a mistyped `agentSelector` that stops matching an agent removes that agent's data for the volume. Use `Retain` unless the data is disposable.

Unknown fields are rejected.

### When a change takes effect

- A change to `spec.mode`, or a removal or re-selection (the `AgentVolume` is deleted or its
  `agentSelector` stops matching an agent), applies to **new claims at the next heartbeat** (about
  15 seconds); agents need no restart.
- A **mode switch** and a **reclaim** (`reclaimPolicy: Delete`) are applied only once no run holds
  the volume. A run that holds it finishes normally first.
- A host agent offers no volumes until its first successful heartbeat, which it sends immediately
  at startup; while the controller is unreachable it keeps its last known set.

### Applying and managing

```bash
unified-cli apply -f unity-library.yaml
# => agent volume applied: unity-library
```

- `GET /api/v1/agent-volumes` lists the declared volumes; `DELETE /api/v1/agent-volumes/{name}` removes one.
- An AppSource can sync `AgentVolume` documents. A volume managed by an AppSource cannot be changed or deleted directly (HTTP 409), unless the AppSource allows manual override.
- `unified-cli export` writes volumes to the `agent-volumes/` directory.
- Reading volumes needs `agentvolume.read` (held by every built-in role); applying or deleting needs `agentvolume.write` (admin). Changes are recorded in the [audit log](../../operator-manual/audit.md) as `agentvolume.apply` and `agentvolume.delete`.
