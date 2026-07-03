# Design: k8s sidecar direct-to-S3 for cache + artifacts (unified-sidecar binary)

**Date:** 2026-07-04
**Status:** Approved (pending implementation plan)

## Problem

On the Kubernetes agent, `cache` steps are a **silent no-op**: `makeRunStep`
dispatches `approval` / `uploadArtifact` / `downloadArtifact` / `run`, but a
`cache` step matches none, falls through to the run branch with an empty script,
execs nothing, and reports **Succeeded**. Job authors believe caching happened
on k8s when nothing was cached — every k8s run is a cold build, and the misreport
is a correctness footgun.

Artifacts, by contrast, *do* work on k8s (via the `unified-artifact` sidecar
that runs `tar | zstd | curl` against the controller's artifact endpoint using
the agent token). But cache cannot reuse that path directly: cache logic
(content-addressed hashing, `restoreKeys` prefix fallback, `.meta` TTL) lives in
`internal/cache` and the standard agent talks **direct to S3**, not through the
controller.

## Goal

Make `cache` restore/save work on the k8s agent, and unify k8s cache **and**
artifacts on a single, consistent transport: a small `unified-sidecar` Go binary
that talks **direct to the S3-compatible object store**, with S3 credentials
supplied to the sidecar via an operator-provisioned Kubernetes **Secret**. This
matches the Argo Workflows / Tekton model (a trusted sidecar/executor with
store credentials mounted from a Secret) and reuses the existing, tested
`internal/cache` and `internal/artifact` logic with no reimplementation.

## Decision record (from brainstorming)

| Question | Decision |
|---|---|
| Transport for k8s cache + artifacts | **Sidecar direct-to-S3.** A `unified-sidecar` Go binary in the sidecar container reads S3 config from env and calls `internal/cache` / `internal/artifact` against the object store. |
| Credentials in the pod | **Accepted, via Secret.** S3 creds are mounted into the sidecar from an operator-provisioned Secret (`envFrom.secretRef`). Same threat model as Argo/Tekton: trusted sidecar image + container boundary. The job container cannot read the sidecar container's env. |
| Controller cache-proxy | **Not built.** Direct-S3 makes it unnecessary — the cache/artifact semantics run in the sidecar reusing `internal/cache`/`internal/artifact`. The controller is untouched. |
| k8s artifact path | **Reworked** from `tar\|zstd\|curl → controller` to `unified-sidecar artifact upload/download → S3 direct`. The object-store key layout is unchanged (`artifacts/{runID}/{name}.tar.gz`), so the human list/download API (and the standard agent's controller-mediated artifacts) keep working against the same keys. |
| Secret provisioning | **Operator-managed named Secret.** Agent config gains `sidecarS3SecretName`; the agent never handles raw S3 creds. `BuildPod` mounts that Secret into the sidecar. |
| Exec mechanism | **argv exec (no shell).** A new `ExecStepArgv` runs the sidecar binary as `argv` directly (no `bash -lc`), so key/path/name values are never interpolated into a shell string (removes the shell-quoting/injection surface) and the sidecar image needs no bash/curl/tar/zstd. |
| Standard agent | **Unchanged.** Cache stays agent-direct-to-S3; artifacts stay via the controller HTTP client. Only the **k8s sidecar** is unified on direct-S3. |
| Cache semantics on k8s | **Parity with the standard agent.** Restore at cache-step time (best-effort; a miss or error never fails the step; hit/miss logged). Save deferred to end-of-run (after main stages), capturing the final workspace. Save failures are logged, never fail the run. |

## Non-goals (YAGNI / separate)

- A controller cache-proxy (unnecessary under direct-S3).
- Changing the standard agent's cache or artifact transport.
- Changing the controller's artifact endpoints or the human list/download API.
- Per-key cache authorization / tenant isolation beyond what direct-S3 already
  gives (cache is content-addressed and cross-run shared today, unchanged).
- Rotating/short-lived S3 credentials (operator supplies a static Secret; STS/
  IRSA-style scoping is a documented future option).

## Architecture

```
 job container ──shares workspace volume──┐
                                          ▼
 unified-artifact sidecar  (image: unified-sidecar binary only, distroless)
   env from Secret: UNIFIED_S3_ENDPOINT/BUCKET/KEY/SECRET[/USE_SSL/REGION]
   k8s-agent execs (argv, no shell):
     unified-sidecar cache    restore --key K [--restore-key R]... --path P
     unified-sidecar cache    save    --key K --ttl-days N --path P
     unified-sidecar artifact upload   --run R --name N --path P
     unified-sidecar artifact download --run R --name N --dest D
        │
        ▼ minio client (direct)
   S3 object store  (caches/<hash>.tar.zst + .meta ; artifacts/{run}/{name}.tar.gz)
```

### 1. `cmd/unified-sidecar` — the binary

A thin CLI (cobra, matching the existing CLI style) with two command groups:

- `cache restore --key --restore-key(repeatable) --path` → build the S3 store
  from env, call `cache.Restore(ctx, store, path, key, restoreKeys)`; log
  hit/miss; **exit 0 on miss and on best-effort errors** (never a hard failure —
  cache restore must not fail the job step).
- `cache save --key --ttl-days --path` → `cache.Save(ctx, store, path, key, ttlDays)`;
  log; exit 0 even on error (best-effort, mirrors the standard agent's post-hook).
- `artifact upload --run --name --path` → `artifact.Upload(ctx, store, runID, name, path)`.
- `artifact download --run --name --dest` → `artifact.Download(ctx, store, runID, name, dest)`.
  Artifact upload/download **do** exit non-zero on failure (artifacts are not
  best-effort — a failed upload/download fails the step, matching current k8s
  artifact behavior).

The store is built via a new `objectstore.S3ConfigFromEnv()` reading
`UNIFIED_S3_ENDPOINT`, `UNIFIED_S3_BUCKET`, `UNIFIED_S3_KEY`, `UNIFIED_S3_SECRET`,
`UNIFIED_S3_USE_SSL` (bool, default false), `UNIFIED_S3_REGION` — the same names
the controller/agent already use. A missing required var is a clear startup error.

### 2. `internal/artifact` — store-level upload/download

The artifact package currently only extracts. Add store-level functions mirroring
`cache.Save`/`cache.Restore` but with the artifact key layout and no meta/TTL:

```go
// Upload tars+zstds path and stores it at artifacts/{runID}/{name}.tar.gz.
func Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, path string) error

// Download fetches artifacts/{runID}/{name}.tar.gz and extracts it into dest.
func Download(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string) error // reuses ExtractTarZstd
```

Factor the "tar+zstd a directory into a writer" walk (currently inline in
`cache.Save`) into a shared helper so `Upload` and `cache.Save` don't duplicate it.
`Download` reuses the existing `ExtractTarZstd`.

### 3. Sidecar image — `docker/artifact-sidecar.Dockerfile`

Rebuild as a minimal static-binary image: build `unified-sidecar` CGO-free, copy
into a distroless/`scratch` base with CA certificates. **No bash, curl, tar, or
zstd** (Go does tar+zstd; minio is the S3 client; argv-exec means no shell). The
image name stays `unified-cd-artifact-sidecar` (it now serves cache too; the
container name `unified-artifact` is kept for continuity).

### 4. Pod construction — `internal/k8sagent/podbuilder.go`

`SidecarSpec` changes from `{Image, Server, Token}` to `{Image, S3SecretName}`.
The injected sidecar container:
- keeps `Name: artifactSidecarName` ("unified-artifact") and the reserved-name guard;
- runs `sleep infinity` (unchanged, kept exec-able);
- drops the `UNIFIED_SERVER` / `UNIFIED_AGENT_TOKEN` env;
- gains `EnvFrom: [{SecretRef: {Name: sidecar.S3SecretName}}]` when `S3SecretName != ""`.

Injected before `injectWorkspace` (unchanged) so it still receives the workspace
mount via the all-containers loop.

### 5. Executor — `internal/k8sagent/executor.go`

Add `ExecStepArgv(ctx, podName, container string, argv []string, stdout, stderr io.Writer) (int, error)`
that execs `argv` directly (no `bash -lc` wrap). `ExecStep` stays for job `run`
steps (which are shell scripts). The sidecar is always invoked via `ExecStepArgv`.

### 6. Orchestrator — `internal/k8sagent/agent.go`

- The `artifactExec` seam changes from `func(ctx, container, script string)` to
  `func(ctx, container string, argv []string) (int, error)` (argv, not shell).
  Rename to `sidecarExec` (it now serves cache too). Production calls
  `a.exec.ExecStepArgv(ctx, podName, container, argv, io.Discard, stderrPusher)`.
- **Artifact branches**: replace the `tar|zstd|curl` script with argv, e.g.
  `[]string{"unified-sidecar","artifact","upload","--run",c.RunID,"--name",step.UploadArtifact.Name,"--path",path.Join(mountPath, step.UploadArtifact.Path)}`.
  Failure handling unchanged (non-zero ⇒ `recordFailure`).
- **Cache branch** (new, after the if:/approval gates, before run):
  - Expand `key := dsl.ExpandTemplate(step.Cache.Key, tplData)` and each
    `restoreKeys[i]` (tplData is already built in `makeRunStep`).
  - **Restore now**: `sidecarExec(execCtx, sidecar, ["unified-sidecar","cache","restore","--key",key,"--restore-key",r1,...,"--path",path.Join(mountPath, step.Cache.Path)])`.
    Report the step **Succeeded regardless** of exit code (best-effort; parity
    with the standard agent — a miss/error is logged, not a step failure).
  - **Register a deferred save**: append `{key, ttlDays, path}` to a
    `cacheSaves []cacheSaveSpec` slice on the orchestrator (guarded for the
    concurrent foreach case — appended under a mutex, or collected per-step and
    merged; see plan).
- **Run deferred saves at end-of-run**: after the main-stage loop and output
  promotion, before `finally`, exec `unified-sidecar cache save --key … --ttl-days … --path …`
  for each collected entry via `sidecarExec`. Failures are logged, never flip the
  run status (parity). Placed before finally so a build cache reflects the main
  stages' output; finally is cleanup/notify and should not perturb the cache.

### 7. Agent config — `internal/k8sagent/config.go` and `cmd/k8s-agent`

Add `SidecarS3SecretName string \`yaml:"sidecarS3SecretName"\``. Thread it into
the `SidecarSpec` where `BuildPod` is called. When empty, cache/artifact sidecar
transfers are unavailable, and the agent logs a **clear one-time startup warning**
so the misconfiguration is visible. **Cache steps remain best-effort even under a
missing Secret** — they are reported Succeeded (the sidecar's `cache restore`/`save`
just fail internally and are logged), consistent with cache's best-effort semantics
everywhere else; they never fail the run. **Artifact steps fail loudly** on the exec
error (the sidecar exits non-zero), so an operator using artifacts sees the
misconfiguration immediately, and the startup warning covers cache-only jobs.

### 8. Tests

- **`internal/artifact`** (unit, `LocalObjectStore`): `Upload`→`Download`
  round-trips a directory; key layout is `artifacts/{runID}/{name}.tar.gz`;
  path-traversal guard still enforced on download.
- **`cmd/unified-sidecar` / sidecar logic** (unit, `LocalObjectStore` via a
  `--endpoint`-injectable store or a testable `run(args, store)` seam): `cache
  save`→`cache restore` round-trip; restore miss exits 0; `artifact
  upload`→`download` round-trip; missing S3 env ⇒ clear error.
- **`internal/k8sagent`** (cluster-free): `podbuilder_test` asserts the sidecar
  has `EnvFrom` the named Secret and no `UNIFIED_SERVER`/`UNIFIED_AGENT_TOKEN`;
  `orchestrate_test` asserts a `cache` step execs `unified-sidecar cache restore …`
  argv into `unified-artifact` and registers a save that execs `cache save …`
  after the main stages; artifact steps exec `unified-sidecar artifact …` argv.
- **`//go:build k8s`** (real cluster, not run here): a pod round-trip — a run
  step writes a file, `cache save` stores it, a fresh pod's `cache restore`
  restores it; same for artifacts.

### 9. Docs

Update `docs/kubernetes-integration.md` and `docs/jobs.md`: k8s cache now works
via the `unified-artifact` sidecar talking **direct to S3**; the operator must
provision an S3 Secret and set `sidecarS3SecretName`; the sidecar holds
bucket-scoped S3 credentials (Secret-mounted; document the threat model and the
STS/IRSA future option); cache is best-effort (miss/error never fails a step).

## Touch points

| Path | Change |
|---|---|
| `cmd/unified-sidecar/main.go` (new) | the sidecar CLI (cache/artifact subcommands) |
| `internal/objectstore/env.go` (new) | `S3ConfigFromEnv()` |
| `internal/artifact/store.go` (new) | `Upload` / `Download` + shared tar+zstd-dir helper |
| `internal/cache/cache.go` | factor the tar+zstd-dir walk into the shared helper (reused by artifact.Upload) |
| `docker/artifact-sidecar.Dockerfile` | static `unified-sidecar` binary, distroless, no bash/curl/tar/zstd |
| `internal/k8sagent/podbuilder.go` | `SidecarSpec{Image, S3SecretName}`; `EnvFrom` Secret; drop server/token env |
| `internal/k8sagent/executor.go` | `ExecStepArgv` (no-shell argv exec) |
| `internal/k8sagent/agent.go` | `sidecarExec` (argv); rewrite artifact branches; add cache restore branch + deferred saves |
| `internal/k8sagent/config.go`, `cmd/k8s-agent` | `SidecarS3SecretName` threading |
| `internal/artifact/*_test.go`, `cmd/unified-sidecar/*_test.go`, `internal/k8sagent/*_test.go` | tests |
| `docs/kubernetes-integration.md`, `docs/jobs.md` | direct-S3 cache/artifact docs + Secret + threat model |

## Acceptance criteria

- A `cache` step on the k8s agent restores at step time and saves at end-of-run
  via the `unified-sidecar` binary talking direct to S3, reusing `internal/cache`
  (hash/restoreKeys/meta/TTL). A cache miss/error never fails the step.
- k8s artifact upload/download go through the same `unified-sidecar` binary
  direct to S3, at the unchanged `artifacts/{runID}/{name}.tar.gz` keys — the
  human list/download API keeps working for k8s-produced artifacts.
- The sidecar is invoked via argv (no shell); its image contains only the static
  binary + CA certs (no bash/curl/tar/zstd).
- S3 credentials reach the sidecar only via the operator-provisioned Secret
  (`sidecarS3SecretName`); the job container cannot read them. A missing Secret
  is surfaced by a one-time startup warning; artifact steps then fail loudly,
  while cache steps stay best-effort (Succeeded, no-op) — never silently
  mis-reporting a successful cache.
- The controller and standard agent are unchanged.
- Cluster-free unit tests cover the binary logic, the artifact store round-trip,
  the pod Secret injection, and the orchestrator's cache/artifact argv dispatch;
  a `//go:build k8s` test round-trips cache + artifact through a real pod.
- `go build ./...` and `go test ./...` pass.
