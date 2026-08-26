# Artifacts and Cache

## Artifacts

Upload and download files between steps and jobs. By default artifacts are
scoped to the current run; a `downloadArtifact` step can fetch from another
run — most usefully a `call:` child run — with `runId:` (see
[Downloading from another run (`runId`)](#downloading-from-another-run-runid)).

```yaml
steps:
  - name: build
    run: go build -o bin/app ./cmd/server

  - name: upload-binary
    uploadArtifact:
      name: app-binary          # artifact name
      path: bin/app             # local path to upload

---
# In a separate job or a later step:
  - name: download-binary
    downloadArtifact:
      name: app-binary          # must match the upload name
      destDir: artifacts        # workspace-relative dir (default: workspace root)

  - name: run-binary
    run: artifacts/app --version
```

Artifacts are stored in the S3-compatible object store. Artifact names must be unique within a run.

The `path`/`destDir` of an artifact step must be workspace-relative — see [Artifact and cache path rules](#artifact-and-cache-path-rules) below.

Artifacts work on both the standard and Kubernetes agents; on the k8s-agent, transfers are handled by an auto-injected workspace sidecar (`unified-artifact`) that talks to the object store **directly** — see [Kubernetes Integration: S3 credentials](../../operator-manual/kubernetes-integration.md#s3-credentials-required) for how transfers work and the S3 credentials the sidecar requires.

Those credentials are **required** on the k8s-agent, and are separate from the controller's own S3 configuration: the sidecar needs a `Secret` in the job Pod's namespace, named by the agent's `sidecarS3SecretName`. Without it every artifact step fails with `artifact requires S3 configuration (UNIFIED_S3_*)` — unlike cache, which degrades to a no-op rather than failing. The k8s-agent now detects this when it claims the run and fails immediately instead of mid-job.

### Downloading from another run (`runId`)

`downloadArtifact.runId` selects the run to download from. It is
template-expandable; combined with `{{ .Steps.<call-step>.ChildRunID }}`
(set on every successful `call:` step) it retrieves artifacts produced by a
called job:

```yaml
steps:
  - name: build_app
    call:
      job: build
      with: { tag: "{{ .Params.tag }}" }

  - name: fetch-child-binary
    downloadArtifact:
      name: app-binary                              # name in the child run
      runId: "{{ .Steps.build_app.ChildRunID }}"    # default: current run
      destDir: artifacts
```

- For matrix `call:` steps, `ChildRunID` aggregates per combination key,
  like outputs: `{{ index .Steps.build_app.ChildRunID "linux/amd64" }}`.
- The expanded `runId` must match `^[A-Za-z0-9_-]{1,64}$`; the expansion
  context excludes `Secrets` and `Stdout`. A template or validation failure
  fails the step.
- `runId` works on both the standard and Kubernetes agents.

### Listing and downloading artifacts (humans)

Besides the job-to-job `uploadArtifact` / `downloadArtifact` steps above, a human operator can
list and fetch a run's artifacts directly through the API or the CLI.

**API:**

```
GET /api/v1/runs/{runID}/artifacts
GET /api/v1/runs/{runID}/artifacts/{name}
```

- `GET /artifacts` lists the artifact names for the run as JSON: `[{"name": "app-binary"}, {"name": "test-report"}]` (an empty run returns `[]`).
- `GET /artifacts/{name}` streams the artifact as a tar+zstd archive (the same format `uploadArtifact`/`downloadArtifact` steps use).
- Both routes accept **either** an agent bearer token **or** a human identity (PAT, OIDC `id_token`, or session cookie) — whichever `ServerAuth` would otherwise accept.
- `PUT /api/v1/runs/{runID}/artifacts/{name}` (upload) is unchanged and remains **agent-only**, authenticated with `BearerAuth` using the agent token. It is not reachable with a PAT, OIDC token, or session — only agents upload artifacts.

**CLI:**

```bash
unified-cli artifact list <run-id>
unified-cli artifact download <run-id> <name> [--dest .]
```

```bash
# List artifacts produced by a run
unified-cli artifact list a1b2c3d4
# app-binary
# test-report

# Download and extract "app-binary" into ./out
unified-cli artifact download a1b2c3d4 app-binary --dest ./out
# extracted app-binary of run a1b2c3d4 to ./out
```

`--dest` defaults to the current directory. Both commands authenticate using the CLI's configured server token (PAT or OIDC login), the same as other `unified-cli` commands.

---

## Cache

Cache directories (e.g. dependency downloads) across runs on the same agent or across agents when S3 is configured.

```yaml
steps:
  - name: restore-cache
    cache:
      path: vendor/             # directory to cache (supports templates, e.g. {{ .Params.working_dir }}/vendor)
      key: go-vendor-{{ hashFile "go.sum" }}
      restoreKeys:              # fallback keys (prefix match)
        - go-vendor-
      ttlDays: 30               # cache expiry (default: 30 days)

  - name: download-deps
    run: |
      if [ ! -d vendor ]; then
        go mod vendor
      fi

  - name: build
    run: go build ./...
```

The `path`, `key`, and `restoreKeys` strings support template expressions (e.g. `path: {{ .Params.working_dir }}/node_modules`, `key: go-vendor-{{ hashFile "go.sum" }}`). A `path` or `key` that fails to expand (or expands to empty) fails the step on both agents.
On hit, the cached directory is restored before the step runs. On miss, the directory is saved after the run completes.

Cache is now supported on the k8s agent (previously a silent no-op) with the same `key`/`restoreKeys`/`ttlDays` semantics — see [Kubernetes Integration: Artifacts and Cache](../../operator-manual/kubernetes-integration.md#artifacts-and-cache) for how transfers work and the required S3 credentials. Restore is best-effort (a miss or error never fails the step); save is deferred until the run's main stages complete.

Best-effort means the step still succeeds, **not** that a failure is reported as success. A restore the agent could not perform — an unreachable store, or a k8s-agent with no `sidecarS3SecretName` configured — is logged as *not restored*, never as a cache hit, and a save that did not happen is not logged as saved. If a cache is silently doing nothing, the agent log says so.

### Cache entries are namespaced per job

Cache entries are namespaced per **qualified job name** (e.g. `team-a/build`) — one job can never restore, and never even sees, another job's cache entries. `restoreKeys` prefix matching is likewise scoped to entries saved by the same job. Before this change the cache was one flat namespace shared by every job in the store: a job could plant an entry under a key another job's `restoreKeys` prefix would match, and that job would restore and execute the planted archive in its own secret context. That is no longer possible: an entry's owning job is baked into its storage location, and the owner is checked again on restore as defense in depth.

This is a storage layout change — every cache entry saved before this change is orphaned (no job can address it under the new layout) and is simply abandoned; the next run of each job regenerates its cache from scratch on the first miss. No migration or manual cleanup is needed.

`ttlDays` is capped at 365 (jobs asking for a longer TTL fail validation at parse time) so a single entry cannot pin itself, and the storage it occupies, indefinitely.

### Artifact and cache path rules

The `path` of an `uploadArtifact`/`downloadArtifact`/`cache` step must be **relative** to the run workspace. Relative paths behave identically across native, isolated, and Kubernetes execution. Absolute paths and paths that escape the workspace (via `..`) are rejected — the step fails with `artifact/cache path ... escapes the workspace`. Inside a step, `$UNIFIED_WORKSPACE` names the current workspace root (the step's working directory), so scripts can build workspace-relative paths portably.

---

