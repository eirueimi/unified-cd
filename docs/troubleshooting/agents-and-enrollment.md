# Agents and Enrollment

## Agent requests fail with 403 `run <id> is claimed by another agent`

**Symptom**

agent requests fail with 403 `run <id> is claimed by another agent`.

**Cause**

the run is owned by a different agent ID (`runs.claimed_by`). Common cause: a stale agent process from before a restart still flushing reports, or two agents configured with conflicting IDs.

**Fix**

restart/retire the stale agent; ensure every agent has a unique ID. The rejected write was not applied.

Separately, outputs (step or run) reported by the *owning* agent after the
run has already been cancelled or completed are not recorded either — this
includes outputs from `finally:` steps of a cancelled run, since those steps
execute after cancellation has already marked the run terminal; the request
gets a 200 `{"alreadyFinalized": true}` response rather than a 403.

## Every agent reports `"version": "dev"`

**Symptom**

`GET /api/v1/agents` (and the web UI's Agents page) shows `"version": "dev"`
for every agent, so a fleet mid-upgrade looks identical to one that is fully
upgraded.

**Cause**

the version is stamped into the binary at build time. A binary built any
other way than a release build — a bare `go build`, or a container image
built from a Dockerfile without `--build-arg VERSION=...` — reports `dev`,
which is the honest answer: that binary has no release identity.

**Fix**

use a released image (`ghcr.io/eirueimi/unified-cd-agent:<tag>`), or pass
`--build-arg VERSION=<tag>` when building the image yourself, or `make build`
for a local binary (it stamps `git describe`). Confirm with
`docker run <image> --version` before rolling it out. Note that the
controller does **not** compare versions and will never reject an old agent
— see [Operations: Checking which version is
running](../operator-manual/operations.md#checking-which-version-is-running).

## Agent enrollment and credentials

### VM agent cannot enroll or refresh

**Symptom:** the agent receives `unauthorized`, `agent identity disabled`, or
`enrollment unavailable` while starting or refreshing.

**Fix:** `unauthorized` means a one-time enrollment credential was malformed,
expired, used, revoked, or a refresh credential is no longer valid. Create a
new enrollment file; plaintext tokens cannot be retrieved from list/get
responses. For a lost or replayed refresh credential, run `unified-cli agent
identity revoke-credentials <agent-id>`, create a new enrollment, and restart
the agent with private `credentialFile` and `enrollmentTokenFile` paths.
After a **full controller database loss** every host agent's credential is
unknown to the controller and its logs show `credential request returned http 401`
with a re-enroll hint: create a new enrollment for the same agent ID and restart the
agent with it (see [Agent recovery contract](../operator-manual/operations.md#agent-recovery-contract)).
Kubernetes agents recover by themselves once their enrollment policy exists again.
`agent identity disabled` requires an administrator to investigate and enable
the identity or replace it. `enrollment unavailable` is retryable only after
PostgreSQL/controller availability is restored; auth fails closed.

### Agent startup reports multiple default credential files

**Symptom:** an ID-less agent start without an enrollment token fails with:

```
multiple default agent credential files found; set --id or --credential-file
```

**Cause:** more than one ID-scoped credential exists below
`$HOME/.unified-cd/`, so the agent cannot safely infer which identity should
run.

**Fix:** restart with `--id <agent-id>` to use that ID's default credential,
or use `--credential-file <path>` to select an explicit file. A valid explicit
enrollment token takes precedence over local discovery and persists its
credential under the returned ID. The former shared
`$HOME/.unified-cd/credential.json` file is not discovered; migrate it or pass
it explicitly with `--credential-file`. See
[Migrating to ID-scoped agent credentials](../operator-manual/migrations/agent-id-scoped-credentials.md).

### Agent enrollment rejects a non-portable VM agent ID

**Symptom:** enrollment creation or default credential-path resolution fails
with:

```
agent ID must use lowercase ASCII letters, digits, '.', '_', or '-', start and end with a letter or digit, and not use a reserved Windows name
```

**Cause:** literal agent IDs are credential directory names, so uppercase,
Unicode, leading or trailing punctuation, separators, and Windows reserved
names could collide or fail on a supported filesystem.

**Fix:** create the VM enrollment with a portable lowercase ID such as
`build-agent-01`. For a pre-existing credential whose embedded ID cannot be
renamed, select its file explicitly with `--credential-file`; implicit
default-path discovery intentionally rejects it.

### Agent rejects a redirected default credential directory

**Symptom:** startup fails with:

```
credential directory must not be a symbolic link or reparse point
```

**Cause:** `$HOME/.unified-cd` or its selected `<agent-id>` directory is a
symbolic link, junction, mount-point reparse path, or another redirecting
filesystem object. Following it could write one agent's credential into
another path.

**Fix:** stop the agent, replace the redirect with a real owner-controlled
directory, and retry enrollment. If an intentionally redirected legacy path
must remain, pass the exact credential file with `--credential-file` and
secure that directory separately.

### Kubernetes agent returns 403 or 503 during enrollment

**Symptom:** `enrollment policy rejected` (403) or `kubernetes identity
unavailable` (503).

**Fix:** A 403 indicates an unrecognized/disabled policy, wrong audience,
ServiceAccount or namespace mismatch, requested label/capability escalation,
or a Pod UID that does not match the live bound Pod. Check the policy and
projected token volume; do not substitute a static Secret. A 503 is retryable:
check the configured cluster verifier, TokenReview permission, bounded Pod
`get` permission, and API-server health. The required audience is
`unified-cd-agent-enrollment`.

### Agent request returns `agent identity mismatch`

**Symptom:** an agent request is rejected with 403 `agent identity mismatch`.

**Fix:** A per-agent access credential was used with another agent's route or
body ID. Start the correct agent identity and do not share credential files.
The controller also preserves the immutable claimed-run ownership check.

### An agent fails at startup with a `volume ...` configuration error

**Symptom:** a host agent or Kubernetes agent whose config declares `volumes:` exits at startup
with one of:

```
volume "go-cache": mode must be "exclusive" or "shared", got ""
volume "unity-main": path /data/ws/working0/unity is under /data/ws/working0, which the workspace GC sweeps (it removes aged directories below /data/ws/working*); choose a path outside any working* directory
duplicate volume name "go-cache" in the agent config
```

and, on a Kubernetes agent:

```
agentName is required when volumes are declared: it prefixes the PVC names, and it must stay the same across restarts or each restart would provision empty volumes
volume "Go_Cache": the name becomes part of a PVC name, so it must be lower-case letters, digits and '-': ...
volume "go-cache": the PVC name "ci-main-go-cache" is 70 characters, over the 63 a label allows; shorten the volume name or agentName
volume "go-cache": size is required on the Kubernetes agent, which provisions a PVC of that size (for example 200Gi)
volume "go-cache": look up PVC ci-main-go-cache: persistentvolumeclaims "ci-main-go-cache" is forbidden: User "system:serviceaccount:unified-cd:unified-cd-k8s-agent" cannot get resource "persistentvolumeclaims" ...
```

**Cause:** the agent validates its volume list before it registers, so a bad entry is a startup
failure rather than a volume that quietly never appears. `mode` is required (`exclusive` or
`shared`). A host path must be absolute and must not be, or sit under, a `working*` directory
(the workspace GC sweeps those) or the agent's `detached` and dot-prefixed directories. On Kubernetes
a volume name must be a DNS label (lower-case letters, digits and `-`), because the PVC is named
`<agentName>-<volume>` and that name is at most 63 characters; a name that is legal on a host
agent can therefore be illegal here. The last error is RBAC: the agent's Role needs `get`, `list`,
`create` and `update` on `persistentvolumeclaims`, and an older Role carried only `create` and
`delete`.

**Fix:** correct the entry and restart the agent. For the RBAC error, upgrade the Helm release (or apply the release bundle) so the Role in
[`charts/unified-cd/templates/agent/rbac.yaml`](https://github.com/eirueimi/unified-cd/blob/main/charts/unified-cd/templates/agent/rbac.yaml) is current (or add the verbs to your own) **before** rolling the agent
image. Never "fix" a too-long or illegal PVC name by changing `agentName` on an agent whose
volumes already hold data: a new `agentName` provisions new, empty PVCs and leaves the old ones
behind. See [Named volumes on
Kubernetes](../operator-manual/kubernetes-integration.md#named-volumes).

### A Kubernetes agent fails at startup with `workspace.pvc was removed`

**Symptom:** the Kubernetes agent exits at startup:

```
/etc/unified-cd/k8s-agent.yaml: podTemplates["golang"].workspace.pvc: workspace.pvc was removed: the workspace is now ephemeral on every agent, and persistence comes from an agent-owned named volume (declare it in the agent's config and mount it with spec.volumes); see docs/operator-manual/migrations/2026-10-20-named-volumes.md
```

**Cause:** an entry under `podTemplates` in the agent config still carries `workspace.pvc`. The
field is gone; this check exists because that file's loader ignores unknown keys, and silently
dropping it would turn a persistent workspace into an ephemeral one.

**Fix:** remove the block and move the persistence to a named volume, following the [migration
guide](../operator-manual/migrations/2026-10-20-named-volumes.md#workspacepvc-is-removed).

## A host agent is not claiming: `free disk space below minimum, skipping claim`

**Symptom**

Runs sit `Queued` although an agent that matches them is connected and heartbeating, and that agent's
log says, once when the agent starts declining (with a reminder at most once a minute while it
stays blocked, and an `INFO` line, `free disk space is no longer below minimum, claiming again`,
when it recovers):

```
level=WARN msg="free disk space below minimum, skipping claim" freeBytes=4203003904 thresholdBytes=5368709120 minFreeDisk=1% thresholdPercent=1 totalBytes=536870912000
```

On `/metrics` the agent shows:

```
unifiedcd_agent_claim_blocked_by_disk{agent="build-agent-01"} 1
```

**Cause**

The free space on the agent's **workspace** filesystem (`workspaceDir`) has dropped below its
`minFreeDisk`, so the agent declines to claim anything until space frees up. `minFreeDisk` is on by
default at `1%` of the filesystem, so you do not have to have set it. Nothing is deleted and no run
fails; the queue just stops draining on that agent. The line tells you the numbers: `freeBytes`
against `thresholdBytes` (the threshold resolved to bytes), `minFreeDisk` is the configured form
and, for a percentage, `thresholdPercent` and `totalBytes` show how it was resolved.
`unifiedcd_agent_workspace_filesystem_free_bytes` against `unifiedcd_agent_min_free_disk_bytes`
shows the same on `/metrics`; the value is reported with the agent's 15s heartbeat, so it can trail
the log line by up to that long.

**How to tell the workspace gate from the volume gate.** The workspace gate stops *all* claiming and
is logged as above, with `unifiedcd_agent_claim_blocked_by_disk` at `1`. The volume gate only keeps
back runs that mount the low volume, is logged as `volume below minFreeDisk, not claiming runs that
mount it` and shows as `unifiedcd_agent_volume_claim_blocked{volume="..."} 1`; see [Run stays Queued:
the volume it mounts is below
`minFreeDisk`](runs-and-scheduling.md#run-stays-queued-the-volume-it-mounts-is-below-minfreedisk).
If only some jobs wait, it is the volume gate.

If the metric `unifiedcd_agent_claim_blocked_by_disk` is `0`, the agent is not declining because of
disk: look at labels, concurrency and volumes instead ([Run stays `Queued`
forever](runs-and-scheduling.md#run-stays-queued-forever)). If the series is absent, the agent is a
Kubernetes agent (which has no workspace gate), is not alive, or predates the metric, so its absence
says nothing about disk. If `unifiedcd_agent_min_free_disk_bytes` is `0`, `minFreeDisk` was set to
`0` / `off` and this check is disabled: a full disk will fail runs instead of pausing claims. (It is
omitted, not `0`, in the first seconds after start while a percentage cannot be resolved yet.)

**Fix**

Free space on the filesystem that holds `workspaceDir`: remove workspaces a crashed run left behind,
prune container images and stopped claim containers, or enable `workspaceRetentionDays`
(see [Operations Guide: Workspace and Claim-Container
Hygiene](../operator-manual/operations.md#workspace-and-claim-container-hygiene)). The agent resumes
claiming on its own at the next check, and `unifiedcd_agent_claim_blocked_by_disk` returns to `0`. If
the threshold itself is too high for this disk (a `1%` default that is larger than the room you
actually keep free, or an absolute value copied from a bigger host), lower it or set `minFreeDisk:
0` and restart the agent; see [Operations: Choosing a
threshold](../operator-manual/operations.md#choosing-a-threshold). If you see `free disk space check
failed, proceeding with claim` instead, the agent could not measure the filesystem and kept
claiming; check that `workspaceDir` exists and is readable.

If the agent fails at startup with `UNIFIED_AGENT_MIN_FREE_DISK: invalid minFreeDisk "..."` (or
`--min-free-disk: ...`, or a `minFreeDisk` error from the config file), the value is malformed:
use a percentage (`1%`), a size (`10GiB`), a byte count or `off`. See the [migration
note](../operator-manual/migrations/2026-10-23-disk-gating-defaults.md).
