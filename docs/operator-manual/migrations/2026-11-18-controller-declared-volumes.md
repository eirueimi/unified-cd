# Migrating to controller-declared volumes (`kind: AgentVolume`)

Named volumes used to be declared in each agent's own config file (`volumes:`). They are now a
controller resource, `kind: AgentVolume`: you apply one declaration, and every agent whose labels
match its `agentSelector` receives it in its next heartbeat answer and provisions it. Nothing about
how jobs mount a volume changes: a job still names the volume in `spec.volumes`. See
[`AgentVolume`](../../user-guide/resources/agent-volume.md) for the resource and
[Volumes: Keeping Data Between Runs](../../user-guide/writing-jobs/volumes.md) for the job side.

This is a **breaking change for agents**: an agent whose config still has a `volumes:` key refuses
to start. Controllers are unaffected, and old agents keep working against a new controller until
you upgrade them.

| Before | After |
|---|---|
| Volumes are listed under `volumes:` in each agent's config file (host agent and k8s agent). | Each volume is a `kind: AgentVolume` applied to the controller (`unified-cli apply -f`). |
| A change needs an edit on every agent and a restart of each. | A change is applied once and reaches the agents within one heartbeat (about 15 seconds); no restart. |
| The same name could mean different modes or sizes on different agents. | A volume name is global: one mode and one size wherever it is delivered. |
| `size` and `mode` per agent; `path:` (host agents) could point anywhere. | `spec.mode` and `spec.size` on the resource. The host path is always `<workspaceDir>/volumes/<name>`; there is no `path` field. |
| Which agents have a volume = which configs list it. | `spec.agentSelector`: the labels an agent must all carry (same `key:value` syntax as a Job's `agentSelector`); empty means every agent. |
| Removing a volume from a config never deleted data. | Deleting an `AgentVolume` (or narrowing its selector) never deletes data by default either. `spec.reclaimPolicy: Delete` makes the agent delete the directory or PVC after the volume has been absent from its set for 10 minutes and no run holds it. The default is `Retain`. |
| A job naming an undeclared volume stayed `Queued` with `no agent declares volume "..."`. | The controller now says `no AgentVolume "..." is declared` when no resource exists, which is distinct from "declared, but no matching agent has it ready". |

## Upgrade order

1. **Upgrade the controller first.** It runs the new database migration (`031`) and starts
   delivering volume sets on the heartbeat.
2. **Apply an `AgentVolume` for every volume** (steps below). Until you do, agents that are already
   upgraded offer no volumes.
3. **Then upgrade the agents**, a few at a time if you like.

An **old agent against a new controller keeps working**: it ignores the new heartbeat body, keeps
advertising the volumes from its own config, and the controller routes on what the agent
advertises. A **new agent against an old controller** gets a `204` heartbeat answer with no body;
it offers no volumes and logs one warning:

```
controller does not deliver AgentVolumes; upgrade the controller
```

If you see that line, the controller is older than the agent. Upgrade the controller; the agent
picks up its set on the next heartbeat without a restart.

## Step by step

For each volume listed under `volumes:` in an agent config file:

1. **Write an `AgentVolume`** with the same name, mode and size, and a selector that matches the
   agents that declared it.

   ```yaml
   # before: in the agent config
   volumes:
     - name: unity-main
       mode: exclusive
       size: 200Gi

   # after: applied to the controller
   apiVersion: unified-cd/v1
   kind: AgentVolume
   metadata:
     name: unity-main
   spec:
     mode: exclusive
     size: 200Gi
     agentSelector:
       - role:unity
   ```

   A volume that every agent declared needs no `agentSelector`. A volume only some agents declared
   needs a selector that matches exactly those. `metadata.name` must be a DNS-1123 label (lower-case
   letters, digits and `-`); on a k8s agent that was already the rule.
2. **Make sure the agents carry the labels** the selector names. An agent's labels are the
   authorized labels of its enrollment (see [Agents](../agents.md)); add them first (re-enroll the
   agent if needed), then apply the volume. Check the labels in `GET /api/v1/agents`.
3. **Apply the resources**: `unified-cli apply -f agent-volumes.yaml`. See
   [`examples/resources/agentvolume.yaml`](https://github.com/eirueimi/unified-cd/blob/main/examples/resources/agentvolume.yaml).
4. **Remove `volumes:` from the agent config** (host agent and k8s agent) and **restart the agent**
   on the new version. A `volumes:` key is refused at startup, even as an empty list or a bare key.
5. **Check** `GET /api/v1/agents`: each agent's `volumes` lists what it advertises as ready, and
   `volumeStatus` gives the reason for any volume that is desired but not ready.

Existing data is kept. The default path is unchanged (`<workspaceDir>/volumes/<name>` on a host
agent, the PVC `<agentName>-<name>` on a k8s agent), and an existing directory or PVC is used as it
is, never recreated, so the new agent picks up the data the old one left.

### A host volume with a custom `path:`

There is no `path` field any more: the directory is always `<workspaceDir>/volumes/<name>`. A volume
that used a custom `path:` would otherwise start as a fresh, empty directory and leave the data
behind. **Move the data first**, with the agent stopped (or the volume idle):

```bash
# <path> -> <workspaceDir>/volumes/<name>
mv /srv/ci/unity-main /var/lib/unified-cd/agent/volumes/unity-main
```

or replace the new directory with a symlink to the old location. Do this before the upgraded agent
first receives the `AgentVolume`.

### Kubernetes agents

- The PVCs `<agentName>-<volume>` are reused as they are. Keep `agentName` in the k8s-agent config:
  it still prefixes the PVC names, and a new `agentName` provisions new, empty PVCs.
- The agent logs a warning if an existing PVC's size, storage class or access mode differs from the
  `AgentVolume`, and leaves it alone. It never resizes a PVC.
- A volume whose claim name `<agentName>-<volume>` exceeds 63 characters is reported not ready; the
  others are unaffected.
- `agentName` must now be a valid DNS label (lowercase letters, digits and `-`) even on an agent
  that serves no volumes; any other value is refused at startup (see below).

## `reclaimPolicy`

`spec.reclaimPolicy` is `Retain` (the default) or `Delete`.

- **`Retain`**: when a volume leaves an agent's set (the `AgentVolume` is deleted, or its
  `agentSelector` stops matching that agent), the agent stops advertising it and leaves the data in
  place. Delete the directory or PVC yourself when you are finished with it.
- **`Delete`**: the agent deletes the directory (host) or the PVC (k8s) after the volume has been
  absent from its set for **10 minutes** without interruption **and** no run holds it. A run that
  holds the volume when it leaves the set finishes normally first. A restart between the removal
  and the reclaim still reclaims, and keeps the 10-minute clock. A volume that comes back into the
  set before the reclaim ran is not deleted (its clock starts again from zero the next time it
  leaves), and the controller being unreachable never causes a reclaim.

> **Warning:** with `reclaimPolicy: Delete`, a mistyped `agentSelector` edit deletes the data on
> **every agent that stops matching** unless it is reverted within 10 minutes. The same happens
> after the grace for:
>
> - moving an `AgentVolume` from one AppSource to another (the old source prunes it before the new
>   one adopts it);
> - deleting an `AgentVolume` and re-applying it more than 10 minutes later;
> - restoring the controller database to a state without the declaration (or starting a fresh
>   controller database), which delivers an empty set to every agent;
> - editing an `agentSelector`, or an agent's labels, so that the agent stops matching.
>
> Use `Delete` only for volumes you can rebuild, and leave the default `Retain` while migrating.

See [Operations: Named-Volume Hygiene](../operations.md#named-volume-hygiene).

## What you will see

A host agent whose config still has `volumes:`:

```
agent config: "volumes" is no longer supported; declare volumes on the controller with kind: AgentVolume (see docs/operator-manual/migrations/2026-11-18-controller-declared-volumes.md)
```

A Kubernetes agent:

```
k8s agent config: "volumes" is no longer supported; declare volumes on the controller with kind: AgentVolume (see docs/operator-manual/migrations/2026-11-18-controller-declared-volumes.md)
```

A Kubernetes agent whose `agentName` is not a valid DNS label (the agent logs `invalid config` and
exits; shown here for `agentName: Bad_Name`):

```
agentName "Bad_Name" is not usable in a PVC name: a lowercase RFC 1123 label must consist of lower case alphanumeric characters or '-', and must start and end with an alphanumeric character (e.g. 'my-name',  or '123-abc', regex used for validation is '[a-z0-9]([-a-z0-9]*[a-z0-9])?')
```

A new agent against a controller that does not yet deliver volumes (logged once):

```
controller does not deliver AgentVolumes; upgrade the controller
```

A job that mounts a volume nobody declared stays `Queued`, with the schedulability reason
`no AgentVolume "unity-main" is declared`; see
[Run stays Queued: no agent declares volume](../../troubleshooting/runs-and-scheduling.md#job-stays-queued-no-agent-declares-volume).
The agent startup errors have their own entries in
[Agents and enrollment](../../troubleshooting/agents-and-enrollment.md#a-host-agent-fails-at-startup-with-agent-config-volumes-is-no-longer-supported).

## What did not change

- A job's `spec.volumes` (`name`, `as`, `mountPath`), `UNIFIED_VOLUME_<ALIAS>`, candidate lists and
  `cleanWorkspace` work as before.
- `exclusive` is still enforced by the agent: the controller does not track which run holds a
  volume.
- `minFreeDisk` stays an agent setting and gates each delivered volume as before.
