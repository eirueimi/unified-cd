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

`--id` is optional: the agent adopts its canonical ID from the enrollment
token / persisted credential rather than requiring one up front. When `--id`
is supplied, it is instead asserted — the agent errors out if the enrollment
response or persisted credential disagrees with it, which is useful for
catching copy-paste mistakes. Because the ID may not be known until after the
first enrollment, `credentialFile`'s default path is ID-independent — see
below.

`credentialFile` is optional: when unset and `--id` is set, the agent
defaults it to `$HOME/.unified-cd/<id>/credential.json`; when `--id` is
omitted, it defaults to the shared `$HOME/.unified-cd/credential.json`
instead. Either way the agent creates that owner-only directory on startup,
so a fresh VM only needs an enrollment token. Running more than one agent on
the same host without `--id` set therefore collides on that single shared
default path — set `--id` or `--credential-file` explicitly for each agent
in that case. `unified-cli agent enrollment create` prints the ready-to-run
`unified-cd-agent` command for the new agent; run it directly, or wrap it in
a hand-written service definition — see [Running the agent as a
service](#running-the-agent-as-a-service) below.

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
`$HOME/.unified-cd/<id>/credential.json` when unset, so nothing needs to be
customized there for a fresh host. They also deliberately omit `--labels`:
an enrolled agent's labels are fixed at enrollment time (`unified-cli agent
enrollment create --label ...`), and the agent's own `--labels` flag is
ignored for every agent, so passing it to the service would have no effect.

On Windows, use Task Scheduler (or a wrapper such as NSSM or WinSW) to run
`unified-cd-agent.exe` with the same flags at logon/boot; there is no
first-party template for it.

## Table of Contents

- [Running the agent as a service](#running-the-agent-as-a-service)
- [Agent Labels](#agent-labels)
- [agentSelector](#agentselector)
- [Capabilities and routing](#capabilities-and-routing)
- [Windows Agents](#windows-agents)
- [Kubernetes Agent](#kubernetes-agent)
- [Job isolation on the standard agent (claim pod)](#job-isolation-on-the-standard-agent-claim-pod)
- [Workspace lifecycle](#workspace-lifecycle)
- [Registration and liveness](#registration-and-liveness)
- [Matrix wire format upgrade note](#matrix-wire-format-upgrade-note)

---

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
  can't honor (a named agent-side template, an `override` patch, a pod-spec
  field beyond `containers`, or a container field outside what the host
  degrades — see [Kubernetes Pod
  Template](jobs.md#kubernetes-pod-template-podtemplate)) → `pod`.
- Everything else (the isolated default, or a host-runnable `podTemplate`) →
  `container`.

That inferred requirement travels with the run and gates claiming: an agent
may only claim a run if its `capabilities` are a superset of the run's
required capability, **in addition to** the existing `agentSelector` label
match — both must pass. In practice this means:

- A `native: true` job is only ever claimed by a standard agent, never by a
  Kubernetes agent (which has no concept of running outside a Pod).
- A `podTemplate` job that only Kubernetes can satisfy (a PVC-backed
  workspace, a named template, `override`, etc.) is only claimed by a
  Kubernetes agent.
- A `podTemplate` job built entirely from features the standard agent's claim
  pod already supports (plain `name`/`image`/`env`/`resources.limits`
  containers) can be claimed by **either** agent type.

This is automatic — you don't need to hand-write an `agentSelector` just to
keep native jobs off Kubernetes or podTemplate jobs off the standard agent
the way earlier releases required.

**Legacy agents** (an agent binary from before capability routing, or a
freshly-registered agent that reports no `capabilities` at all) are treated
as capability-agnostic: the capability check is skipped for them, and they
match purely on `agentSelector` labels, same as before this feature shipped.
This is deliberate — it means a rolling upgrade (some agents already on the
new binary, some not) never strands a run that an old agent could otherwise
still run correctly.

An unknown capability string in an agent's registration request (anything
other than `native`/`container`/`pod`) is rejected with `400` — the agent is
not recorded, rather than silently accepted with a capability set that would
never match anything.

**If no registered agent can satisfy a job's inferred capability and
selector**, the run stays `Queued` indefinitely rather than failing — see
[Job stays Queued / unschedulable
warning](troubleshooting.md#job-stays-queued--unschedulable-warning) in the
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
> claim pod](jobs.md#job-isolation-native-and-the-claim-pod)) or a `uses:`
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

See [docs/kubernetes-integration.md](kubernetes-integration.md) for full setup,
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
`native` and the claim pod](jobs.md#job-isolation-native-and-the-claim-pod)
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
| `--pause-image` | `pauseImage` | `busybox:1.36` | Image for the pause (netns-holder) container, one per claim. |
| `--runner-image` | `runnerImage` | `ghcr.io/eirueimi/unified-cd-runner:v0.0.3` | Primary container image injected when the job's `podTemplate` defines none. |

See [Configuration Reference: Agent Flags](configuration.md#agent-flags) for
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

1. **At startup**, before serving any claims, `cmd/unified-cd-agent`'s `main()` writes
   the `ucd-sh` binary embedded in the agent's own binary (`internal/shim/
   embedded`) to `<tools-dir>/ucd-sh` (mode `0755`) — `tools-dir` is a
   sibling of `--workspace-dir` (e.g. `--workspace-dir ~/workspace` →
   `~/tools`).
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
see [Job Reference: `native: true`](jobs.md#native-true--host-process-jobs).

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

## Workspace lifecycle

Each concurrency slot owns one slot directory:
`<workspace-dir>/working<N>` (default `~/workspace`, override with
`--workspace-dir`, `UNIFIED_AGENT_WORKSPACE_DIR`, or the `workspaceDir`
config key). Within that slot directory, **every job gets its own
subdirectory**, named after a sanitized form of the job's `metadata.name`:
`working<N>/<sanitized-job-name>`. `run:` steps execute with this per-job
directory as their working directory (bind-mounted at `/workspace` — or
`podTemplate.workspace.mountPath` — into every claim-pod container for an
isolated job), and relative artifact/cache paths resolve against it.

`N` ranges from `0` to `--max-concurrent - 1`; each slot's claim loop always
uses the same `working<N>/<job>` directory for every run of that job, so
concurrent slots never share a directory, and — because the subdirectory is
per-job — two different jobs sharing a slot over time never mix files either
(this also closes the pre-isolation cross-job file mixing within a slot).

**Workspaces are reused (carry over) across runs of the same job.** This is
the default and is unchanged by job isolation: files from a job's previous
run remain in its per-job directory unless cleaned. Two knobs control
cleaning, and they OR together (either one triggers a clean at claim start):

- **Agent-level `--clean-workspace`** (`UNIFIED_AGENT_WORKSPACE_DIR` sibling
  flag) — wipes every job's directory on every claim, agent-wide.
- **Job-level `podTemplate.cleanWorkspace: true`** — wipes only that job's
  directory, on every claim of that job, on **both** agents (the host agent
  now honors the same per-job knob the k8s-agent pool already did).

If your jobs write credentials or other secrets to disk and you don't use
either cleaning knob, delete them in a `finally:` step — otherwise later runs
of the same job (or, without per-job isolation this no longer applies across
*different* jobs) can still read files left in a stale carry-over directory.

> **Security note:** because workspaces persist by default, secrets written
> to disk by one Run of a job are readable by a later Run of **that same
> job** claimed into the same slot, unless cleaning is enabled. Per-job
> directories mean this no longer crosses job boundaries.

**Mode marker (`.ucd-mode`).** Each per-job directory carries a
`.ucd-mode` file recording whether the job last ran `native` or isolated. If
a job's definition flips modes between runs (e.g. `native: true` added or
removed), the directory is reset before the next claim — this closes the one
remaining root-ownership leftover hole described below.

**Root-owned files (Linux, rootful docker).** Containers created by rootful
docker write as root inside the bind-mounted workspace; the agent process
(usually non-root) can then fail to clean or overwrite those files
(`EPERM`). When a plain `os.RemoveAll` clean fails this way, the agent falls
back to a throwaway root cleanup container (`<runtime> run --rm -v
<workDir>:/w busybox rm -rf /w/. ...`) before recreating the directory; if
that also fails, it WARNs and proceeds with whatever is left (the previous,
pre-isolation behavior). **Recommended fix: run rootless podman.** With
rootless podman, the container's root maps to the agent's own user, so this
class of problem does not occur in the first place — it is the first-choice
deployment for Linux hosts running isolated jobs.

**Disk usage is an operator responsibility.** Per-job directories accumulate
under each `working<N>/` over time (one subdirectory per distinct job name
ever run in that slot); there is no automatic workspace GC. Include
`wsBase` in your normal disk-usage monitoring/cleanup.

**macOS/Windows file-sharing requirement.** For isolated jobs, `wsBase` (the
`--workspace-dir` root) must live under a path the container runtime's file
sharing exposes to containers — e.g. under `/Users` for Docker Desktop on
macOS, or an equivalent shared drive on Windows. A `wsBase` outside the
runtime's shared paths will fail to bind-mount into the claim pod.

`--clean-workspace` removes and recreates a job's directory at claim time
(right before a Run starts executing in that slot), not at agent startup —
so the very first Run of a job after the agent starts still runs against
whatever was left in the directory from before, unless it was cleaned by a
previous claim.

Every `run:` step also receives `UNIFIED_AGENT_OS` (Go's `runtime.GOOS` on
the host for a `native: true` job, but `linux` for an isolated job or a
`uses:` scope regardless of host OS) in its environment, in addition to the
workspace directory as its cwd — see [UNIFIED_AGENT_OS environment
variable](#unified_agent_os-environment-variable) above.

---

## Registration and liveness

On startup, an agent registers with the controller (`POST
/api/v1/agents/register`), sending its ID, hostname, OS, labels, version, and
exposed environment variables. **Registration replaces the stored label set
wholesale** — if a label from a prior registration is absent from the new
`--labels`/`UNIFIED_AGENT_LABELS`/config value, it is dropped. This means
removing a label and restarting the agent actually takes effect, rather than
labels only ever accumulating.

When an agent claims a run (`POST /api/v1/agents/{id}/claim`), the
controller also upserts the agent row, but via a separate, non-destructive
path: it only overwrites hostname/OS/version/env when the claim supplies a
non-empty value, and it **merges** (rather than replaces) labels. This lets
a claim refresh `last_seen_at` and pick up newly-observed labels without
clobbering richer data recorded at full registration time (e.g. the
register-only `hostname:<h>` label — see [Automatic hostname
label](#automatic-hostname-label)).

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
[docs/jobs.md: Matrix and Foreach Steps](jobs.md#matrix-and-foreach-steps))
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
