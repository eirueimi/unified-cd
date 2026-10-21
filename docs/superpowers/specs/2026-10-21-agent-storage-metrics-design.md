# Agent storage metrics — Design

Date: 2026-10-21
Status: Approved (design); implementation plan to follow
Depends on: `docs/superpowers/specs/2026-10-20-agent-named-volumes-design.md` (named volumes), currently open as PR #176

## 1. Purpose

Jobs fail because an agent runs out of disk, and nothing warns anyone first. The
operator finds out from a failed build.

The ask is narrow and worth stating plainly: **notice that space is running low,
early enough to act.** Not prevent the failure — prevention already exists,
after a fashion, and is a separate question (see §3). Alerting already exists in
the operator's environment; what is missing is a number to alert on.

The named-volumes work makes this sharper. An operator who declares a 200GB
Unity volume has no way to ask how full it is, and a volume that fills is a new
way for a job to fail.

## 2. Goals, non-goals, anti-goals

**Goals**

- An operator can alert on "this agent is running low" before a job fails.
- The numbers reach Prometheus through the controller, because that is the only
  thing the operator's Prometheus can reach.
- A volume's figure has the same meaning on both agent types.
- When an agent stops claiming because of disk, that is visible as a fact rather
  than inferred from an idle queue.

**Non-goals**

- Preventing the failure. `minFreeDisk` is the prevention lever and it already
  exists; this work makes its threshold and its effect visible, nothing more.
- Attributing bytes to a volume ("how much of this filesystem is *this volume*").
  That needs a tree walk; see §4.6.
- Scraping agents directly. The operator's Prometheus cannot reach them.
- Duplicating what kubelet already publishes for PVCs, for operators whose
  Prometheus *can* reach the cluster.
- Giving the Kubernetes agent a disk check it does not have today.

**Anti-goals** — outcomes that make this a failure even if it ships:

- An alert that lies. A dead agent whose last reading said "plenty of space", a
  volume figure so stale it describes last week, or a `0` that means "not
  applicable" rather than "fine". Each converts a silence into a false
  reassurance, which is worse than no metric.
- A measurement that costs more than the problem. Walking a 200GB tree on a
  timer to answer a question `statfs` answers in microseconds.
- A new privilege requirement on the Kubernetes agent.

## 3. Current state

Two disk figures are measured today and neither leaves the agent.

- **Free space before a claim.** `freeBytes(wsBase)` runs before each claim and
  the agent declines to claim when the result is below `MinFreeDisk`
  (`internal/agent/agent.go:431`). The value is used for that comparison and
  discarded. `MinFreeDisk` **defaults to 0, which disables the check entirely**,
  so an operator who has not set it has no protection and no signal.
- **Advisory volume size.** `VolumeManager.CheckUsage(name)` measures a host
  volume's usage and logs a warning on overrun (`internal/agent/volumes.go:658`).
  On a host agent `size` is advisory by design, so that log line is the only
  consumer of the measurement.

What the API exposes: `GET /api/v1/agents` and `GET /api/v1/agents/{agentId}`
return `api.AgentInfo` — id, hostname, os, labels, version, env, lastSeenAt,
capabilities, and the volume *names* the agent declares. No sizes, no usage, no
free space. `/metrics` carries run, step, HTTP, webhook, auth and log counters
and nothing about disk. `api.HeartbeatRequest` carries `ActiveRunIDs` and, for a
Kubernetes agent, pod bindings.

`freeBytes` returns available bytes only. The Windows implementation already
obtains `totalBytes` from `GetDiskFreeSpaceEx` and discards it
(`internal/agent/diskfree_windows.go:18`); the Unix one needs
`stat.Blocks * stat.Bsize` added (`internal/agent/diskfree_unix.go:18`).

## 4. Design

### 4.1 What is measured, and from where

Every figure is a `statfs` of **the filesystem a thing sits on** — never a tree
walk. One concept, two vantage points.

**Host agent**, on each heartbeat (every 15s — `DefaultHeartbeatInterval`,
`internal/agent/heartbeat.go:12`):

- the filesystem containing `workspaceDir`;
- the filesystem containing each declared volume's resolved path. These are
  often the same filesystem as the workspace, because the default volume path is
  `<workspaceDir>/volumes/<name>` — and reporting both is still right, because
  an operator who gave a volume its own `path` on another mount gets the number
  that actually matters for it.

A handful of `statfs` calls every 15 seconds is not a cost worth optimising.

**Kubernetes agent**, when a run mounts a volume: a `statfs` of the mount point
**from inside the claim pod**, which already has the PVC mounted.

The obvious alternative — querying kubelet's stats API — is rejected: it needs
node-level RBAC (`nodes/stats` or equivalent), which is a real privilege
increase for an agent that today touches only pods and PVCs.

`df` cannot be assumed present in a job image. The shim already solves this
class of problem: `/.ucd/ucd-sh` is mounted in every container, so **this adds a
`statfs` subcommand to `ucd-sh`**, alongside the existing `pause` and
`--install`. The image is required to provide nothing.

### 4.2 Which agent reports what

The two agent types do not report the same set, and pretending otherwise would
publish a reassuring zero for something that cannot happen.

| | Host agent | Kubernetes agent |
|---|---|---|
| Workspace filesystem | yes | **no** — it has no `workspaceDir` |
| Per-volume filesystem | yes, every heartbeat | yes, when a run mounts that volume |
| `minFreeDisk` threshold | yes | **no** — the field is host-only (`internal/config/agent.go`) |
| Blocked-from-claiming | yes | **no** — the check lives only in the host claim loop |

A Kubernetes agent **omits** the series it cannot answer; it does not send zero.
An absent series reads as "not applicable"; a zero reads as "fine", and this
design treats a false reassurance as a failure.

### 4.3 How the numbers reach Prometheus

**The heartbeat handler persists the reading; the metrics endpoint reads it back
at scrape time.** Not in-process gauges.

This is forced by the deployment: the controller is stateless and runs several
replicas behind a load balancer. With gauges held in a replica's memory, each
replica would only see the heartbeats the load balancer happened to send it — so
a replica would keep reporting a frozen reading for an agent that now heartbeats
elsewhere, which is precisely the lying alert §2 forbids. Deregistration would
reach only the replica that served the DELETE, and the stale-agent reaper
(`DeleteStaleAgents`, a background loop in `cmd/controller/main.go`) has no hook
into any replica's gauges at all.

The repository already solves this for its one existing per-agent metric:
`unifiedcd_agents` is published by `dbCollector` (`internal/metrics/collector.go`),
a scrape-time collector that reads the database. This work follows that pattern.

Consequences, all of them good:

- **Series disappear by themselves.** They are derived from rows at scrape time,
  so deleting an agent's row deletes its series. No TTL, no expiry bookkeeping,
  no cross-replica coordination.
- **The collector reports only live agents**, reusing the `staleAfter` the
  existing collector already takes. An agent past that window is omitted rather
  than reported with its last number — the same reasoning as the zero above.

Storage: a new migration (**023**; 022 is the latest on the named-volumes
branch) carrying the readings. Whether that is a column on `agents` or a small
side table is the builder's call; the readings are small, bounded by the number
of declared volumes, and rewritten whole on each heartbeat.

### 4.4 The volume list is a snapshot, not an upsert

Each heartbeat carries the agent's **current** view of its volumes, and the
controller replaces what it holds for that agent. A volume dropped from the
agent's configuration therefore loses its series on the next heartbeat.

The consequence worth stating: a Kubernetes agent holds its volume readings only
in memory, so a restart loses them and those series are absent until a run next
mounts each volume. That is the honest outcome — after a restart the agent
genuinely does not know, and §2 prefers an absent series to a stale one.

### 4.5 What the controller exports

Names follow the existing `unifiedcd_` prefix. `agent` is the agent ID.

```
unifiedcd_agent_workspace_filesystem_free_bytes{agent}
unifiedcd_agent_workspace_filesystem_size_bytes{agent}
unifiedcd_agent_volume_filesystem_free_bytes{agent,agent_name,volume}
unifiedcd_agent_volume_filesystem_size_bytes{agent,agent_name,volume}
unifiedcd_agent_volume_measured_timestamp_seconds{agent,agent_name,volume}
unifiedcd_agent_min_free_disk_bytes{agent}
unifiedcd_agent_claim_blocked_by_disk{agent}
```

`min_free_disk_bytes` is the configured threshold, and **0 means the check is
disabled**, which is the default. Publishing it lets an alert be written as
"within N bytes of the point where this agent stops claiming" rather than
against a number the alert author has to keep in step by hand — and it makes the
disabled case visible, which is its own warning.

`claim_blocked_by_disk` is 0 or 1, and is reported by the agent rather than
inferred by the controller: the agent knows it at the moment it declines, and
the controller would otherwise be guessing from a threshold and a reading taken
at different times.

**`agent_name` exists for Kubernetes.** Several k8s-agent replicas sharing a
configuration share their PVCs, which are named `<agentName>-<volume>`
(`internal/k8sagent/config.go`), but each replica has its own enrolled agent ID.
Keying only by ID would give N series for one physical volume, each with its own
age; keying only by name would break the delete-with-the-row rule. Carrying both
keeps the lifecycle correct and gives operators a clean aggregation key —
`min by (agent_name, volume)` over the free-bytes series, with the measurement
timestamp telling them how much to trust it. On a host agent `agent_name` is the
agent's own name and the aggregation is a no-op. The documentation must show
that query, because an operator who alerts on the raw series will get one alert
per replica.

### 4.6 What is deliberately not measured

**How many bytes of a filesystem belong to a particular volume.** Answering it
means walking the tree — tens of gigabytes for the Unity case — and it answers
"which volume is eating the disk", not "am I about to run out". The second
question is the one asked here, and `statfs` answers it. The first is a
reasonable follow-up and should reuse the existing `CheckUsage` walk rather than
a new mechanism.

## 5. Decisions taken in this design

Each was decided here and should be revisited by the builder only with a reason.
Items 4 to 7 came out of the builder check.

1. **Report through the heartbeat.** Forced by the operator's Prometheus
   reaching only the controller, and a good fit for agents that poll outward.
2. **`statfs` only, never a tree walk** (§4.6).
3. **Per-volume figures on the host too**, not only on Kubernetes — they are
   nearly free, and they are the number that matters when an operator gave a
   volume its own `path`.
4. **A `ucd-sh` subcommand rather than `df`**, so the job image is required to
   provide nothing. Kubelet's stats API is rejected over the RBAC increase.
5. **Persist and export at scrape time, not in-process gauges** (§4.3). An
   earlier draft of this design said the heartbeat handler would set gauges;
   that is wrong on a multi-replica controller and would have produced exactly
   the lying alert §2 forbids. The repository's existing `dbCollector` is the
   pattern.
6. **A Kubernetes agent omits the series it cannot answer** rather than sending
   zero (§4.2), because a zero reads as "fine".
7. **The volume list is a snapshot** (§4.4), so a dropped volume loses its
   series, and a restarted k8s agent reports nothing for a volume until a run
   mounts it again.
8. **Series carry both `agent` and `agent_name`** (§4.5), so the lifecycle stays
   correct while replicas sharing one PVC can be aggregated.

## 6. Left to the builder

- Whether `freeBytes` grows a second return value or is joined by a `diskStats`
  function; both platform files already have the data.
- Whether the readings live in a column on `agents` or a small side table, and
  the exact wire shape on `api.HeartbeatRequest`.
- Whether the k8s `statfs` runs at mount time, at teardown, or both, and what
  happens when the exec fails — it must not publish a zero, and it must not
  publish a stale timestamp either.
- Metric names above are a proposal; follow whatever the repository's naming
  review prefers, keeping the `unifiedcd_` prefix.
- A test that the series vanish when an agent's row goes, and one that a
  Kubernetes agent publishes no `claim_blocked_by_disk`. Both are the kind of
  absence nobody notices breaking.

## 7. Documentation obligations

Per `AGENTS.md`, not done until these agree with the code:

- `docs/operator-manual/operations.md` — alerting on these metrics is a new
  operator capability, and the `minFreeDisk`-is-disabled-by-default trap belongs
  here, where someone setting up monitoring will meet it. Include the
  `min by (agent_name, volume)` query and say why the raw series would alert per
  replica.
- `docs/reference/configuration.md` — `minFreeDisk`'s description should say its
  value is published as a metric, and that 0 publishes as 0.
- `docs/troubleshooting/` — an entry for "an agent is not claiming", pointing at
  `unifiedcd_agent_claim_blocked_by_disk`.
- Wherever `/metrics` is documented, list the new series with their labels, say
  which ones a Kubernetes agent omits and why, and say plainly that a Kubernetes
  volume figure refreshes only when a run mounts that volume.
- `docs/user-guide/writing-jobs/volumes.md` — a pointer from the volume `size`
  field to the metric, since `size` on a host agent is advisory and this is how
  an operator sees the real number.
