# Agent Labels and Routing

This document covers `agentSelector` and agent-side label configuration,
which controls which agent executes a given Job, plus the agent's workspace
lifecycle and its registration/liveness semantics with the controller.

## Agent identity and enrollment

New agents authenticate as a controller-issued per-agent principal, not with a
fleet-wide token. Create a VM's one-time enrollment credential as an
administrator, put it in a private file, and configure the agent with
`credentialFile` and `enrollmentTokenFile`. Its access credential is
short-lived; the rotating refresh credential remains in the protected file.

`--id` is optional: the agent adopts its canonical ID from a valid enrollment
token or persisted credential rather than requiring one up front. A valid
explicit enrollment token is authoritative: its enrollment response supplies
the effective ID before the agent looks for a local credential. When `--id` is
supplied, it is asserted — the agent errors out if the enrollment response or
persisted credential disagrees with it, which is useful for catching
copy-paste mistakes.

`credentialFile` is optional. Its default is always
`$HOME/.unified-cd/<agent-id>/credential.json`; the agent creates the
owner-only directory when it persists a credential. With no `--id`, a valid
enrollment token supplies `<agent-id>` and the new credential is saved under
that returned ID. With neither a token nor `--id`, the agent discovers and
uses exactly one ID-scoped default credential. If more than one exists, start
the intended agent with `--id` or `--credential-file`; it does not choose one
arbitrarily. The former shared `$HOME/.unified-cd/credential.json` path is
ignored by default and is used only when explicitly passed with
`--credential-file`. A process never removes another ID's credential.

See [Migrating to ID-scoped agent credentials](migrations/agent-id-scoped-credentials.md)
when upgrading a host that has the former shared credential path.

`unified-cli agent enrollment create` prints the ready-to-run
`unified-cd-agent` command for the new agent; run it directly, or wrap it in
a hand-written service definition — see [Running the agent as a
service](#running-the-agent-as-a-service) below.

`--agent-id` is optional: omit it and the controller auto-generates a canonical
ID (`agent-XXXXXXXX`), returned in the command output — handy for ephemeral or
autoscaling agents. Pass an explicit `--agent-id` when you want a stable,
human-meaningful name. Several auto-generated agents can share a host because
each persisted credential is scoped to its returned ID. For an ID-less,
token-less restart with multiple local credentials, select the intended one
with `--id` or `--credential-file`.

An explicit VM `--agent-id` must be a portable canonical directory name:
lowercase ASCII letters and digits with internal `.`, `_`, or `-`, starting
and ending with a letter or digit. Windows reserved names such as `con`,
`nul.json`, `com1`, and `lpt9.log` are rejected. This single syntax keeps the
literal `$HOME/.unified-cd/<agent-id>/credential.json` layout injective across
case-insensitive and Unicode-normalizing filesystems. Existing credentials
with another ID syntax remain usable only through an explicit
`--credential-file`; they are not implicit default-path candidates.

The token can be supplied either as a file (`--enrollment-token-file`, the
more secure default — nothing sensitive touches shell history or `ps`) or
inline (`--enrollment-token <value>` / `UNIFIED_AGENT_ENROLLMENT_TOKEN`, or
`--enrollment-token -` to read it from stdin). `enrollment create --quiet`
prints only the raw token, so it pipes straight into the agent without ever
hitting a file or the terminal:

```bash
unified-cli agent enrollment create --agent-id agent-1 --label kind:linux --quiet \
  | unified-cd-agent --server https://ci.example.com --enrollment-token -
```

Only one of the file and inline/env/stdin forms may be set at a time; the
agent exits with an error if both are given.

Supplying a **new** enrollment token to an agent that is already enrolled
(has a valid persisted credential) re-enrolls it: the token is exchanged
again, and any authorized labels attached to that token take effect. If the
token has expired or was already consumed (the controller rejects it with
`401`), the agent logs a WARN and keeps running on its existing credential
instead of failing startup — it only refuses to start when it has neither a
usable credential nor a usable token.

Because of this, leaving a **consumed** enrollment token in place (file or env)
makes every later restart log that one WARN before falling back to the
credential. Once the agent has enrolled and written its credential file, you can
remove the enrollment token to keep restarts quiet; re-add a fresh token only
when you want to re-enroll (e.g. to change authorized labels).

Kubernetes agents instead prove their projected ServiceAccount token against a
controller enrollment policy. The controller derives their ID from the verified
cluster, namespace, and Pod UID and assigns their permitted labels. A
requested label is therefore only a request, never a way to gain scheduling
authority. Capabilities are never assigned by the policy: each agent
auto-detects and self-reports its own capabilities — see [Capabilities and
routing](#capabilities-and-routing).

## Running the agent as a service

There is no built-in "install as a service" command — enroll the agent
(above), then point your platform's own service manager at the
`unified-cd-agent` binary directly. Two minimal, hand-written examples:

**systemd** (`~/.config/systemd/user/unified-cd-agent.service`):

```ini
[Unit]
Description=unified-cd Agent (agent-1)
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/unified-cd-agent --server=https://ci.example.com --id=agent-1 --enrollment-token-file=/var/lib/unified-cd-agent/enrollment.token
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Enable with:

```bash
systemctl --user daemon-reload
systemctl --user enable --now unified-cd-agent
```

**launchd** (`~/Library/LaunchAgents/dev.unified-cd.agent.plist`):

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.unified-cd.agent</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/unified-cd-agent</string>
    <string>--server=https://ci.example.com</string>
    <string>--id=agent-1</string>
    <string>--enrollment-token-file=/var/lib/unified-cd-agent/enrollment.token</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
</dict>
</plist>
```

Load with `launchctl load ~/Library/LaunchAgents/dev.unified-cd.agent.plist`.

Both examples omit `--credential-file`: it defaults to
`$HOME/.unified-cd/<agent-id>/credential.json` when unset, so nothing needs to be
customized there for a fresh host. They also deliberately omit `--labels`:
an enrolled agent's labels are fixed at enrollment time (`unified-cli agent
enrollment create --label ...`), and the agent's own `--labels` flag is
ignored for every agent, so passing it to the service would have no effect.

On Windows, use Task Scheduler (or a wrapper such as NSSM or WinSW) to run
`unified-cd-agent.exe` with the same flags at logon/boot; there is no
first-party template for it.

## Agent Labels

Agents announce labels (tags) at startup. The controller uses them for `agentSelector` matching.

```bash
UNIFIED_AGENT_LABELS=kind:linux,pool:build ./bin/unified-cd-agent \
  --server https://controller.example.invalid \
  --credential-file /var/lib/unified-cd-agent/credentials.json \
  --enrollment-token-file /var/lib/unified-cd-agent/enrollment.token

# Or via the --labels flag
./bin/unified-cd-agent --labels kind:linux,pool:build ...
```

### Automatic hostname label

When registering an agent, the controller automatically appends `hostname:<agent-hostname>`
if no explicit `hostname:*` label is present. This lets you pin a job to a specific machine
via `agentSelector` without needing to configure `--labels`.

```yaml
spec:
  agentSelector:
    - hostname:ci-worker-03
```

If the client already supplies a `hostname:*` label, that value takes precedence and
no duplicate is added.

---

## agentSelector

`spec.agentSelector` in a Job is the list of labels that a qualifying agent must have.
The controller uses PostgreSQL's `<@` (contained-by) array operator to check
`agent_selector <@ labels` — i.e. the selector must be a subset of the agent's label
set (`internal/store/postgres.go`) — this is an **AND match** (no OR support).

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: build
spec:
  agentSelector:
    - kind:linux
    - pool:build
  steps:
    - name: build
      run: make build
```

### Parameter expansion

Each element of `agentSelector` supports `{{ .Params.X }}` expansion using the Run's input
parameters (defined in `spec.params.inputs`). The params available at run creation time
(whether triggered via API, webhook, `call:`, or the cron scheduler) are used for expansion.

```yaml
spec:
  params:
    inputs:
      - name: pool
        type: string
        required: true
  agentSelector:
    - "pool:{{ .Params.pool }}"
```

```bash
unified-cli run trigger build --param pool=build-arm64
# → agentSelector is expanded to ["pool:build-arm64"] at runtime;
#   only agents with that label can claim the run
```

All four run-trigger paths route runs identically: each resolves the Run's
params first, then expands `agentSelector` with those params before creating
the Run. Scheduled (cron) runs use the schedule's configured params (falling
back to each input's declared `default`) as the expansion context, the same
as any other trigger.

---

## Capabilities and routing

Alongside labels, every agent advertises a typed `capabilities` list —
`native`, `container`, or `pod` — describing what kind of step execution it
can actually perform. Unlike labels (author-chosen, free-form topology tags),
capabilities are auto-detected and self-reported by the agent at startup (and
on every subsequent registration) — there is no admin flag or enrollment
setting to configure them, so they cannot be mistyped or forgotten by a job
author:

| Capability | Meaning | Reported by |
|---|---|---|
| `native` | Can run a step as a plain host process | Standard agent (always) |
| `container` | Can run a step inside an isolated container | Standard agent (when a container runtime — docker/podman/nerdctl — is detected), Kubernetes agent (always) |
| `pod` | Can build a Kubernetes Pod | Kubernetes agent (always) |

The standard agent reports `["native"]`, or `["native", "container"]` once it
detects a container runtime at startup. The Kubernetes agent reports
`["pod", "container"]`.

### Automatic routing

At trigger time (a direct `POST /runs/trigger`, a webhook delivery, a
`call:` step's child run, or a fired Schedule (cron) trigger — all four go
through the same inference), the controller looks at the job's spec and
works out which capability a run of it needs:

- `spec.native: true` → `native`.
- `spec.podTemplate` is set and uses a feature the standard agent's claim pod
  can't honor at all — a named agent-side template, an `override` patch, a
  pod-spec field beyond `containers`, or a container field outside the
  host-supported set (`volumeMounts`, `securityContext`, `envFrom`, ...) —
  → `pod`.
- `spec.podTemplate` is set and a container's field is host-supported by
  *name* but uses a sub-field the host only partially honors: a
  `resources.requests` entry; a `resources.limits` key other than `cpu`/
  `memory` as a string (an extended resource such as `nvidia.com/gpu`,
  `resources.claims`, or a bare numeric `cpu`/`memory` value); or an `env`
  entry with no literal `value` (i.e. `valueFrom`) → `pod`. See [Kubernetes
  Pod
  Template](../user-guide/writing-jobs/isolation-and-containers.md#kubernetes-pod-template-podtemplate)
  and the [sub-field routing migration
  guide](migrations/podtemplate-subfield-routing.md).
- Everything else (the isolated default, or a host-runnable `podTemplate`) →
  `container`.

That inferred requirement travels with the run and gates claiming. It is
derived from the spec that will actually execute: a run whose `uses:` steps
resolve git templates is re-derived after resolution, because a template can
contribute pod-level fields (such as `volumes`) that make a run Kubernetes-only
which did not look that way as submitted. The requirement therefore gates
claiming: an agent
may only claim a run if its `capabilities` are a superset of the run's
required capability, **in addition to** the existing `agentSelector` label
match — both must pass. In practice this means:

- A `native: true` job is only ever claimed by a standard agent, never by a
  Kubernetes agent (which has no concept of running outside a Pod).
- A `podTemplate` job that only Kubernetes can satisfy (a named template, an
  `override` patch, `resources.requests`, an unqualified `resources.limits`
  key, a non-literal `env` entry, etc.) is only claimed by a Kubernetes
  agent. `podTemplate.workspace.mountPath` and `reuse` are **not** among
  them — the standard agent honors the mount path and treats `reuse` as a
  hint, so neither forces Kubernetes; see [Kubernetes Pod
  Template](../user-guide/writing-jobs/isolation-and-containers.md#kubernetes-pod-template-podtemplate).
  (The `pvc` block under `workspace` that an earlier release also accepted has been removed;
  persistence is now a [named volume](#named-volumes).)
- A `podTemplate` job built entirely from features the standard agent's claim
  pod already supports in full (plain `name`/`image` containers, literal
  `env` values, `resources.limits` restricted to `cpu`/`memory` as strings)
  can be claimed by **either** agent type.

This is automatic — you don't need to hand-write an `agentSelector` just to
keep native jobs off Kubernetes or podTemplate jobs off the standard agent
the way earlier releases required.

**Volumes add a further condition.** A job that mounts [named
volumes](#named-volumes) (`spec.volumes`) is claimed only by an agent that
declares at least one candidate name for every entry, in addition to the
capability and label checks. That is not a capability string: the agent
advertises its volume names separately, as the `volumes` field of its
registration, which the controller matches against each run's
(template-expanded) volume names when the run is queued. Unlike capabilities,
which fall back to a legacy baseline for an agent that reports none (see
Legacy agents below), volumes have no such fallback: an agent that declares
no volumes covers none. A job naming a volume no suitable agent declares
stays `Queued` (see [Job stays Queued: no agent declares
volume](../troubleshooting/runs-and-scheduling.md#job-stays-queued-no-agent-declares-volume)).

**Legacy agents** (an agent binary from before capability routing, or any
agent whose record carries no `capabilities`) are treated as a standard host
agent that advertises `native` and `container`. They keep claiming `native`
and `container` runs by `agentSelector` labels, same as before this feature
shipped, so a rolling upgrade (some agents already on the new binary, some
not) does not strand the runs an old host agent could run correctly. They can
never claim a `pod` run: a record without capabilities can only belong to a
host agent, because the Kubernetes agent has always advertised `pod`, and a
host agent would drop the pod-level fields of a `pod` run (`nodeSelector`,
`volumes`, `initContainers`, `override`, a named template) and still report
success. The same baseline applies to an agent ID that has no record at all.

An agent's record can lose its capabilities after registration: a heartbeat
re-creates a minimal record when something deleted it (for example a
duplicate-ID instance deregistering at the end of a start-before-stop
rollout), and that record has none. Every agent therefore sends its
capability set with each claim poll, and the controller writes it back when
it is non-empty (an empty or missing set never clears what is stored). A
current Kubernetes agent is back to claiming `pod` runs on its next poll. An
agent built before this change sends no set on a claim, so in that situation
it stays on the legacy baseline until it restarts and re-registers; during a
rolling upgrade, upgrade the Kubernetes agents too.

An unknown capability string in an agent's registration request (anything
other than `native`/`container`/`pod`) is rejected with `400` — the agent is
not recorded, rather than silently accepted with a capability set that would
never match anything.

**If no registered agent can satisfy a job's inferred capability and
selector**, the run stays `Queued` indefinitely rather than failing — see
[Job stays Queued / unschedulable
warning](../troubleshooting/runs-and-scheduling.md#job-stays-queued-unschedulable-warning) in the
Troubleshooting guide for how the Web UI surfaces this and how to fix it.

---

## Windows Agents

On Windows hosts, `step.run` is executed via [Git Bash](https://git-scm.com/download/win)
(Windows does not have a native POSIX shell).

### Git Bash not found

At agent startup, the PATH and known install locations (e.g. `C:\Program Files\Git\bin\bash.exe`)
are searched. If Git Bash is not found, **the agent exits with an error at startup**
(previously the failure was only discovered at the first step execution).

```
shell check failed error="git bash not found — install Git for Windows (https://git-scm.com/download/win) or add bash.exe to PATH"
```

Fix: Install [Git for Windows](https://git-scm.com/download/win) or add `bash.exe` to your PATH.

### UNIFIED_AGENT_OS environment variable

Every `step.run` receives the `UNIFIED_AGENT_OS` environment variable (Go's `runtime.GOOS`:
`windows` / `linux` / `darwin`). Job authors can use this to branch on OS.

> **Isolated jobs and scopes always run in Linux, regardless of host OS.** A
> step in an isolated job (the default — see [Job Isolation: `native` and the
> claim pod](../user-guide/writing-jobs/isolation-and-containers.md#job-isolation-native-and-the-claim-pod)) or a `uses:`
> scope step always executes in a Linux container, so `UNIFIED_AGENT_OS` is
> `linux` there even when the agent's host is Windows or macOS
> (`internal/agent/agent_os.go: agentOSForStep`). Only steps in a `native:
> true` job run directly on the host and report the host's `runtime.GOOS`.

```yaml
steps:
  - name: platform-specific
    run: |
      if [ "$UNIFIED_AGENT_OS" = "windows" ]; then
        echo "Running in Git Bash on Windows"
      else
        echo "Running in native Unix shell"
      fi
```

---

## Kubernetes Agent

The sections above are host-agent-centric; the k8s-agent participates in the same
`agentSelector` routing with a few Kubernetes-specific differences:

- It auto-registers a `kubernetes` label in addition to any configured `labels`
  (`internal/k8sagent/agent.go`), so `agentSelector` can target k8s pools without
  extra configuration.
- It supports `call:` steps like the standard agent.
- It runs job/scope steps as Pods and attaches an artifact sidecar
  (`unified-artifact`) for `uploadArtifact`/`downloadArtifact`/`cache` steps.

See [Kubernetes Integration Guide](kubernetes-integration.md) for full setup,
sidecar, and pod-lifecycle details.

---

## Job isolation on the standard agent (claim pod)

By default (unless a job sets `spec.native: true`), the standard agent runs
a claim's steps inside a per-claim "claim pod": a pause container that owns
a network namespace, plus one container per `podTemplate.spec.containers`
entry (and an injected primary container if none is defined), all joined to
the pause container's netns and sharing the claim's per-job workspace via a
bind mount. This mirrors the Kubernetes agent's real Pod: `podTemplate`
sidecars are reachable at `localhost` from a default step, and no host ports
are ever published, so concurrent claims never collide. See [Job Isolation:
`native` and the claim pod](../user-guide/writing-jobs/isolation-and-containers.md#job-isolation-native-and-the-claim-pod)
in the Job Reference for the full model and YAML examples.

**Supported container runtimes:** docker, podman, nerdctl. Apple's
`container` CLI is **not** auto-detected and is **not** supported for
isolated jobs — there is no reliable `--network container:<id>` equivalent
for it, so it can't back a claim pod. It is selectable only via an explicit
`--container-runtime container`, and only for non-isolated `runsIn.image`
steps. macOS hosts run isolated jobs via docker or podman.

**docker-compose deployments:** The bundled compose stacks (repo-root
`docker-compose.yaml` and `deployments/docker/docker-compose.yaml`) run a
Docker-in-Docker (`dind`, `privileged: true`) sidecar so the host agent can
execute isolated jobs, not just `native: true` ones. The agent reaches the
dind daemon via `DOCKER_HOST=tcp://dind:2375` and shares the `ucd-workspaces`
volume with dind at the same path (`UNIFIED_AGENT_WORKSPACE_DIR=/ucd-workspaces`)
so a job container's workspace bind mount — resolved by the dind daemon —
sees the files the agent wrote. The dind daemon listens on plain TCP 2375 on
the compose network; this is for local/dev use, so do not expose it beyond
the compose network.

**Agent flags/config for the claim pod:**

| Flag | Config key | Default | Purpose |
|---|---|---|---|
| `--pause-image` | `pauseImage` | `busybox:1.36`, pinned by digest | Image for the pause (netns-holder) container, one per claim. Unlike the runner, it is not derived from the agent's version. |
| `--runner-image` | `runnerImage` | `ghcr.io/eirueimi/unified-cd-runner:<agent version>` (`:latest` for a build with no release version) | Primary container image injected when the job's `podTemplate` defines none. The default is the runner published with this agent's release; set a digest to pin it (see [Default images and the pause image digest](operations.md#default-images-and-the-pause-image-digest)). |

See [Configuration Reference: Agent Flags](../reference/configuration.md#agent-flags) for
the full flag list.

### Shim installation (`ucd-sh`)

Every claim-pod container (the primary `job` container, `podTemplate`
sidecars), every `uses:`-scope container, and the workspace-cleanup
container needs the `ucd-sh` shim binary to exec into: it's both the
default `run:` interpreter (`/.ucd/ucd-sh -c`) and the keep-alive
(`/.ucd/ucd-sh pause`, replacing `sleep infinity`). Unlike the k8s-agent
(which self-installs the shim into an `emptyDir` via a prepended init
container — see [Kubernetes Integration: `/.ucd` shim
injection](kubernetes-integration.md#ucd-shim-injection)), the standard
agent has a real host filesystem, so it takes a simpler path:

1. **At startup**, before serving any claims, `cmd/unified-cd-agent`'s `main()`
   writes the `ucd-sh` binary embedded in the agent's own binary
   (`internal/shim/embedded`) to
   `<workspace-dir>/.ucd-tools/ucd-sh` (mode `0755`). Keeping `.ucd-tools`
   inside the workspace base makes it visible to remote container runtimes
   that share that base.
2. **Every container the agent creates afterward** — claim-pod containers,
   `uses:`-scope containers, the workspace-cleanup container — bind-mounts
   that tools directory **read-only** at `/.ucd`, the same reserved path the
   k8s-agent uses.
3. **A zero-byte embed is a hard startup failure**, not a first-exec
   surprise. The `ucd-sh` binary is embedded from the committed, generated
   `internal/shim/embedded/ucd-sh-<arch>` bytes (produced by `go generate
   ./internal/shim/embedded/`).
   If that file is missing or truncated, `InstallShim` refuses to start
   rather than let every isolated job fail its first exec with an opaque "no
   such file" error. Regenerate with `go generate ./internal/shim/embedded/`
   and rebuild. The failure is logged and the process exits immediately:

   ```
   install ucd-sh shim failed error="ucd-sh shim is not embedded in this agent binary (0 bytes): the committed internal/shim/embedded/ucd-sh-<arch> is missing or empty — run `go generate ./internal/shim/embedded/` and rebuild before starting the agent"
   ```

This check runs alongside — but independently of — the Windows Git Bash
`RequireShell` check (see [Windows Agents](#windows-agents)): both are
startup-time hard-fails so a misconfigured agent never silently degrades
into a confusing per-step failure. **Native steps are unaffected**: a
`native: true` job's steps still run as plain host processes under host
`bash -lc` (or an explicit `shell:`), never touching `/.ucd` or the shim —
see [Job Reference: `native: true`](../user-guide/writing-jobs/isolation-and-containers.md#native-true-host-process-jobs).

#### Compose development builds

The root `docker-compose.yaml` agent service keeps the repository bind-mounted
at `/app` so Air can watch Go source changes. For every rebuild, Air copies
that source into a disposable container-local `/tmp/unified-cd-agent-src`
tree, excluding `.git`, `tmp`, and both `ucd-sh` embed paths. It prepares and
builds the real shim only in that temporary tree, then writes the resulting
agent executable to `/app/tmp/unified-agent`. Consequently, Compose rebuilds
never write generated shim bytes to
`/app/internal/shim/embedded/ucd-sh-amd64` or
`/app/internal/shim/embedded/ucd-sh-arm64` in the host bind mount.

### Troubleshooting isolated claims

If a claim fails before any step runs — no container runtime found, the
claim pod failed to start (e.g. image pull failure), or workspace
preparation failed — the agent fails the run immediately and writes the
reason as a **"System"** line in the run's own logs (internally `stepIndex
-1`, shown in the Web UI/CLI logs as a system-level entry rather than
attached to any step). Check the run's log output first, even if no step
appears to have started; the actionable error (e.g. "isolated job requires a
container runtime (docker/podman/nerdctl); mark the job native: true or
route it via agentSelector") is there, not just in the agent's own process
log.

### Crash-orphaned claim containers

The claim pod's pause and sidecar containers are started as long-lived
processes (`/.ucd/ucd-sh pause`, not `--rm`) and are torn down by the agent
itself when the claim finishes. If the agent process exits ungracefully
mid-claim — killed, OOM, host reboot — that teardown never runs, and the
pause container plus every podTemplate sidecar for that claim are left
running on the host. Unlike the Kubernetes agent, where an orphaned pod is
eventually reaped by the cluster's own pod garbage collection, **the host
agent has no automatic container GC**: nothing on the host notices or
cleans up an orphaned claim pod on its own. Operators running the host
agent should treat this as routine hygiene — periodically prune
claim-pod-shaped containers (e.g. a `docker container prune`-style sweep,
or one scoped to containers made from `pauseImage`/`runnerImage`/podTemplate
images) rather than assuming the agent will clean up after a crash. A
labelled restart-time sweep — the agent tagging its own claim-pod
containers and removing any stale ones with its own label on startup — is
possible future work, not implemented today.

---

## Bounding a run's cleanup phase (`finallyTimeout`)

A job's [`spec.finally`](../user-guide/writing-jobs/approval-and-finally.md#finally-block-finally)
block, and the `post:`/`cache:` hook drains that follow it, deliberately keep
running after the run is cancelled — that is what they are for. Ignoring the
cancellation also means they cannot inherit the job-level `spec.timeoutMinutes`
deadline, so the agent applies its own ceiling instead:

| Setting | Default | Where |
|---|---|---|
| `--finally-timeout` / `UNIFIED_AGENT_FINALLY_TIMEOUT` / `finallyTimeout` | `10m` | standard agent |
| `finallyTimeout` / `UNIFIED_K8S_FINALLY_TIMEOUT` | `10m` | Kubernetes agent |

Both agents execute the cleanup phases through the same orchestration loop, so
the *semantics* are identical on either backend. The **arithmetic is not**: the
Kubernetes agent carries one more window than the standard agent, because it
releases its claim Pod after that shared loop has returned. See the number to
plan for below — it differs by backend, and the Kubernetes one is the larger.

The budget applies **per phase**, not as one shared total across the run —
because the phases are separated by the main DAG, which may legitimately run
for hours, so a single window opened before it would already have expired by
the time cleanup began.

### The number to plan for

**Size against five × `finallyTimeout` — 50 minutes at the default**, not 10.
That is the Kubernetes agent's number, and it is the one to use for a
`terminationGracePeriodSeconds`, a drain window, a node-drain timeout, or an
alerting threshold. The standard agent's is one window smaller (40m at the
default); sizing it to the larger number is always safe, sizing to the smaller
is not.

The windows, in the order a run enters them:

1. the `post:`/`cache:` hook drain that follows the main `steps` DAG
2. the `finally` pipeline
3. the `post:`/`cache:` hook drain that follows `finally`
4. scope/claim-pod teardown
5. **Kubernetes agent only** — the claim Pod's own deletion, or its return to
   the idle pool. It carries the same `finallyTimeout`, but it runs from the
   k8s agent's claim loop *after* the shared orchestration loop has returned,
   so it cannot share window 4. The standard agent has no equivalent: it tears
   its claim pod down inside window 4.

**Why the Kubernetes number is the one this page leads with.**
`terminationGracePeriodSeconds` is a Kubernetes-only field, so the reader most
likely to act on it is on the agent with five windows. Sizing it to 40m, then
hitting a wedge in window 5, means the kubelet SIGKILLs the agent ten minutes
before the documented ceiling — leaking the very claim Pod that window exists
to clean up. When in doubt, use 50m.

That is the ceiling, not the expectation: reaching it means four or five
separate phases each wedged, and any one of them reaching its own ceiling is
already an incident. A run that hits nothing spends milliseconds here.

The only post-DAG phase **without** a ceiling is the final report to the
controller (`FinishRun`/`SetRunOutputs`), which retries until it lands. That is
deliberate: it runs no job code, it stops immediately on any 4xx (a run the
controller has already reaped or deleted), and if the agent gave up on a
persistent 5xx the run would stay `Running` at the controller forever — the
stuck-run reaper keys on **agent** liveness, and an agent that gave up is still
heartbeating. During a full controller outage the agent's own heartbeats fail
too, so it goes stale and the reaper does take over.

### What you see when a phase is truncated

The run's own **System** stream (not just the agent's log) gains a line naming
the phase and the budget:

```
unified-cd: the finally: phase did not finish: it hit the 10m cleanup budget (finallyTimeout) and was stopped. Work still in flight was interrupted and anything not yet started was skipped.
```

The run's status depends on which phase it was:

| Phase truncated | Run status | Why |
|---|---|---|
| the `finally` pipeline | **`Failed`** | It is author-written pipeline work, and a `finally` step's failure already fails the run. This holds even when every step in the block is `continueOnError: true` — that flag scopes a step's failure; whether the phase finished is not a step's fact to suppress. |
| either hook drain | unchanged | A `post:`/`cache:` hook has never changed the run's status, whatever it fails on. Making "failed because the budget expired" fatal while "failed because the object store was down" stayed benign would be an incoherent rule. |
| scope/claim-pod teardown | unchanged | It runs after the run is already terminal. A leaked container is an agent-host problem, so it is logged on the agent (`scope/pod teardown truncated by its finallyTimeout budget`), not on the run. |

The second row is the one to watch: a truncated drain means a `cache:` save
that did **not** complete, on a run reported `Succeeded`. The System line is
the only thing that says so — alert on it if cache correctness matters to you.

### Setting it

A non-positive value falls back to the 10m default: "unbounded" is deliberately
not expressible, because an unbounded cleanup phase pins the run and the
agent's concurrency slot with nothing able to break it — the stuck-run reaper
keys on **agent** liveness, and the agent keeps heartbeating throughout.

An **unparseable** value is a startup error, not a fallback: `finallyTimeout:
"10 minutes"` in either agent's config file, or `--finally-timeout=10minutes`
on the standard agent, makes the agent exit non-zero at boot. The single
exception is `UNIFIED_AGENT_FINALLY_TIMEOUT`, which is ignored when it does not
parse (matching every other env override on the standard agent) and leaves the
default in place.

Raise it for a fleet whose teardown genuinely takes longer (large cache saves,
a slow rollback). Prefer per-step `timeoutMinutes:` inside `finally` for
anything that can hang individually — the fleet-wide budget is the backstop,
not the primary control.

---

## Workspace lifecycle

A job's workspace is **ephemeral**: it is empty when a run starts and is removed when the run ends,
for `native: true` and isolated jobs alike, and the Kubernetes agent behaves the same way (a Pod's
workspace is an `emptyDir`, and a reused Pod's is emptied before every run). The only thing that
survives from one run to the next is a [named volume](#named-volumes) a job mounts. See
[Volumes](../user-guide/writing-jobs/volumes.md) for the job-author view and the [migration
guide](migrations/2026-10-20-named-volumes.md) if you are coming from an earlier release, where a
host agent kept `working<N>/<job>` between runs.

Each concurrency slot owns one slot directory:
`<workspace-dir>/working<N>`. When no override is supplied, `workspace-dir`
is the agent's startup working directory; override it with `--workspace-dir`,
`UNIFIED_AGENT_WORKSPACE_DIR`, or the `workspaceDir` config key.
Within that slot directory, **every job gets its own
subdirectory**, named after a sanitized form of the job's `metadata.name`:
`working<N>/<sanitized-job-name>`. `run:` steps execute with this per-job
directory as their working directory (bind-mounted at `/workspace` — or
`podTemplate.workspace.mountPath` — into every claim-pod container for an
isolated job), and relative artifact/cache paths resolve against it.

The default also places `.ucd-tools` and, for detached runs, `detached`
directly beneath the startup directory; cleanup never removes that base
directory or unrelated direct children.

`N` ranges from `0` to `--max-concurrent - 1`; each slot's claim loop uses
`working<N>/<job>`, so concurrent slots never share a directory, and two
different jobs sharing a slot over time never mix files either. A directory
is a *place to work*, not a place to keep things: which slot a run lands in
says nothing about what it will find there.

**The agent cleans the workspace on both sides of every run.**

- **Before the run** it removes the directory and recreates it empty. This is the guarantee: a run
  starts empty even if the agent died mid-run last time. If the directory cannot be emptied the
  **run fails** (`prepare workspace failed: clean workspace ...`) instead of starting on another
  run's files; earlier releases warned and proceeded.
- **After the run** it sweeps the directory again, so a run's output does not hold disk until that
  job's next run. This half is best-effort: a failure is a warning in the agent log
  (`workspace sweep after the run failed; the next run cleans it before starting`) and changes
  nothing about the run, because the before-run clean is the guarantee.

Because of that, a job that writes credentials or other secrets to disk no longer exposes them to
the next run of the same job: the next run starts on an empty directory. A **volume** is the
exception, and does persist whatever is written to it; treat a volume's contents as long-lived.

Two knobs concern cleaning, and neither is needed for an ordinary workspace:

- **Agent-level `--clean-workspace` (and the `cleanWorkspace:` config key) does nothing.** Every
  ordinary workspace is already cleaned unconditionally, and the flag never wipes a volume. It is
  kept only so existing agent configs and unit files still start; remove it when convenient.
- **Job-level `spec.cleanWorkspace: true` / `podTemplate.cleanWorkspace: true`** wipe a *volume*
  that serves as the job's workspace (a volume mounted at `"."`). They are the only deliberate way
  unified-cd empties a volume. They change nothing for a job with no volume at the workspace root.
  The two spellings are ORed; `spec.cleanWorkspace` exists because a `native: true` job cannot set
  `podTemplate`. See [Wiping a volume](../user-guide/writing-jobs/volumes.md#wiping-a-volume-cleanworkspace).

**Mode marker.** Ordinary workspaces no longer carry a `.ucd-mode` marker file, and a job that flips
between `native` and isolated simply gets a fresh directory like any other run. A workspace that
*is* a volume keeps its marker **outside** the volume, in `<workspace-dir>/.ucd-volume-modes/<volume>`,
so nothing the agent writes appears in the volume's contents. A flip on such a volume **fails the
run** rather than resetting the directory, because a reset would delete the volume.

**Root-owned files (Linux, rootful docker).** Containers created by rootful
docker write as root inside the bind-mounted workspace; the agent process
(usually non-root) can then fail to clean or overwrite those files
(`EPERM`). When a plain removal fails this way, the agent falls back to a
throwaway root cleanup container (a `busybox` container with the directory
mounted) before recreating the directory. If that also fails, **the run
fails**. **Recommended fix: run rootless podman.** With rootless podman, the
container's root maps to the agent's own user, so this class of problem does
not occur in the first place — it is the first-choice deployment for Linux
hosts running isolated jobs. The same applies to a volume wiped with
`cleanWorkspace`, and a volume's contents are owned by root when a rootful
runtime wrote them, which an operator reclaiming the directory by hand will
need root for.

**Disk usage.** Ordinary workspaces no longer accumulate: each is removed after its run. What can
be left behind is a directory from a run whose agent crashed before the sweep; the opt-in
[`workspaceRetentionDays`](operations.md#workspace-and-claim-container-hygiene) GC reclaims those.
The thing to monitor now is each [named volume](#named-volumes)'s directory, which grows without
bound and is never swept.

**macOS/Windows file-sharing requirement.** For isolated jobs, `wsBase` (the
`--workspace-dir` root) must live under a path the container runtime's file
sharing exposes to containers — e.g. under `/Users` for Docker Desktop on
macOS, or an equivalent shared drive on Windows. A `wsBase` outside the
runtime's shared paths will fail to bind-mount into the claim pod. A volume's `path` is bind-mounted
into the claim pod as well, so the same rule applies to it.

Every `run:` step also receives `UNIFIED_AGENT_OS` (Go's `runtime.GOOS` on
the host for a `native: true` job, but `linux` for an isolated job or a
`uses:` scope regardless of host OS) in its environment, in addition to the
workspace directory as its cwd — see [UNIFIED_AGENT_OS environment
variable](#unified_agent_os-environment-variable) above.

---

## Named volumes

A **named volume** is persistent storage that an *agent* owns and a job mounts by name. It is the
only way data survives between runs, and the operator controls it: the job says only which volume
and where it appears, never how large it is or where it lives. The job-author view is in
[Volumes: Keeping Data Between Runs](../user-guide/writing-jobs/volumes.md); this section is the
operator's side for the **standard (host) agent**. For the Kubernetes agent see [Named volumes on
Kubernetes](kubernetes-integration.md#named-volumes).

Declare volumes in the agent's **config file**. The list is file-only: it has no command-line flag
and no environment variable.

```yaml
# unified-agent.yaml
volumes:
  - name: unity-main
    mode: exclusive
    path: /var/lib/unified-cd/volumes/unity-main   # optional
    size: 200Gi                                    # advisory
  - name: go-cache
    mode: shared
```

| Field | Required | Meaning |
|---|---|---|
| `name` | Yes | What jobs call it. One path segment: `.`, `..` and names containing `/` or `\` are rejected. Names are matched exactly. |
| `mode` | Yes | `exclusive`: at most one run on this agent holds the volume at a time. `shared`: any number of runs may hold it, with **no** write-safety guarantee from unified-cd. |
| `path` | No | Absolute host directory backing the volume. Defaults to `<workspaceDir>/volumes/<name>`. Created at startup if absent. |
| `size` | No | Advisory capacity such as `200Gi`. After each run the agent measures the volume and warns when it exceeds this; it **never enforces** it, because per-directory quotas are not portable across operating systems without root. The free space of the filesystem under the volume is on `/metrics`: see [Watching agent disk headroom](operations.md#watching-agent-disk-headroom). |
| `storageClassName` | — | Kubernetes only; ignored on a host agent. |

The agent refuses to start (`volume "<name>": ...`) on a bad entry: a missing or unknown `mode`, a
duplicate `name`, a relative `path`, a non-positive `size`, or two volumes whose paths overlap.
**A `path` may not be, contain or sit under** the workspace directory's `working*` entries (which
the workspace GC sweeps), the `detached` directory, or any dot-prefixed entry of the workspace
directory (agent tooling). That is enforced rather than left to the spelling of the GC's glob: a
volume deleted for going unused past `workspaceRetentionDays` would destroy exactly the data
declared in order to keep it. The workspace GC never touches a volume.

**How routing works.** The agent advertises its volume names at registration (the `volumes` field
of `GET /api/v1/agents`). A run that mounts volumes is routed only to an agent that declares at
least one candidate for *every* entry. A volume that is declared but busy does not block routing:
the run is claimed and waits for it, holding a concurrency slot (the controller's busy filter makes
this rare, not impossible). When several agents declare the same name they hold **independent
copies**; a run goes to any agent with a free one, and nothing prefers the agent that ran the job
last.

**Things to know.**

- **Removing a volume from the config deletes nothing.** The agent stops advertising it and the
  directory stays on disk. Removing a line from a config file must not destroy tens of gigabytes.
- **Restart recovery.** A host agent treats every volume as free at startup (it lost the runs that
  held them; the controller's lost-claim reconcile handles those runs) and sweeps any volume links a
  crashed native run left behind, and the holder records of processes that are gone.
- **Native jobs reach a volume through a symlink** (a directory junction on Windows when a symlink
  is not permitted), created before the run and removed after it. An absolute mount path is
  therefore an agent-wide exclusive resource: two native runs cannot point the same host path at two
  volumes. See [Mount paths](../user-guide/writing-jobs/volumes.md#mount-paths).
- **`exclusive` holds within one agent process.** The agent keeps the table of who holds a volume
  in memory, so two agent *processes* on one volume directory do not see each other: one agent ID
  started twice with an overlapping restart, or two agents on one machine that share a
  `workspaceDir` (both default to `<workspaceDir>/volumes/<name>`). Give each agent its own
  `workspaceDir`, or its own explicit `path` for the volume, and start each agent ID once. As a
  diagnostic only, an agent records each exclusive volume it holds in
  `<workspaceDir>/.ucd-volume-locks/<volume>/` (outside the volume, so its contents stay exactly
  what jobs put there) and warns in the run's log, and in the agent log, when it finds a record of
  another process that is still running:
  `volume "unity-main" is also held by another agent process`. The run proceeds; the warning
  decides nothing. It can only see agents that share a `workspaceDir`, and it cannot tell two
  agents with different `workspaceDir`s and the same explicit `path` apart.
  See [the troubleshooting entry](../troubleshooting/steps-and-execution.md#a-run-warns-that-a-volume-is-also-held-by-another-agent-process).
- **Draining.** A run still waiting for a volume when the agent starts draining is failed with
  `run abandoned: the agent is draining and the run was still waiting for its volumes`; a drain does
  not wait out a holder that may run for tens of minutes.
- **Usage warnings.** `volume usage exceeds its advisory size; the size is not enforced` is the
  over-`size` warning. `volume usage could not be measured completely: some entries could not be
  read, so the figure is a lower bound` means root-owned files (from a rootful runtime) hid part of
  the tree.
- **Hygiene is yours.** Nothing sweeps a volume. See [Operations: Named-Volume
  Hygiene](operations.md#named-volume-hygiene).

---

## Registration and liveness

On startup, an agent registers with the controller (`POST
/api/v1/agents/register`), sending its ID, hostname, OS, labels, version, and
exposed environment variables. **Registration replaces the stored label set
wholesale** — if a label from a prior registration is absent from the new
`--labels`/`UNIFIED_AGENT_LABELS`/config value, it is dropped. This means
removing a label and restarting the agent actually takes effect, rather than
labels only ever accumulating.

### Agent version

The `version` an agent registers with is the build version stamped into its
binary at compile time (`-ldflags -X .../internal/agent.Version`). It is
stamped by the release binaries (`.goreleaser.yaml`), by `make build`, and by
the `unified-cd-agent` and `unified-cd-k8s-agent` images (from the `VERSION`
build arg that the release workflow feeds from the git tag). Any other build
— including a bare `go build` — reports `dev`.

Read it in `GET /api/v1/agents` (the `version` field, also shown on the
Agents page of the web UI), or straight from a binary or image with
`unified-cd-agent --version` / `docker run <agent-image> --version`. The
controller reports its own version in its first log line and as the
`unifiedcd_build_info{version}` metric — see
[Operations: Upgrades](operations.md#upgrades).

**Nothing compares these versions.** The version is reported for humans, so
an operator can see which part of a fleet is mid-upgrade; it is not a
compatibility check, and a controller will never refuse an agent for being
old. Compatibility is decided by capabilities (above), which is why an agent
reporting no capabilities is treated as a legacy host agent (`native` and
`container`) rather than rejected.

When an agent claims a run (`POST /api/v1/agents/{id}/claim`), the
controller also upserts the agent row, but via a separate, non-destructive
path: it only overwrites hostname/OS/version/env when the claim supplies a
non-empty value, and it **merges** (rather than replaces) labels. This lets
a claim refresh `last_seen_at` and pick up newly-observed labels without
clobbering richer data recorded at full registration time (e.g. the
register-only `hostname:<h>` label — see [Automatic hostname
label](#automatic-hostname-label)). The claim also carries the agent's
capability set and declared volumes, and writes each only when non-empty, so
a record that was re-created without them is repaired on the next poll while
a claim can never blank what registration recorded.

The declared **volumes** follow a fifth rule of their own. Registration
replaces the recorded list wholesale, so dropping a volume from the config
and restarting the agent stops it being advertised. A claim writes the list
**when it supplies one** and leaves the recorded list alone when it does not:
an empty list on a claim means "not supplied", never "none", so a claim can
never wipe what registration recorded. The reason the claim carries the list
at all is that it can recreate an agent row the database lost (for example
after a reset under a running agent), and a recreated row that forgot the
volumes would stop routing volume jobs to an agent that still declares them.

Independent of claim activity, the agent runs a background heartbeat loop
that calls `POST /api/v1/agents/{id}/heartbeat` every 15s
(`agent.DefaultHeartbeatInterval`), refreshing `last_seen_at`. This matters
because a busy agent (all concurrency slots occupied) stops polling for new
claims, and without a heartbeat it could be misidentified as dead. See
[High Availability: Orphaned-Run Recovery](high-availability.md#orphaned-run-recovery)
for how the controller uses `last_seen_at` staleness to reap stuck runs and
delete dead agent rows — that mechanism is documented there, not duplicated
here.

---

## Matrix wire format upgrade note

The release that added `matrix:`/`foreach:` step expansion (see
[Steps](../user-guide/writing-jobs/steps.md#matrix-and-foreach-steps))
changed the claim wire format: the previous per-claim `ForeachKey` /
`ForeachValue` string fields were replaced by a `MatrixValues map[string]string`
field (one entry per matrix dimension; a foreach-sugared step produces a
single-entry map).

**There is no backward-compatibility shim for this change.** An agent
(standard or k8s-agent) running the pre-matrix binary cannot correctly
expand `foreach:`/`matrix:` steps claimed under the new wire format —
**the controller and every agent (standard and k8s-agent alike) must be
upgraded together.** Do not roll out a matrix-capable controller while
older agent binaries are still claiming runs from it.
