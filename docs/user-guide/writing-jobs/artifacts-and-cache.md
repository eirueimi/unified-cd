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

Artifacts work on both the standard and Kubernetes agents; the standard agent moves them with URLs the controller issues for the run (see [Object transfer](../../operator-manual/agents.md#object-transfer-objecttransfer)); on the k8s-agent, transfers are handled by an auto-injected workspace sidecar (`unified-artifact`) that moves the archive to or from the object store with URLs the controller issues for the run — see [Kubernetes Integration: How the sidecar reaches the object store](../../operator-manual/kubernetes-integration.md#how-the-sidecar-reaches-the-object-store). There is nothing to configure for it and no Secret to create: the sidecar holds no store credentials, and artifact and cache steps need no k8s-agent setting beyond the controller URL the agent already uses.

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
- `GET /artifacts/{name}` streams the artifact as a tar+zstd archive (the same format `uploadArtifact`/`downloadArtifact` steps use). If the operator set `objectStore.artifactDownload: redirect`, a human caller receives a `302` to a short-lived presigned URL instead (`?as=url` returns it as JSON); agents are always streamed the bytes.
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

Cache directories (e.g. dependency downloads) across runs on the same agent or across agents. The controller's object store holds the entries; agents need no cache configuration (the host agent's old `cacheEndpoint`/`cacheKey`/`cacheSecret`/`cacheBucket` settings were removed, see the [migration guide](../../operator-manual/migrations/2026-11-20-run-scoped-object-access.md)). The cache is never silently disabled.

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

`hashFile` is available only in the cache step's `key`, `path` and `restoreKeys` templates, and only on the standard (host) agent, including its isolated claim pods (not in a step that runs in a scope container, which fails with the same workspace error). It hashes files matched by a glob relative to the job workspace root (not the agent's own working directory); absolute paths, `..` segments, symlinked directories in the pattern, and non-regular files (devices, directories) are rejected. It is **not** available on the Kubernetes agent: the workspace lives in the job pod, which the agent process cannot read, so a cache step using `hashFile` there fails with `hashFile: the job workspace is not readable from this agent`; use a literal or parameter-derived key instead. Templates the controller evaluates (`displayName`, `agentSelector`, `concurrency`, `volumes`, webhook mappings), `foreach`/`matrix` sources, and other step text cannot use it: they fail with `hashFile is only available in step-time templates (cache key/path)` (controller) or the workspace error above (agent), because the controller never reads files on behalf of a job definition.
On hit, the cached directory is restored before the step runs. On miss, the directory is saved after the run completes.

Cache is now supported on the k8s agent (previously a silent no-op) with the same `key`/`restoreKeys`/`ttlDays` semantics — see [Kubernetes Integration: Artifacts and Cache](../../operator-manual/kubernetes-integration.md#artifacts-and-cache) for how transfers work and why no store credentials are needed. Restore is best-effort (a miss or error never fails the step); save is deferred until the run's main stages complete.

Best-effort means the step still succeeds, **not** that a failure is reported as success. A restore the agent could not perform — an unreachable store or controller — is logged as *not restored*, never as a cache hit, and a save that did not happen is not logged as saved. If a cache is silently doing nothing, the agent log says so.

### Cache entries are namespaced per job

Cache entries are namespaced per **qualified job name** (e.g. `team-a/build`) — one job can never restore, and never even sees, another job's cache entries. `restoreKeys` prefix matching is likewise scoped to entries saved by the same job. Before this change the cache was one flat namespace shared by every job in the store: a job could plant an entry under a key another job's `restoreKeys` prefix would match, and that job would restore and execute the planted archive in its own secret context. That is no longer possible: an entry's owning job is baked into its storage location, and the owner is checked again on restore as defense in depth.

This is a storage layout change — every cache entry saved before this change is orphaned (no job can address it under the new layout) and is simply abandoned; the next run of each job regenerates its cache from scratch on the first miss. No migration or manual cleanup is needed.

`ttlDays` is capped at 365 (jobs asking for a longer TTL fail validation at parse time) so a single entry cannot pin itself, and the storage it occupies, indefinitely.

### Artifact and cache path rules

The `path` of an `uploadArtifact`/`downloadArtifact`/`cache` step must be **relative** to the run workspace. Relative paths behave identically across native, isolated, and Kubernetes execution. Absolute paths and paths that escape the workspace (via `..`) are rejected — the step fails with `artifact/cache path ... escapes the workspace`. Inside a step, `$UNIFIED_WORKSPACE` names the current workspace root (the step's working directory), so scripts can build workspace-relative paths portably.

**Symlinks.** The host agent and the Kubernetes agent treat symlinks in an artifact or cache tree the same way. A symlink met while archiving a directory is stored as a link entry, not as a copy of its target, and is never followed; a link to a directory and a dangling link archive fine, so `node_modules` and `.venv` (full of `.bin` links) can be cached. Artifacts refuse link entries: a `downloadArtifact` of an archive that holds one fails with `unsupported symlink entry`. A cache restore skips link entries, still reports a hit, and prints one warning line to the run log (`cache restore warning: skipped N symlink/hardlink entries; links are not restored by the agents, so the cache may be incomplete ...`); tools such as `.venv` or `node_modules/.bin` may need re-creating after a restore.

On a Kubernetes agent the transfer is done by the sidecar, which in addition stays inside the workspace: a symlink whose target is outside the workspace is not followed, so a `path` or `destDir` that goes through one fails (artifacts) or is skipped (cache), and the target is never read or written. On the host agent the same rule applies to **isolated** jobs (the job runs in the claim pod and controls the workspace from inside it): a `path` or `destDir` whose symlink target, or any symlink on the way to it, lies outside the workspace (or, for a path inside a [named volume](volumes.md), outside that volume's directory) is refused with `escapes the workspace through a symbolic link`, including a symlink named directly as the path or a dangling one. The archive and extract steps also open the path beneath the workspace as they go, so a directory swapped for a symlink after the check is not followed either. A `native: true` job is not confined: it already runs as the agent user with no isolation boundary to protect, so a deliberate link such as `out -> /opt/build` keeps working. What an extraction writes follows the same split: for an isolated job it never writes through a symlink that leads outside the workspace (or the volume it targets), while a native job is only kept from writing through a symlink below its destination that leads outside that destination. A symlink that points inside the workspace works as part of a path as usual.

**Volumes.** A path *inside* a [named volume](volumes.md)'s mount path works as written (`path: .ucd-cache/go/build` archives the volume's real contents), on every agent. A path that is an *ancestor* of a volume's mount point (`.`, or `.ucd-cache` for a volume at `.ucd-cache/go`) archives everything around the volume but **excludes the volume's contents**, on both agent types, and says so in the run log:

```
artifact path "." excludes volume "go-cache" mounted at ".ucd-cache/go": the contents of a volume are never archived as part of a parent directory; name the volume's mount path to archive it
```

(`cache path ...` for a cache step; one line per volume.) A volume can hold tens of gigabytes, so it is never swept into the archive of its parent. Name the volume's own mount path to archive it deliberately. The same holds for extraction: a `downloadArtifact` with an ancestor `destDir` (or a cache restore of an ancestor path) skips any archived file that lies inside a volume, instead of writing it into the volume, and announces it (`artifact download path "." excludes volume ...: files of the artifact that lie inside a volume are not extracted into it`); make the volume's own mount path the `destDir` to extract into it deliberately. On the Kubernetes agent this uses a repeatable `--exclude` flag on the `unified-artifact` sidecar, so the sidecar image and the agent must be upgraded together (see [Operations: Upgrades](../../operator-manual/operations.md#upgrades)).

---

