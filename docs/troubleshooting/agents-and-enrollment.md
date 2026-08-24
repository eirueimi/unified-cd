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

