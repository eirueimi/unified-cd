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
unified-cd agent list
# docker-agent-1   c1e136ded609   linux         hostname:c1e136ded609,kind:docker,pool:default   2026-07-04 04:54
# k8s-agent-1      DESKTOP-EMUF6H6 windows/k8s   kubernetes,kind:k8s,pool:default,hostname:...    2026-07-04 04:54
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

Cancel a run stuck this way with `unified-cd run cancel <run-id>`.

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
- Use an absolute path in `path:`/`destDir:` only when the file is
  intentionally outside the workspace (e.g. a shared cache directory) —
  absolute paths pass through unchanged.
- See [Job Reference: Artifacts](jobs.md#artifacts) for the full path
  resolution rules.

## `artifact download` fails

**Symptom**

`unified-cd artifact download <run-id> <name>` errors instead of extracting a
file.

**Cause**

Either the run ID is wrong/belongs to a different job than expected, or the
artifact `name` doesn't match what `uploadArtifact` used for that run (names
are case-sensitive and must match exactly).

**Fix**

Always list the run's artifacts first to get the exact name:

```bash
unified-cd artifact list <run-id>
# app-binary
# test-report

unified-cd artifact download <run-id> test-report --dest ./out
```

If the list is empty, the run never reached (or failed before) its
`uploadArtifact` step — check `unified-cd logs <run-id>` for the upload step's
outcome.

## Conditional step ran when it shouldn't

**Symptom**

A step gated with `if:` runs even though its condition looks false, and the
agent log contains:

```
if: condition eval failed, running step
```

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

- Confirm the agent is back and healthy: `unified-cd agent list`.
- Re-trigger the job once the agent (or a replacement in the same pool) is
  available.
- On Kubernetes, the run's `ucd-run-*` pod is garbage-collected separately;
  no manual pod cleanup is required.
- See [High Availability Guide: Orphaned-Run Recovery](high-availability.md#orphaned-run-recovery)
  for the full heartbeat/reaper timing and design.

## Controller fails with `column "..." does not exist` after upgrading

**Symptom**

After upgrading the controller binary/image against an existing database, the
controller starts (migrations appear to run without error) but requests fail
at runtime with errors such as:

```
column "role" does not exist
column "managed_resources" does not exist
relation "audit_logs" does not exist
column "sync_status" does not exist
```

**Cause**

Commit `79c1074` squashed the original incremental migrations `001`-`017`
into a single consolidated `001_init` (plus a new, renumbered `002`-`006`
series for schema changes added after the squash). A database that was
**provisioned before the squash** already has `schema_migrations.version`
recorded as `17` (or wherever it had reached in the old numbering).
`golang-migrate` only applies migrations with a version *greater than* the
recorded one — since the new chain tops out at version `6`, `migrate up`
against such a database is a **silent no-op**: it reports success and the
controller starts normally, but none of the columns/tables introduced after
the squash point (`role`, `managed_resources`, `audit_logs`, `sync_status`,
etc.) ever get created.

This only affects databases created **before** the squash landed. A database
initialized from the current migration set (fresh install) is unaffected.

**Fix**

In-place `migrate up` is **not a supported upgrade path** across the squash
boundary. Choose one of:

- **Fresh init (recommended when data loss is acceptable)** — provision a new
  empty database and let the controller run the current migration set from
  scratch (`001_init` through the latest). This is the simplest and
  best-tested path; see [Operations Guide: Recovery Runbook](operations.md#recovery-runbook)
  for re-applying resources afterward.
- **Manual bridge (when the existing data must be preserved)** — inspect
  `schema_migrations.version` on the old database, then manually apply the
  DDL each pre-squash migration (`002`-`017` in the old numbering, as they
  existed on the commit immediately before `79c1074`) would have added, and
  finally set `schema_migrations` to match the new chain's head version so
  `migrate up` treats the database as fully migrated. This must be done by
  hand (or with a custom script) — there is no automated tool for it — and
  should be tested against a copy of the database first.

Check which case you're in before upgrading:

```sql
SELECT version, dirty FROM schema_migrations;
```

If `version` is `6` or lower on a database that predates commit `79c1074`,
it silently skipped the squashed migrations and needs the bridge above rather
than a plain restart.

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
