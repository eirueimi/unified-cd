# Job Structure

## Job Structure

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: <string>                  # unique job name (required)
  labels:                         # optional key-value labels
    key: value
spec:
  params: { ... }                 # input/output parameter declarations
  agentSelector: [ ... ]          # required agent label filters
  concurrency: { ... }            # concurrency control
  description: <string>           # optional: human-readable summary of the job
  timeoutMinutes: 60              # job-level timeout in minutes
  native: false                   # true = host-process job, no containers at all (see below)
  podTemplate: { ... }            # sidecar containers for an isolated job (both agents honor this)
  steps:
    - name: <string>              # step name (required, unique within job)
      if: <expression>            # run condition
      env: { KEY: VALUE }         # environment variables
      run: <shell script>         # shell command
      outputs: { key: expr }      # capture output values
      call: { ... }               # call another registered job
      uses: { ... }               # inline a git template
      cache: { ... }              # cache a directory
      uploadArtifact: { ... }     # upload a file as an artifact
      downloadArtifact: { ... }   # download a previously uploaded artifact
      post: { ... }               # post-run cleanup hook
      container: <string>         # exec into a named podTemplate container instead of the primary
      continueOnError: false      # don't fail the run if this step fails
      timeoutMinutes: 10          # step-level timeout in minutes
      retry: { attempts: 3, backoff: 30s }  # retry a run: step on failure (run: only)
    - parallel:                   # OR: a group of steps that run concurrently
        - name: <string>          # (see "Concurrent Steps (parallel)")
          run: <shell script>
```

---

## Metadata

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique job identifier. Used in CLI, API, and when calling this job from another job. |
| `metadata.labels` | map[string]string | No | Arbitrary labels. Not used for routing; reserved for future filtering. |

### Hierarchical grouping (annotations.path)

A job's position in the Web UI tree comes from `metadata.annotations.path`.
Jobs synced by an AppSource get this set automatically from their directory
(relative to the AppSource `spec.path`), so `jobs/team-a/build.yaml` shows as
`build` under a `team-a` folder. The stored, unique job name is the *qualified*
name `team-a/build` — trigger it with `unified-cli run trigger team-a/build`.
Jobs applied directly with no `path` appear at the tree root.

**Upgrade note:** if you're upgrading from a version predating hierarchical
grouping, only jobs at the AppSource root (no subdirectory) keep their old
name unchanged. Jobs that previously synced from a subdirectory (e.g.
`jobs/team-a/build.yaml`, previously stored as `build`) are re-keyed to their
qualified name (`team-a/build`) on the next sync — this is a one-time
prune/re-create of those jobs. Re-point any Schedules or WebhookReceivers
that reference the old flat name before or right after upgrading.

---

## Job-level Timeout

```yaml
spec:
  timeoutMinutes: 120
```

If the job has not completed within `timeoutMinutes`, the entire run is cancelled.
Individual steps can also have their own `timeoutMinutes` (step-level timeout is independent).

---

## Job Description

```yaml
spec:
  description: Build and deploy the application
```

`spec.description` *(optional, string)* — a human-readable summary of the job, shown in the WebUI job list and job detail pages. Plain text.

---

