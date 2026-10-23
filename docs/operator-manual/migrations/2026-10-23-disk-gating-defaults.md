# Migrating to disk gating by default

The agent's `minFreeDisk` check used to be **off** unless you set a byte count, and it looked only
at the workspace filesystem. It is now **on by default**, accepts a percentage, also protects
named volumes, and exists on the Kubernetes agent too. An agent that you upgrade without touching
its config starts declining claims when a filesystem is nearly full; on a disk that was already
nearly full, that is the first thing you will see.

## What changed

| # | Before | After |
|---|---|---|
| 1 | `minFreeDisk` defaulted to `0`, which disabled the check. | It defaults to **`1%`** of the filesystem. The check is on unless you set `0` or `off`. |
| 2 | Only a byte count was accepted (`minFreeDisk: 5368709120`). | A percentage (`"1%"`), a size (`"10GiB"`, `"10Gi"`, `"500MB"`) or a byte count. **Byte values keep their meaning.** |
| 3 | `minFreeDisk: 0` and "unset" were the same. | `0` (or `off`) still disables the check, and now it is an explicit choice that beats the default. |
| 4 | A malformed `UNIFIED_AGENT_MIN_FREE_DISK` was silently ignored. | It fails startup with an error naming the source (see below). A malformed `--min-free-disk` or `minFreeDisk:` in the file fails startup as well. |
| 5 | Only the workspace filesystem (`workspaceDir`) was checked, and a low workspace stopped all claiming. | The workspace still gates **all** claiming. In addition, a low **named volume** keeps back only the runs that mount it (they stay `Queued`); other jobs are unaffected. |
| 6 | The Kubernetes agent had no such check. | It has `minFreeDisk` (default `1%`, env `UNIFIED_K8S_MIN_FREE_DISK`) and gates claims on its volumes. It has no workspace gate. |
| 7 | A Kubernetes run's workspace `emptyDir` was unlimited. | **Unchanged by default.** The new `workspaceSizeLimit` (global, or `podTemplates.<name>.workspaceSizeLimit`; env `UNIFIED_K8S_WORKSPACE_SIZE_LIMIT`) is opt-in and unset by default, because a limit that is too small kills legitimate runs. |
| 8 | `unifiedcd_agent_min_free_disk_bytes` was the configured byte count, `0` when unset. | It is the **resolved** threshold in bytes: a percentage is applied to the workspace filesystem's size. `0` means the check is disabled. It is omitted while a percentage cannot be resolved yet (no workspace measurement). |
| 9 | No per-volume series. | New `unifiedcd_agent_volume_claim_blocked{agent,agent_name,volume}` (`1` while the volume is withheld from claims), on both agent types. |

Details: [Configuration Reference](../../reference/configuration.md#agent-config-file),
[Operations: Choosing a threshold](../operations.md#choosing-a-threshold) and [Kubernetes
Integration: Disk limits and headroom](../kubernetes-integration.md#disk-limits-and-headroom).

## What to do before upgrading

1. **Check free space on every agent host.** Run `df -h` on the filesystem that holds each agent's
   `workspaceDir` and on each named volume's `path`. Any filesystem with less than 1% free will
   stop being claimed from (workspace) or stop receiving runs (volume) as soon as the new agent
   starts. Free space first, or choose a lower value.
2. **Set an explicit value** rather than relying on the default, so the number is a decision you
   made. The default is a percentage and scales with the disk; if one run can write more than 1% of
   your disk, pick an absolute size that covers it:

   ```yaml
   # agent.yaml (host agent)
   minFreeDisk: "20GiB"      # or "1%", or a byte count such as 21474836480
   ```

   ```yaml
   # k8s-agent-config.yaml (Kubernetes agent)
   minFreeDisk: "10Gi"       # gates the agent's named volumes
   ```

3. **To keep the old behaviour, opt out explicitly**: `minFreeDisk: 0` (or `off`; also
   `--min-free-disk=0` or `UNIFIED_AGENT_MIN_FREE_DISK=0`; `UNIFIED_K8S_MIN_FREE_DISK=0` on the
   Kubernetes agent). An explicit `0` beats the default and the environment variable.
4. **If you already set a byte count**, nothing changes for the workspace; the same number now also
   applies to every named volume. A value larger than a volume's whole filesystem cannot be met, so
   that volume is not gated and the agent logs one WARN. The workspace filesystem is **not**
   exempted the same way: a byte count larger than the workspace's normal free space stops all
   claiming, so check it against `workspaceDir`'s filesystem too.
5. **Fix any alerts and dashboards that assume `unifiedcd_agent_min_free_disk_bytes == 0` means "never
   configured".** It now means "explicitly disabled"; the PromQL in
   [Watching agent disk headroom](../operations.md#watching-agent-disk-headroom) is updated.
6. **Kubernetes agents: decide on `workspaceSizeLimit`.** Nothing changes until you set it. If you
   do, size it above your largest workspace; a run that exceeds it is evicted (see [Kubernetes run
   fails: pod was evicted](../../troubleshooting/artifacts-and-storage.md#kubernetes-run-fails-pod-was-evicted-usage-of-emptydir-volume-workspace-exceeds-the-limit)).
   Note that the Kubernetes agent's config loader ignores unknown keys, so a misspelled
   `minFreeDisk` or `workspaceSizeLimit` is silently ignored (the 1% default applies, or the
   workspace stays uncapped); copy the exact key names above.

## Startup errors

A malformed value now stops the agent from starting, naming where it came from:

```
UNIFIED_AGENT_MIN_FREE_DISK: invalid minFreeDisk "10 gigs": unknown size suffix "gigs"
--min-free-disk: invalid minFreeDisk "10 gigs": unknown size suffix "gigs"
```

The Kubernetes agent reports `UNIFIED_K8S_MIN_FREE_DISK: invalid minFreeDisk ...` for the
environment variable, and `line N: invalid minFreeDisk ...` for the config file. The accepted forms are
a percentage greater than 0 and less than 100 (`"1%"`, `"0.5%"`), a size (`"10GiB"`, `"10Gi"`,
`"500MB"`), a whole number of bytes, or `0` / `off`. `"100%"` is rejected (it could never be met);
use `off` to disable.

## Rolling back

Downgrading the agent restores the old behaviour: the check is off unless you set a byte count,
and the new keys are no longer read. A **byte count** in `minFreeDisk` is understood by both the old
and the new agent, so keeping that form lets you roll back and forward without editing the file; a
percentage or size string (`"1%"`, `"10GiB"`) is not understood by the old agent, which reads
`minFreeDisk` as an integer and refuses to start on it. To roll back without a config edit, set
the byte-count form (or `0`) before upgrading. A Kubernetes agent downgrade leaves the `minFreeDisk`
and `workspaceSizeLimit` keys in the file unread (its loader ignores unknown keys); pods already
created with a `sizeLimit` keep it until they are replaced. Removing the gate at runtime without
rolling back is `minFreeDisk: 0` and a restart.
