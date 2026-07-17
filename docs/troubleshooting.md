# Troubleshooting

Symptom-indexed fixes for the failures most commonly hit when running unified-cd.

## Run stays `Queued` forever

**Symptom**

A triggered run never leaves the `Queued` status, even though agents are connected:

```
ID:          17c9e93a-7c33-48be-831c-d7b9098ba887
Job:         my-job
Status:      Queued
```

**Cause**

No connected agent satisfies the job's `agentSelector`, or every agent that
does is already at its concurrency limit. Claiming only happens when an
agent's label set is a superset of `agentSelector` (AND match) **and** the
agent has a free concurrency slot.

**Fix**

Check which agents are connected and what labels they advertise:

```bash
unified-cli agent list
# docker-agent-1   c1e136ded609   linux         hostname:c1e136ded609,kind:docker,pool:default   2026-07-04 04:54
# k8s-agent-1      DESKTOP-EMUF6H6 windows/k8s   kubernetes,kind:kubernetes,pool:default,hostname:...    2026-07-04 04:54
```

- Compare the job's `agentSelector` against the label sets above — every
  selector entry must have an exact match on some agent.
- If the labels match but the run is still stuck, the matching agent(s) may
  already be running `--max-concurrent` jobs; start another agent in the pool
  or wait for a slot to free up.
- If the job is called via a `call:` step from another run, check for the
  slot-deadlock case first — see [Calling Other Jobs (`call`)](jobs.md#calling-other-jobs-call).
  A parent run holding its only agent slot while waiting on a same-pool child
  looks identical to this symptom but requires raising `--max-concurrent`
  instead of relabeling agents.

Cancel a run stuck this way with `unified-cli run cancel <run-id>`.

## Job stays Queued / unschedulable warning

**Symptom**

A run stays `Queued` and never gets claimed, and the job's page in the Web
UI shows a warning banner near the top:

```
⚠ This job can't be scheduled right now: no registered agent provides
capability [pod]. Runs will stay Queued until a matching agent registers.
```

**Cause**

This is a stronger, more specific version of the generic ["Run stays Queued
forever"](#run-stays-queued-forever) symptom above. Every job now has an
inferred capability requirement — `native`, `container`, or `pod` — derived
from its spec (see [Capabilities and
routing](agents.md#capabilities-and-routing)), on top of any hand-written
`agentSelector`. The banner means the controller checked the **current
agent inventory** via `GET /api/v1/jobs/{name}/schedulability` and found
that no registered agent satisfies **both**: capabilities ⊇ the job's
required capability, AND labels ⊇ the job's `agentSelector`. Unlike the
generic Queued symptom (which can also just mean "every matching agent is
busy"), this banner only fires when the mismatch is structural — no
currently-connected agent could ever claim this run, busy or not.

The `Reason` field (and banner text) tells you which half failed:

- `no registered agent provides capability [...]` — no connected agent
  reports the needed capability at all (e.g. a `podTemplate` that needs
  Kubernetes, but no k8s-agent is registered; or a `native: true` job with
  only k8s-agents online).
- `no registered agent matches labels [...]` — at least one agent has the
  right capability, but none also carries every label in `agentSelector`.

If `agentSelector` contains a `{{ .Params.X }}` expression, the label half
can't be evaluated from the job definition alone (it only resolves at
trigger time with real parameter values); the banner is suppressed for the
label part and the API response sets `selectorDependsOnParams: true` —
schedulability isn't falsely reported just because a selector is
parameterized.

**Fix**

- Register (or start) an agent that reports the missing capability — a
  standard agent reports `native` (+ `container` with a runtime installed),
  a Kubernetes agent reports `pod` + `container`. See [Capabilities and
  routing](agents.md#capabilities-and-routing) for the full model.
- Or adjust the job: drop an `agentSelector` label that no connected agent
  carries, or change `native`/`podTemplate` so the job's inferred
  requirement matches an agent you actually have (e.g. remove a
  Kubernetes-only `podTemplate` feature so the job infers `container`
  instead of `pod`, letting it run on a standard agent too).
- Legacy agents (pre-upgrade binaries reporting no `capabilities`) are not
  counted against you here — they match by label only, same as before this
  feature shipped, so they don't need to be re-registered just to clear the
  warning.
- Once a satisfying agent registers, the banner disappears on the job page's
  next load and the run is claimed on the matching agent's next poll — no
  need to re-trigger it.

## Job isolation

Jobs are isolated by default (see [Job Isolation: `native` and the claim
pod](jobs.md#job-isolation-native-and-the-claim-pod)); most of the failures below are an
isolation setup gap surfacing as a run failure. If you're migrating an existing job, also see the
[job-isolation migration guide](migration-2026-07-job-isolation.md).

### Run fails immediately: "isolated job requires a container runtime"

**Symptom**

A run fails immediately — no step ever starts — with a **System** log line (`stepIndex -1`):

```
isolated job requires a container runtime (docker/podman/nerdctl); mark the job native: true or route it via agentSelector
```

**Cause**

The job is isolated (no `spec.native: true`) and was claimed by a standard agent whose host has
none of docker, podman, or nerdctl installed. An isolated job needs a container runtime to build
its claim pod; without one, the agent fails the run immediately instead of silently running the
steps on the host (`internal/agent/agent.go`).

**Fix**

- Install docker, podman, or nerdctl on the agent host, or
- Add `spec.native: true` to the job if it's meant to run as host processes, or
- Route the job to an agent that has a runtime via `agentSelector`.

### Run fails immediately on Kubernetes: "native: true jobs are host-only"

**Symptom**

A run fails immediately with:

```
native: true jobs are host-only; the k8s agent cannot run them
```

**Cause**

The job sets `spec.native: true`, but it was claimed by a k8s-agent. `native` opts a job out of
containerization entirely, and the k8s-agent has no concept of running outside a Pod, so it
cannot honor that (`internal/k8sagent/agent.go`).

**Fix**

Route `native: true` jobs away from k8s-agents (and toward host agents with the tools the job
needs) via `agentSelector`.

### Workspace cleaning warnings after a job flips native ↔ isolated

**Symptom**

The agent log shows `workspace clean failed; retrying via cleanup container` and/or `cleanup
container failed; proceeding with dirty workspace`, often right after a job's `native: true` was
added or removed.

**Cause**

Each per-job workspace directory carries a `.ucd-mode` marker recording whether the job last ran
native or isolated; when a job's mode flips, the agent resets the directory before the next claim
(`internal/agent/workspace.go`). This is also where root-owned leftovers can appear: containers
created by **rootful docker** write files as root inside the bind-mounted workspace, which the
agent's own process can't remove. The agent retries via a throwaway root cleanup container; if
that also fails, it **WARNs** and proceeds with whatever is left rather than failing the run.

**Fix**

- Run **rootless podman** on the agent host — the container's root maps to the agent's own user,
  so root-owned leftovers don't occur in the first place.
- If you see the WARN with rootful docker, manually clean the affected per-job workspace
  directory with elevated permissions — see [Workspace lifecycle](agents.md#workspace-lifecycle).

### Stray `ucd-sh pause` containers on an agent host after an agent crash

**Symptom**

`docker ps` (or `podman ps`) on an agent host shows pause and/or sidecar containers still running
`/.ucd/ucd-sh pause` long after the runs that created them finished — typically noticed after the
agent process was killed, OOM'd, or the host rebooted. (Older agent versions ran `sleep infinity`
instead — same symptom, different command.)

**Cause**

This is expected, not a bug. Claim pod containers are long-lived (`/.ucd/ucd-sh pause`, not
`--rm`) and are torn down by the agent itself when a claim finishes; if the agent exits ungracefully
mid-claim, that teardown never runs. Unlike the k8s-agent, whose orphaned pods are eventually
reaped by the cluster's own pod garbage collection, **the host agent has no automatic container
GC** — see [Crash-orphaned claim containers](agents.md#crash-orphaned-claim-containers).

**Fix**

Treat this as routine hygiene: periodically prune claim-pod-shaped containers on agent hosts
(e.g. a `docker container prune`-style sweep, or one scoped to containers made from the
`pauseImage`/`runnerImage`/podTemplate images), rather than assuming a crash cleans up after
itself.

---

Compile-time migration errors — removed step-level `runsIn:`, `native: true` combined with
`podTemplate`, `native: true` combined with a step `container:` — are cataloged in the
[migration guide's validation error
table](migration-2026-07-job-isolation.md#validation-errors-you-may-see-after-upgrading).

## Webhook returns 401

**Symptom** — one of these `signature verification failed: …` messages:

```
signature verification failed: secret "<name>" not found — create it with ...
signature verification failed: secret "<name>" is empty — set a non-empty value ...
signature verification failed: missing X-Hub-Signature-256 header — GitHub sends it only when ...
signature verification failed: X-Hub-Signature-256 does not match — the "<name>" secret differs ...
```

**Cause**

The receiver's `spec.auth.type` is `hmac-sha256` or `github`, and signature
verification failed. The message names the specific reason:

- **`secret "<name>" not found`** — no secret with that `secretRef` exists.
- **`secret "<name>" is empty`** — the secret exists but its value is empty.
  This commonly happens when the value was piped in without one (e.g.
  `echo | unified-cli secret set <name>`), or set with an empty string.
- **`missing … header`** — no signature header arrived. For GitHub, this means
  the webhook has **no Secret configured** (GitHub only sends
  `X-Hub-Signature-256` when a Secret is set).
- **`… does not match`** — a signature arrived but the HMAC differs: the stored
  secret differs from the sender's, or the raw body was altered in transit.

**Fix**

- Set the secret with the **two-argument form**, which does not add a trailing
  newline, and use the *exact same value* on the sender:
  ```bash
  unified-cli secret set <name> '<value>'
  ```
  Avoid `echo "<value>" | unified-cli secret set <name>` — `echo` appends a
  `\n`, so the stored secret won't match the sender's. Use `echo -n` if you must
  pipe.
- For GitHub receivers, set the webhook **Secret** field to that same value and
  set **Content type** to `application/json` (a form-encoded body is signed the
  same on both sides but changes the bytes the receiver hashes).
- `hmac-sha256` receivers accept either `X-Signature: sha256=<hex>` or the
  GitHub-compatible `X-Hub-Signature-256: sha256=<hex>`; `github` receivers only
  check `X-Hub-Signature-256`. Confirm the sender uses the expected header.
- The signature must be computed over the **exact raw request body bytes** —
  re-encoding the JSON (key order, whitespace) before signing produces a
  different HMAC and this same error.
- To isolate whether the stored secret is the problem, sign a test body with the
  value you *think* is stored and POST it directly; if that succeeds, the
  mismatch is on the sender's side:
  ```bash
  SECRET='<value-you-think-is-stored>'
  BODY='{"ref":"refs/heads/main"}'
  SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | sed 's/^.* //')"
  curl -i -X POST http://<controller>/webhook/<name> \
    -H 'Content-Type: application/json' -H "X-Hub-Signature-256: $SIG" -d "$BODY"
  ```
- See [Resource Reference: WebhookReceiver](resources.md#webhookreceiver) for
  the full auth field table and delivery response codes.

## Webhook returns 400 `invalid JSON payload`

**Symptom**

```
invalid JSON payload
```

A GitHub delivery fails with `400` even though the `Secret` is correct (the
signature check passed).

**Cause**

The receiver parses the raw request body as JSON. GitHub only sends raw JSON
when the webhook's **Content type** is `application/json`. With the other option,
`application/x-www-form-urlencoded`, GitHub wraps the payload as
`payload=<url-encoded JSON>` — the signature still verifies (it is computed over
the raw body on both sides), but that body is not valid JSON, so parsing fails.

**Fix**

- On GitHub, open **Settings → Webhooks →** *(your hook)* and set **Content
  type** to `application/json`, then **Redeliver** from Recent Deliveries.
- For non-GitHub senders, POST the JSON body directly (do not form-encode it)
  with `Content-Type: application/json`.
- See the [Getting Started webhook walkthrough](getting-started.md#configuring-the-webhook-on-github).

## Webhook returns 400 `missing required param`

**Symptom**

```
missing required param: image
```

**Cause**

The target job declares a `required: true` input (e.g. `image`), and the
receiver's `spec.paramsMapping` either omits that key entirely or maps it from
a payload field that isn't present in the delivered body — either way, no
value resolves for a required input.

**Fix**

- Add (or correct) a `paramsMapping` entry for every required input:
  ```yaml
  spec:
    paramsMapping:
      image: "{{ .Payload.repository.name }}"
  ```
- If a required input can reasonably default, give it a `default` in the job
  instead of requiring every caller to supply it.
- Test the mapping by POSTing a representative payload to the receiver and
  confirming the response is `200` with a run, not `400`.
- See [Resource Reference: WebhookReceiver](resources.md#webhookreceiver) for
  the full delivery response table (`200` / `204` / `401` / `400`).

## k8s pod `ImagePullBackOff` on `unified-artifact`

**Symptom**

The job's pod is stuck in `ImagePullBackOff` or `ErrImagePull`, and
`kubectl describe pod` shows the failing container is `unified-artifact` (the
auto-injected workspace sidecar), not one of the job's own containers.

**Cause**

The k8s-agent injects a sidecar container named `unified-artifact` into every
pod to handle `uploadArtifact` / `downloadArtifact` / `cache` transfers. Its
image is set by the agent's `sidecarImage` config field
(default `ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest`) — if that tag
isn't pullable from the node (private registry without imagePullSecrets, typo
in the tag, or the tag was deleted), the pod can never become Ready.

**Fix**

- Confirm the configured `sidecarImage` is pullable from the cluster's nodes:
  `docker pull <sidecarImage>` from a node, or check for image pull secrets if
  the registry is private.
- The sidecar image **must match the agent's release** — it runs the
  `unified-sidecar` binary via `exec`, using a binary protocol; an
  older/mismatched image is incompatible even if it happens to pull
  successfully. Pin `sidecarImage` to the same version as the k8s-agent.
- See [Kubernetes Integration Guide](kubernetes-integration.md) for the full
  sidecar contract and `sidecarS3SecretName` configuration.

## A sidecar failed to start

**Symptom**

A step that talks to a `podTemplate` sidecar (a database, a tool container)
fails or hangs — connection refused, timeout, or similar — and it's unclear
whether the sidecar's own process ever came up.

**Cause**

There are no readiness probes for `podTemplate` sidecars (see [Job Isolation:
`native` and the claim pod](jobs.md#job-isolation-native-and-the-claim-pod)),
so a step can easily run before its sidecar is ready — or the sidecar's
process may have failed to start at all (bad config, missing env var, crash
on boot).

**Fix**

Open the run in the Web UI and select the sidecar in the **Sidecars** group
in the step sidebar (a separate group from "Steps") to read its own
stdout/stderr — this is the sidecar's own process output (e.g. `mysqld`'s
startup log), not step output. The status dot and label next to its name
show whether it's still `running` or has `exited N`; a non-zero `N` is the
container's exit code and points straight at why it never came up. This
works even after the run finishes and the pod/container is torn down — the
sidecar's log lines persist in the run's log store. See [Job Reference:
Sidecar container logs](jobs.md#sidecar-container-logs) for the full
behavior.

## Artifact step fails `no such file`

**Symptom**

```
upload-artifact "missing-artifact": tar walk "/root/workspace/working0/does-not-exist.txt": lstat /root/workspace/working0/does-not-exist.txt: no such file or directory
```

**Cause**

A relative `path:` in `uploadArtifact` (or `destDir:` in `downloadArtifact`)
resolves against the **run workspace** — the same directory `run:` steps
execute in — on every agent type (standard and Kubernetes). This error means
the file genuinely isn't there at that location: a common cause is a step
that wrote the file using its own working-directory assumption (e.g.
`cd subdir && make build` writing to `subdir/out/report.txt`, then a later
step referencing `out/report.txt` relative to the workspace root instead of
`subdir/out/report.txt`), or a step that wrote outside the workspace entirely
(e.g. an absolute path like `/tmp/report.txt`).

**Fix**

- Double check the exact path the producing step wrote to, relative to the
  run workspace root — add a debugging `run: ls -la` or `find . -name
  '<file>'` step before the `uploadArtifact` step if unsure.
- If a step intentionally `cd`s into a subdirectory, reference the artifact
  path relative to the workspace root, not the step's `cd` target.
- Artifact/cache paths must be **workspace-relative**: an absolute `path:`/
  `destDir:` (or one that escapes the workspace via `..`) is now rejected
  outright — see [Step fails with `artifact/cache path ... escapes the
  workspace`](#step-fails-with-artifactcache-path--escapes-the-workspace). Have
  the producing step write the file inside the workspace instead.
- See [Job Reference: Artifacts](jobs.md#artifacts) for the full path
  resolution rules.

## `artifact download` fails

**Symptom**

`unified-cli artifact download <run-id> <name>` errors instead of extracting a
file.

**Cause**

Either the run ID is wrong/belongs to a different job than expected, or the
artifact `name` doesn't match what `uploadArtifact` used for that run (names
are case-sensitive and must match exactly).

**Fix**

Always list the run's artifacts first to get the exact name:

```bash
unified-cli artifact list <run-id>
# app-binary
# test-report

unified-cli artifact download <run-id> test-report --dest ./out
```

If the list is empty, the run never reached (or failed before) its
`uploadArtifact` step — check `unified-cli logs <run-id>` for the upload step's
outcome.

## Step fails with `artifact/cache path ... escapes the workspace`

**Symptom**

A step fails with `artifact/cache path "<p>" escapes the workspace`.

**Cause**

An `uploadArtifact`/`downloadArtifact`/`cache` step used an absolute path or a `..` path that leaves the run workspace. This is rejected to keep steps from reading or writing files outside the workspace (on Kubernetes the artifact sidecar is more privileged than the job container).

**Fix**

Use a path relative to the workspace (e.g. `dist`, not `/workspace/dist` or `../dist`). `$UNIFIED_WORKSPACE` names the workspace root if you need an absolute base.

**Behavior change (2026-07 path hardening)**

Before the 2026-07 path hardening, an absolute artifact/cache path was not
rejected — it was silently re-rooted under the workspace (on Kubernetes) or
resolved against the host filesystem (on the standard agent), so a step
could appear to work while quietly reading or writing the wrong location.
Absolute paths now hard-fail with the error above instead. This is
intentional: if a step that used to "work" with an absolute path now fails
here, it was previously relying on the silent-reroot/host-resolve behavior —
switch it to a workspace-relative path per the Fix above.

## `uses:` run fails with `uses: targets must be kind: JobTemplate`

**Symptom**

A run using `uses: git://...` fails at creation with:

```
uses: targets must be kind: JobTemplate (got kind: Job); convert the template, or invoke the job with call:
```

or a strict-decode error naming a field, e.g. `field agentSelector not found in type dsl.JobTemplateSpec`.

**Cause**

`uses:` targets must be `kind: JobTemplate` — a strict schema holding only
what inlining can honor. The fetched file is either a pre-migration
`kind: Job` template, or declares a field outside the JobTemplate schema.
Note that pinned refs (`@v1.2.3` / `@<sha>`) pointing at commits from before
the template's migration keep fetching the old `kind: Job` content forever.

**Fix**

Convert the template (`kind: Job` → `kind: JobTemplate`, drop unsupported
fields) and re-pin tags/SHAs to a post-migration commit — see the
[migration guide](migration-2026-07-uses-jobtemplate.md). If the target
genuinely needs its own pod/agent/run semantics, keep it a `kind: Job` and
invoke it with `call:` instead.

## Scoped `uses` step can't find workspace files

**Symptom**

A `uses:` step with `runsIn.image` set (a [scope](jobs.md#uses-level-runsinimage-scope))
runs a template step that expects a file produced earlier in the outer job —
`cat`, a build tool, a script — and the file is simply not there (`no such
file or directory` or equivalent), with no error from the framework itself
pointing at the cause.

**Cause**

A `uses:` step with a uses-level `runsIn.image` runs the **entire inlined
template** in one isolated environment (one container on the standard agent,
one dedicated pod on Kubernetes) that starts from a **fresh, empty
filesystem** and never shares the outer job's workspace. Files written by
steps before the `uses:` step — via `run:`, `uploadArtifact`, or anything
else — are silently absent inside the scope; there is no error at scope
start, because from the scope's point of view there was never anything to
find. This is easy to miss because a **non-scoped** `uses:` step (no
`runsIn`) inlines into the caller's own workspace and does not have this
problem.

**Fix**

Treat the scope like a separate machine and cross the boundary explicitly:

- Pass inputs in via `with:` (environment variables) or `downloadArtifact`
  (pulls a previously uploaded artifact into the scope's filesystem).
- Get outputs back out via `uploadArtifact` (pushes to the run's artifact
  store, retrievable outside the scope) or `outputs:`/stdout.

See [Uses-level `runsIn.image` (scope)](jobs.md#uses-level-runsinimage-scope)
for the full model.

## Job fails apply with a dangling `container:` reference

**Symptom**

Applying (or triggering) a job that has no `uses:` step anywhere in
`steps:`/`finally:` now fails immediately with:

```
step "x" references container "y", which is not defined in the job's podTemplate
```

where this previously either passed apply and failed later at run creation,
or (for a job with `runsIn`-style scoping unrelated to `uses:`) failed
opaquely when the agent tried to exec into a nonexistent container.

**Cause**

A step's `container:` field (or a `uses:`-inlined step's inherited
`container:`) must name either the reserved primary container (`job`), one
of the other reserved names, or a container actually declared in
`spec.podTemplate.spec.containers`. For a **plain job** — no `uses:` step in
`steps:` or `finally:`, and no named agent-side `podTemplate.name` (whose
containers live in agent config and aren't visible here) — this is now
checked at **apply time** via `internal/dsl.Job.Validate` /
`ValidateContainerReferences` (`internal/dsl/container.go`), instead of only
at run creation or step-exec time. A job that carries any `uses:` step still
defers this check to the controller's post-resolution sweeper, because the
template's pod-shape merge may supply the missing container at resolution
time; a named agent-side `podTemplate.name` defers to pod build on the
agent.

**Fix**

- Add the missing container to `spec.podTemplate.spec.containers`, or
- Fix the typo in the step's `container:` field, or
- Remove `container:` from the step to use the primary container instead.

See [Job Reference — `container:`](jobs.md#container--targeting-a-podtemplate-container)
and [`ValidateContainerReferences`](../internal/dsl/container.go).

## `podTemplate` container/volume name rejected as an invalid DNS-1123 label

**Symptom**

Applying a `Job` or a `JobTemplate` (or resolving a `uses:` template whose
`podTemplate` merges into the caller) fails with an error like:

```
podTemplate container name "My_Tools" is not a valid DNS-1123 label (lowercase alphanumerics and '-', must start/end alphanumeric)
podTemplate volume name "Cache Vol" is not a valid DNS-1123 label (lowercase alphanumerics and '-', must start/end alphanumeric)
```

or `podTemplate container name is required` / `... exceeds 63 characters` for
an empty or overlong name.

**Cause**

Every `podTemplate.spec.containers[].name` and `podTemplate.spec.volumes[].name`
must be a valid Kubernetes DNS-1123 label — lowercase alphanumerics and `-`
only, starting and ending with an alphanumeric character, 63 characters or
fewer (`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`). This is now checked at **apply
time** by `internal/dsl.ValidateDNS1123Label` (`internal/dsl/container.go`),
for both `Job.Validate` and `JobTemplate.Validate` — previously an invalid
name (uppercase, underscores, spaces, a leading/trailing `-`, a `.`, or an
empty/too-long name) went unvalidated until the pod was actually built,
surfacing as an opaque Kubernetes/container-runtime API error much later.
This also closes a case/whitespace evasion of the reserved-name checks
(`job`/`unified-artifact`/`ucd-shim` for containers,
`workspace`/`ucd-tools` for volumes): those checks normalize
(trim+lowercase) before comparing, but a variant like `" Job "` is now
rejected by shape validation before it would even reach that comparison.

**Fix**

Rename the container/volume to a valid DNS-1123 label, e.g. `my-tools`
instead of `My_Tools`, `cache-vol` instead of `Cache Vol`. See [Job
Reference — Kubernetes Pod Template
(`podTemplate`)](jobs.md#kubernetes-pod-template-podtemplate) for the full
rule and the reserved-name list.

## `uses: git://...` job fails to resolve with invalid characters

**Symptom**

A `uses: git://...` job fails to resolve with `git URI ref "..." contains invalid characters`.

**Cause**

The `@ref` portion contains characters outside `[A-Za-z0-9._/+-]` or starts with `-` (blocked to prevent git option injection).

**Fix**

Reference a normal branch, tag, or SHA. Relative ref syntax like `HEAD~1`, `HEAD^`, or `main@{upstream}` is intentionally rejected; use a plain branch name, tag, or full SHA instead.

## Conditional step ran when it shouldn't

**Symptom**

A step gated with `if:` runs even though its condition looks false, and the
agent log contains:

```
if: condition eval failed, running step
```

(on the k8s agent, the same line is prefixed: `k8s: if condition eval failed,
running step` — grep for `if condition eval failed, running step` to match
both agents)

with a nested compile error, e.g.:

```
if: expression "{{ eq .Params.x \"y\" }}" compile error: ERROR: <input>:1:17: Syntax error: missing ':' at '"y"'
```

**Cause**

`if:` expressions are **CEL**, not Go templates — unlike `run:`, `env:`, and
`outputs:` in the same job, which do use `{{ .Params.X }}`-style Go template
syntax. Writing an `if:` with `{{ }}` delimiters (or any other expression that
fails to compile or evaluate) **fails open**: the step still runs, and the
only trace is a `WARN` line in the agent log — the run itself is not marked
failed and the CLI/API give no other indication.

**Fix**

- Use valid CEL syntax, with lowercase variables and no `{{ }}` delimiters:
  ```yaml
  if: 'params.env == "production"'
  ```
  not:
  ```yaml
  if: '{{ eq .Params.env "production" }}'   # wrong — Go template, fails open
  ```
- After adding or changing a non-trivial `if:`, check the agent log for
  `if: condition eval failed, running step` to confirm it compiled.
- See [Job Reference: Conditional Execution (`if`)](jobs.md#conditional-execution-if)
  for the full CEL variable/function reference — this is especially important
  to verify for any `if:` gating a production deploy.

## A step's log shows `step panicked: ...`

**Symptom**

A step fails with a log line like:

```
step panicked: runtime error: invalid memory address or nil pointer dereference
```

(stream `stderr`), and the run is `Failed` (or, for a `continueOnError: true`
step, the step itself is `Failed` but the run continues).

**Cause**

Something inside the step's own execution path — the step body, template
expansion, or the underlying backend exec — panicked instead of returning a
normal error. The agent recovers the panic at the step boundary, writes the
panic value and stack into that step's own log (this line), and reports the
step `Failed`, honoring `continueOnError` exactly like a normal error would.
Only this run is affected: sibling concurrent runs on the same agent and the
agent process itself are unaffected — a panic here used to crash the whole
agent process (taking down every run it was executing), which is why this is
now caught at the step boundary rather than left to propagate.

**Fix**

Treat it like any other step failure: the panic message and stack trace in
the step's log point at the failing code path (a job's `run:` script, a
custom tool, or the like). Fix the underlying bug in whatever the step
invoked; there is no agent-side workaround needed once the run itself is
correctly reported `Failed` rather than stuck `Running`.

A rarer variant panics outside the step body itself — e.g. while preparing
the workspace, before any step started — and surfaces instead as the run
being failed with an `agent panic: ...` message (no per-step log line, since
no step ever ran). The cause and fix are the same: something panicked, the
agent turned it into a normal Failed outcome instead of crashing, and the
panic text points at the failing code.

## Run marked `Failed` with "agent lost"

**Symptom**

A run that was `Running` flips to `Failed` with no step-level error, and the
controller log shows:

```
stuck-run reaper: failed orphaned run (agent lost)
```

**Cause**

The agent that claimed the run stopped sending heartbeats (crashed, was
killed, or lost network connectivity) and never resumed. The controller's
orphaned-run reaper detects a `Running` run whose claiming agent's heartbeat
has gone stale and fails the run rather than leaving it stuck forever. It
fails (never re-queues) the run, since re-running partially-executed steps
risks duplicating side effects like deploys.

**Fix**

This is expected recovery behavior, not a bug to work around — the run
genuinely needs to be re-triggered once the underlying agent problem is
fixed:

- Confirm the agent is back and healthy: `unified-cli agent list`.
- Re-trigger the job once the agent (or a replacement in the same pool) is
  available.
- On Kubernetes, the run's `ucd-run-*` pod is garbage-collected separately;
  no manual pod cleanup is required.
- See [High Availability Guide: Orphaned-Run Recovery](high-availability.md#orphaned-run-recovery)
  for the full heartbeat/reaper timing and design.

## Run marked `Failed` by heartbeat reconcile after a lost claim

**Symptom**

A run flips from `Running` to `Failed` unusually fast — well under the
stuck-run reaper's ~90s-stale-heartbeat-plus-grace window above — with no
step ever having reported progress, and `unified-cli agent list` shows the
run's claiming agent as perfectly healthy the whole time (`last_seen_at`
kept advancing normally; the agent never went stale). The controller log
does **not** show `stuck-run reaper: failed orphaned run (agent lost)` for
this run — that line is specific to the slower stale-heartbeat path.

**Cause**

Every agent heartbeat now reports the run IDs it currently considers active
(`activeRunIds`). This particular run's claim never made it into that set —
typically because the HTTP response to a successful `Claim` call never
reached the agent process (a network blip right after claiming, or the
agent restarted in the instant between claiming and starting the run) — so
the agent itself never learned it owned the run and correctly never reports
it as active. The controller cross-checks this on every heartbeat: a
`Running` run assigned to that agent, absent from its self-reported active
set, and claimed more than ~60s ago (a grace window so a claim whose
heartbeat simply hasn't landed yet isn't reaped prematurely) is failed as
orphaned right there — instead of waiting for that agent to look dead via
the much slower stale-heartbeat path. The run is failed, never re-queued,
for the same reason the stuck-run reaper doesn't re-queue: re-running
partially-executed steps can duplicate side effects.

**Fix**

This is expected self-healing, not a bug to work around — the run genuinely
needs to be re-triggered, same as any other orphaned run:

- Re-trigger the job; the agent itself is healthy and will claim it normally.
- If this recurs frequently for one agent, suspect network reliability
  between that agent and the controller — a lost claim response is the
  underlying trigger, not agent instability.
- A legacy agent (built before this feature) sends a bodyless heartbeat and
  never participates in this reconcile path; it relies solely on the
  stuck-run reaper above for a lost-claim recovery, which takes longer but
  still eventually fails the run.

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

## Run fails with log line `git template resolution failed for more than 1h0m0s`

**Symptom**

a run fails with log line `git template resolution failed for more than 1h0m0s: ...`.

**Cause**

the job references a `git://` template whose host stayed unreachable (or credentials stayed invalid) past `UNIFIED_GIT_RESOLVE_DEADLINE`.

**Fix**

fix the repository URL/credentials and re-trigger the run.

## Controller logs `dropping log line for sealed run`

**Symptom**

The controller logs `dropping log line for sealed run` (or `dropping log lines for sealed run`).

**Cause**

An agent sent log lines for a run whose logs were already archived (~30 s after the run finished). Log sealing is active whenever an object store is configured, independent of the `--log-trim-days` setting. The archive is the sealed source of truth, so the lines were discarded — storing them would make the run untrimmable and, after a trim, invisible anyway.

**Fix**

Occasional occurrences are expected noise. Common causes include:

- An agent retrying after a network partition (the run was finalized by the stuck-run reaper meanwhile)
- Teardown/buffer flushes arriving later than the archiver delay

If you see a sustained stream of these warnings for the same run, the agent may be stuck and worth restarting.

## A run's log shows `[N log line(s) dropped: controller unreachable]`

**Symptom**

A run's log (on the `stderr` stream, wherever the agent's log pusher for that
step/run was flushing) contains a synthetic line like:

```
[42 log line(s) dropped: controller unreachable]
```

This is distinct from [`dropping log line for sealed
run`](#controller-logs-dropping-log-line-for-sealed-run) above — that one is
logged by the *controller* and means lines arrived too late (after
archival); this one is written into the *run's own log stream* by the
*agent* and means lines never arrived at all.

**Cause**

The agent buffers log lines locally and ships them to the controller in
batches; if the controller is unreachable for a sustained stretch (network
partition, controller restart/outage), the agent keeps the batches it
couldn't send in a bounded in-memory queue (capped at 1MiB). Once that cap
is exceeded, the **oldest** queued batches are evicted to make room for
newer ones — at least the single most recent batch is always kept, even if
it alone exceeds the cap. Rather than let that gap in the run's log pass
silently, the agent counts every discarded line and, on the next flush that
successfully reaches the controller, emits this one synthetic line
reporting exactly how many lines were lost. The counter then resets — if a
second partition causes more drops later in the same run, a second marker
with the new count appears when connectivity next recovers.

**Fix**

The marker itself needs no action — it is a visibility feature, not an
error to fix. It tells you a real gap exists in that step's log around the
time the marker appears (the step almost certainly still ran; only the log
of it is incomplete):

- If the gap coincides with a known controller restart/outage or network
  maintenance window, this is expected — no data beyond the log lines
  themselves was lost (the step's actual execution, its status reports, and
  its artifacts/outputs are unaffected; only the buffered stdout/stderr
  text for that window is gone).
- If you see this marker frequently with no obvious controller-side cause,
  check connectivity and latency between the affected agent and the
  controller — a marginal or high-latency link between them, not the
  controller's own health, is the more likely culprit.
- There is no way to recover the specific dropped lines after the fact; the
  count in the marker is the only remaining record that they existed.

## Controller fails at startup with `schema drift: ... does not exist`

**Symptom**

After upgrading the controller binary/image against an existing database, the
controller **fails fast at startup** (it never finishes booting) with an
error such as:

```
schema drift: schema_migrations.version=7 claims 007_step_call_link is applied,
but step_reports.child_run_id does not exist; migration files were likely
renumbered after this database was migrated - see docs/troubleshooting.md
("Schema drift") for recovery
```

**Cause**

After running `golang-migrate`'s `Up()`, the controller calls `verifySchema()`
(`internal/store/postgres.go`, `internal/store/verify.go`), which cross-checks
`schema_migrations.version` against a sentinel object (a table, column, or
index) for every migration it claims is applied. `golang-migrate` only
compares version *numbers* — if migration files were renumbered (e.g. an
old incremental series was squashed/re-sequenced) after a database was already
migrated to the old numbering, `migrate up` treats that database as fully
migrated and silently skips the renumbered files, even though their schema
objects were never created. `verifySchema()` catches this by probing for the
actual objects and fails startup immediately with a "schema drift" error
instead of letting the controller boot with a stale schema and fail later,
per-request, with errors like `column "role" does not exist`.

This is exactly the same class of drift described in
[Schema drift (migration renumbering)](#schema-drift-migration-renumbering)
below — see that section for full diagnosis and recovery steps (apply the
missing migration's `.up.sql` by hand, then restart so `verifySchema()`
re-checks and confirms the fix).

## Dev stack: controller container unhealthy, `vendor/modules.txt` errors

**Symptom**

`docker compose up` starts the controller container but it never becomes
healthy, and its logs show something like:

```
inconsistent vendoring in /app:
	github.com/some/module@v1.2.3: is explicitly required in go.mod, but not marked as explicit in vendor/modules.txt

	go mod vendor
to sync the vendor directory.
```

**Cause**

The dev `docker-compose` stack's `air` hot-reload containers mount the repo
into the container (`/app`), including the git-ignored local `vendor/`
directory. If `go.mod`/`go.sum` changed (e.g. after a `git pull` or branch
switch) but the local `vendor/` wasn't regenerated, the in-container Go build
fails with this inconsistency error and the controller never passes its
health check.

**Fix**

```bash
go mod vendor
docker compose restart controller agent
```

## Local Kubernetes won't start (`kubelet is not healthy`)

**Symptom**

Docker Desktop's Kubernetes (kind mode) never comes up, and its logs show:

```
The kubelet is not healthy after 4m0s
```

with the hint `required cgroups disabled` in the underlying error.

**Cause**

The Kubernetes node runs inside WSL2, and the kubelet requires cgroup v2.
WSL2 defaults to cgroup v1 on some installs/kernels, so the kubelet fails
its startup health check.

**Fix**

Edit (or create) `%UserProfile%\.wslconfig` and add the kernel command line
switch to force cgroup v2 — put it on its own line, one key per line:

```ini
[wsl2]
kernelCommandLine = cgroup_no_v1=all systemd.unified_cgroup_hierarchy=1
```

Then restart WSL2 so the change takes effect:

```bash
wsl --shutdown
```

Restart Docker Desktop and re-enable Kubernetes. Verify cgroup v2 is active
from inside any WSL2 distro:

```bash
test -f /sys/fs/cgroup/cgroup.controllers && echo "cgroup v2 active"
```

## Agent enrollment and credentials

### VM agent cannot enroll or refresh

**Symptom:** the agent receives `unauthorized`, `agent identity disabled`, or
`enrollment unavailable` while starting or refreshing.

**Fix:** `unauthorized` means a one-time enrollment credential was malformed,
expired, used, revoked, or a refresh credential is no longer valid. Create a
new enrollment file; plaintext tokens cannot be retrieved from list/get
responses. For a lost or replayed refresh credential, run `unified-cd agent
identity revoke-credentials <agent-id>`, create a new enrollment, and restart
the agent with private `credentialFile` and `enrollmentTokenFile` paths.
`agent identity disabled` requires an administrator to investigate and enable
the identity or replace it. `enrollment unavailable` is retryable only after
PostgreSQL/controller availability is restored; auth fails closed.

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

## Schema drift (migration renumbering)

**Symptom:** the controller exits at startup with an error like:

```
schema drift: schema_migrations.version=7 claims 007_step_call_link is applied,
but step_reports.child_run_id does not exist; migration files were likely
renumbered after this database was migrated - see docs/troubleshooting.md
("Schema drift") for recovery
```

**Cause:** migration files were renumbered (typically when parallel branches
merged) after this database had already been migrated. golang-migrate compares
only version numbers, so a database whose recorded version matches an older
numbering silently skips the current file with that number.

**Diagnosis:** compare `SELECT version FROM schema_migrations;` against
`internal/store/migrations/`. The startup error names the first migration
whose objects are missing; later ones may be missing too.

**Recovery:**

1. For each missing migration (start with the one named in the error), apply
   its `.up.sql` statements manually, e.g.:

   ```
   psql "$DSN" -f internal/store/migrations/007_step_call_link.up.sql
   ```

2. Leave `schema_migrations.version` as-is when it already equals the highest
   migration number; only the schema objects were missing.
3. Restart the controller. Startup verification re-runs and confirms the fix.

**If the error says `schema_migrations is dirty` instead:**
Either a previous migration attempt crashed midway, leaving the dirty flag set, or another replica's migration is currently in flight (this can happen transiently during a mixed-version rolling deploy). Restart the controller first; if the error persists across restarts, it is the crashed-migration case, not an in-flight one — inspect which statements of the named version actually applied to the schema, repair them manually to a consistent state, then clear the flag with golang-migrate's `force` command or `UPDATE schema_migrations SET dirty = false` once the schema matches the version number. Then restart the controller.
