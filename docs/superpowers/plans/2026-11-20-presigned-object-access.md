# Run-scoped Object Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Job-running machines (the Kubernetes artifact sidecar and host agents) stop holding a bucket-wide S3 key. Instead they get 15-minute presigned URLs, one object and one operation each, from the controller, and only for their own run's artifacts and their own job's caches. Bulk data still moves directly between the client and the store.

**Architecture:** The controller gains a presign endpoint (`POST /api/v1/runs/{runID}/objects/presign`). It authenticates an agent bearer or a sidecar's projected ServiceAccount token, derives every object key itself, signs with minio-go, and does cache lookups and `.meta` writes with its own key. A shared client (`internal/objtransfer`) turns that endpoint into an `ObjectStore`-shaped artifact store and a `cache.Remote`. It uses multipart with growing part sizes, Range resume, and an automatic fall back to controller streaming when the store cannot presign (the local filesystem store). The sidecar and the host agent switch to that client. The store-credentials broker and the host agent's own cache key are removed.

**Tech Stack:** Go 1.24, minio-go v7 (`Presign`, `Core` multipart API), chi, Postgres advisory locks, Helm chart, Garage v2.3.0 for integration tests.

**Spec:** `docs/superpowers/specs/2026-11-20-presigned-object-access-design.md` (approved; its "Designer's calls" are binding).

## Global Constraints

- URL lifetime is a fixed 15 minutes: `objectstore.PresignExpiry = 15 * time.Minute`, which is `X-Amz-Expires=900` on every URL. Nothing is configurable.
- Each URL covers exactly one object key and one operation: GET, HEAD, PUT, or one `UploadPart` (`partNumber` + `uploadId`). The controller never presigns a key ending in `.meta`.
- The client never sends a raw object key. The controller derives it: artifacts as `artifact.ArtifactKey(runID, name)`, caches as `cache.DataKey(jobName, key)`. The job name always comes from the run record (`store.RunJobName`), never from the client.
- Part sizing: `PartSize(n) = min(64 MiB << ((n-1)/1000), 5 GiB)`. That is 64 MiB, doubling every 1,000 parts, with at most 10,000 parts. A stream that ends inside the first part is uploaded as one PUT.
- If multipart presign is unavailable (the GCS fallback), the client spools the stream to a temp file in its temp dir and does one PUT with a known Content-Length.
- Abandoned multipart uploads: the controller aborts them on run end and in a leader-elected sweep (new advisory-lock key `0x6d707274` 'mprt', hourly) that aborts uploads initiated more than 24h ago.
- Clients retry with 6 attempts and a 10s cap per delay (the existing broker policy, moved to `objtransfer`). There is one exception: `409 binding-pending` is retried for up to 30s.
- Host agent setting: `objectTransfer: direct | controller` (default `direct`; env `UNIFIED_OBJECT_TRANSFER`; flag `--object-transfer`).
- Controller setting: `objectStore.artifactDownload: proxy | redirect` (default `proxy`; env `UNIFIED_ARTIFACT_DOWNLOAD`; flag `--artifact-download`).
- Exact error strings:
  - host agent: `agent config: the cache store flags (--cache-endpoint, --cache-key, --cache-secret, --cache-bucket) are no longer supported; caches use controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)`
  - controller: `objectStore.artifactDownload: redirect requires an S3-compatible object store`
  - k8s agent (this plan's ruling): `k8s-agent config: sidecarS3SecretName and sidecarS3SecretMode are no longer supported; the artifact sidecar uses controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)`
  - old sidecar against the new controller (quoted in the migration guide): `artifact requires S3 configuration (UNIFIED_S3_*): store credentials broker <url>: store credentials request failed: 404 Not Found: 404 page not found`
- Removed: `POST /api/v1/store-credentials`, `objectstore.BrokerConfig`/`brokerProvider`, the `UNIFIED_S3_BROKER_*` env and the `--broker-run-id` sidecar flag, `StoreCredentialRequireRunBinding` (still accepted, logs a deprecation warning, and is ignored), the host agent's `--cache-endpoint/--cache-key/--cache-secret/--cache-bucket` (and their env and yaml keys; set means refuse to start), and the k8s agent's `sidecarS3SecretName`/`sidecarS3SecretMode` (set means refuse to start).
- Kept, with unchanged names: `agentAuth.kubernetesStoreCredentialPolicies`, `StoreCredentialRateLimit` (`UNIFIED_STORE_CREDENTIAL_RATE_*`), `api.KubernetesStoreCredentialAudience`. They now govern the presign endpoint, and the docs say so.
- Repo rules: English only; no PII in code, docs or commits; every task updates the docs its change makes wrong, in the same commit (grep for every renamed or removed name before committing).
- Commit with `git -c user.name=eirueimi -c user.email="128969149+eirueimi@users.noreply.github.com" commit`. Every message ends with the two lines:
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`
  `Claude-Session: https://claude.ai/code/session_01G3NWwwXnjYtSfzJjZYTrnV`

## Review Focus

These are the five failure modes most likely to bite a user that the spec-mandated tests would not catch. Each is pinned by a named test in its owning task.

1. **Run end aborts a concurrent run's cache upload.** Two runs of one job share a cache prefix. Pinned by `TestAbortRunUploads_SkipsCachePrefixWhileAnotherRunOfJobIsActive` (Task 3).
2. **A pooled Pod's first step races the binding heartbeat.** The binding reaches the controller only on the next heartbeat (15s), which is longer than the old 6s retry window. Pinned by `TestClient_RetriesBindingPendingUpTo30s` (Task 4) and `TestExecuteRun_HeartbeatKickedAfterPodBinding` (Task 6).
3. **The k8s agent still fails fast for a missing `sidecarS3SecretName`, so every artifact step fails after the upgrade.** Pinned by `TestExecuteRun_ArtifactStepsNoLongerNeedSidecarSecret` (Task 6).
4. **Range resume stitches together two versions of an object** (an artifact re-uploaded mid-download). Pinned by `TestResumingReader_ObjectReplacedMidDownloadFails` (Task 4).
5. **Redirect mode leaks into machines.** An agent in `controller` mode receives a 302 to a store it cannot reach, or the CLI forwards its bearer to a store on a subdomain. Pinned by `TestArtifactDownload_RedirectModeStillProxiesAgents` and `TestArtifactDownload_CLIDropsAuthorizationOnSubdomainRedirect` (Task 9).

---

### Task 1: Store signing primitives, controller TLS/region, Garage harness, GCS finding

**Files:**
- Modify: `internal/objectstore/objectstore.go` (add interfaces after line 39), `internal/objectstore/s3.go` (methods after `List`, ~line 138), `internal/objectstore/local.go` (add `Stat`)
- Create: `internal/objectstore/presign.go`, `internal/objectstore/presign_test.go`, `internal/objectstore/garage_test.go` (`//go:build garage`), `internal/objectstore/gcs_test.go` (`//go:build gcs`), `internal/objectstore/objectstoretest/s3env.go`
- Modify: `cmd/controller/main.go:205-208,385-402`, `internal/config/controller.go` (`S3UseSSL`, `S3Region` next to `S3Secret`, ~line 140; env at ~271), `.github/workflows/ci.yml` (new `garage` job), `CONTRIBUTING.md` (~line 71 test-tag table)

**Interfaces:**
- Produces:
  - `const PresignExpiry = 15 * time.Minute`
  - `type ObjectInfo struct{ Key string; Size int64; ETag string; LastModified time.Time }`
  - `type Statter interface{ Stat(ctx context.Context, key string) (ObjectInfo, error) }`: returns `ErrNotFound` if the key is missing. Implemented by S3 and Local.
  - `type Upload struct{ Key, UploadID string; Initiated time.Time }`
  - `type CompletedPart struct{ PartNumber int; ETag string }`
  - `type Presigner interface { Statter; PresignGet(ctx, key, filename string) (*url.URL, error); PresignHead(ctx, key string) (*url.URL, error); PresignPut(ctx, key string) (*url.URL, error); PresignUploadPart(ctx, key, uploadID string, part int) (*url.URL, error); StartMultipart(ctx, key string) (string, error); CompleteMultipart(ctx, key, uploadID string, parts []CompletedPart) error; AbortMultipart(ctx, key, uploadID string) error; ListMultipartUploads(ctx, prefix string) ([]Upload, error); MultipartPresign() bool }`. Only `*S3ObjectStore` implements it. `PresignGet` with a non-empty filename sets `response-content-disposition=attachment; filename="<filename>"`.
  - `S3Config.MultipartPresign bool`, set by `MultipartPresignSupported(endpoint string) bool`.
  - Controller config: `s3UseSSL`/`UNIFIED_S3_USE_SSL`/`--s3-use-ssl` and `s3Region`/`UNIFIED_S3_REGION`/`--s3-region`. Presigned URLs travel to clients, so S3 and GCS need `https` and a known region (GCS: `auto`).

- [ ] **Write failing tests** in `presign_test.go`. Use a store built by an unexported `newS3ForTest(endpoint, bucket, region)` that skips `BucketExists`; with a region set, presigning needs no network.
  - `TestPresign_EachURLIsOneObjectOneOp_15Minutes`: for each Presign* method, the URL path is exactly `/<bucket>/<key>`, `X-Amz-Expires` is `900`, and `X-Amz-SignedHeaders` is `host`. The UploadPart URL carries `partNumber=3&uploadId=U`. GET carries no `partNumber`.
  - `TestPresignGet_SetsContentDisposition`: `response-content-disposition` is `attachment; filename="out.tar.gz"`.
  - `TestMultipartPresignSupported`: `storage.googleapis.com` gives the value recorded by the GCS finding below (default `false`); `s3.amazonaws.com` and `garage:3900` give `true`.
  - `TestLocalStat_NotFoundAndModTime`: errors with `errors.Is(err, ErrNotFound)` when missing, and returns `Size`/`LastModified` when present.
  - `garage_test.go` (`//go:build garage`), using `objectstoretest.S3FromEnv(t)`, which skips unless `UNIFIED_TEST_S3_ENDPOINT/BUCKET/KEY/SECRET` are set:
    - `TestGarage_PresignedPutGetHead`: a plain `http.Client` PUTs `hello` to the PUT URL, then GET returns `hello` and HEAD returns 200.
    - `TestGarage_PresignedMultipartTwoParts`: StartMultipart, PUT part 1 (5 MiB) and part 2 (1 byte) through presigned URLs, CompleteMultipart with their ETags; `Stat.Size` is `5 MiB + 1`.
    - `TestGarage_AbortAndList`: a started upload appears in `ListMultipartUploads(prefix)` and is gone after Abort.
    - `TestGarage_RangeGet`: GET with `Range: bytes=2-` on `hello` returns 206 and `llo`.
  - `gcs_test.go` (`//go:build gcs`): the same two-part multipart flow against `storage.googleapis.com` (SSL, region `auto`), skipping unless `UNIFIED_TEST_GCS_BUCKET/KEY/SECRET` are set.
- [ ] **Run to fail:** `go test ./internal/objectstore/ -run 'Presign|MultipartPresignSupported|LocalStat' -count=1`
- [ ] **Implement:**
  - `Presign*` delegate to `client.Presign(ctx, method, bucket, key, PresignExpiry, params)`.
  - Multipart methods use `minio.Core{Client: s.client}`: `NewMultipartUpload`, `CompleteMultipartUpload`, `AbortMultipartUpload`, `ListMultipartUploads` (page through `NextKeyMarker`/`NextUploadIDMarker`).
  - `Stat` uses `StatObject` and maps `NoSuchKey`/404 to `ErrNotFound`.
  - The controller builds `S3Config{UseSSL, Region, MultipartPresign: MultipartPresignSupported(endpoint)}`.
- [ ] **Run to pass:** the same command; then start Garage and run the integration tests:
  ```
  docker run -d --rm --name ucd-garage-test -p 3900:3900 -e GARAGE_RPC_SECRET=$(printf '0%.0s' {1..64}) \
    -e GARAGE_DEFAULT_ACCESS_KEY=garageadmin -e GARAGE_DEFAULT_SECRET_KEY=garageadmin12345 -e GARAGE_DEFAULT_BUCKET=ucd-test \
    -v "$PWD/deployments/garage/garage.toml:/etc/garage.toml:ro" dxflrs/garage:v2.3.0 /garage server --single-node --default-bucket
  UNIFIED_TEST_S3_ENDPOINT=localhost:3900 UNIFIED_TEST_S3_BUCKET=ucd-test UNIFIED_TEST_S3_KEY=garageadmin \
    UNIFIED_TEST_S3_SECRET=garageadmin12345 go test -tags garage ./internal/objectstore/ -count=1 -v
  ```
- [ ] **CI:** add a `garage` job to `.github/workflows/ci.yml` that runs the `docker run` above, waits for `docker exec ucd-garage-test /garage status`, then runs `go test -tags garage ./internal/objectstore/... -count=1` (Task 4 adds `./internal/objtransfer/...`). With the env set, a skip is a failure: pass `-run Garage -v` and fail the step on `--- SKIP`.
- [ ] **GCS finding:** if GCS HMAC credentials are available, run `UNIFIED_TEST_GCS_BUCKET=… UNIFIED_TEST_GCS_KEY=… UNIFIED_TEST_GCS_SECRET=… go test -tags gcs ./internal/objectstore/ -run GCS -v`. Record the outcome, date and command in `docs/operator-manual/operations.md` under a new "Object store compatibility" section. If it passes, set the GCS branch of `MultipartPresignSupported` to `true`. If it fails or cannot be run, keep `false` (single-PUT spool fallback, up to GCS's 5 TiB object limit) and record "not yet verified" with the command an operator runs to check.
- [ ] **Docs:**
  - `docs/reference/configuration.md`: `s3UseSSL`, `s3Region`.
  - `operations.md`: the object store compatibility table (S3: multipart presign; Garage: verified by the `garage` CI job; GCS: per the finding).
  - `CONTRIBUTING.md`: rows for the `garage` and `gcs` tags with the commands above.
- [ ] **Commit:** `feat(objectstore): presign and multipart primitives, Garage integration harness`

### Task 2: Cache lookups and `.meta` writes become store-side helpers

**Files:**
- Modify: `internal/cache/cache.go` (whole file; `save` ~91, `restore` ~207, `DeleteExpired` ~251, `findBestMatch` ~292), `internal/cache/cache_test.go`, `internal/cache/exclude_test.go`
- Modify callers so they keep today's behaviour: `cmd/unified-sidecar/run.go:139-207` (`runCache`), `internal/agent/backend_host.go:216-302`

**Interfaces:**
- Produces:
  - `type Remote interface { Open(ctx context.Context, key string, restoreKeys []string) (rc io.ReadCloser, matchedKey string, err error); Save(ctx context.Context, key string, ttlDays int, body io.Reader) error }`: `Open` returns `ErrCacheMiss` when nothing matches.
  - `type StoreRemote struct{ Store objectstore.ObjectStore; JobName string }`: the controller-side `Remote` (and, until Tasks 6 and 7, the clients' own).
  - `func DataKey(jobName, key string) string` (`objectKey+".tar.zst"`)
  - `func JobPrefix(jobName string) string`
  - `func Lookup(ctx, store, jobName, key string, restoreKeys []string) (dataKey, matchedKey string, err error)`
  - `func Commit(ctx, store, jobName, key string, size int64, expiresAt time.Time) error`
  - `const OrphanGrace = 24 * time.Hour`
  - Signatures lose `store, jobName` and take `remote Remote` instead: `Save/SaveExcluding/SaveIn(ctx, remote, [root,] path, key, ttlDays, exclude)` and `Restore/RestoreExcluding/RestoreWritten/RestoreInWritten/RestoreIn(ctx, remote, [root,] path, key, restoreKeys, exclude[, w])`. `RestoreIn` also returns `matchedKey`.

- [ ] **Write failing tests:**
  - `TestLookup_ExactHitRequiresMeta`: a data object with no `.meta` is a miss (`ErrCacheMiss`). The same data object with `.meta` (`OwnerJob` = job) is a hit and `matchedKey == key`.
  - `TestLookup_ExactHitRejectsForeignOwner`: `.meta.OwnerJob` = `other` gives a miss.
  - `TestLookup_RestoreKeysPicksLongestTTL`: today's `findBestMatch` behaviour, through `Lookup`.
  - `TestCommit_WritesMetaFromArgs`: the decoded `.meta` has `OriginalKey`, `OwnerJob`, `Size=42` and `ExpiresAt` equal to what was passed.
  - `TestDeleteExpired_RemovesOrphansOlderThanGrace`: with a `Statter` store, a `.tar.zst` without `.meta` and `LastModified` 25h ago is deleted, and one 1h old is kept. A store without `Statter` keeps both.
  - `TestStoreRemote_RoundTrip`: `SaveIn` then `RestoreIn` through `&StoreRemote{…}` restores the tree. This is the existing round-trip test, ported.
- [ ] **Run to fail:** `go test ./internal/cache/ -count=1`
- [ ] **Implement:**
  - `Lookup`: exact match first (`.meta` present and `OwnerJob` matching; expiry is not checked, as today), then the restoreKeys scan.
  - `StoreRemote.Save`: put with the counting reader, then `Commit(size=counted, now+ttlDays)`.
  - `StoreRemote.Open`: `Lookup`, then `Store.Get(dataKey)`.
  - Orphans are found by listing `caches/`, subtracting the `.meta` stems, and calling `Stat` on the rest.
  - Callers wrap the store in `&cache.StoreRemote{Store: store, JobName: job}`.
- [ ] **Run to pass:** `go test ./internal/cache/ ./cmd/unified-sidecar/ ./internal/agent/ -count=1`
- [ ] **Docs:**
  - `docs/contributing/background-workers.md` (cache cleanup now also removes data objects that have had no `.meta` for 24h).
  - `docs/user-guide/writing-jobs/artifacts-and-cache.md`: only if it describes exact-key hits without metadata (grep `meta`).
- [ ] **Commit:** `refactor(cache): store-side Lookup/Commit and a Remote seam; GC removes orphaned archives`

### Task 3: Controller presign endpoint and multipart lifecycle

**Files:**
- Create:
  - `internal/api/objects.go`
  - `internal/controller/object_requester.go`: requester auth
  - `internal/controller/api_objects.go`: the presign handler
  - `internal/controller/multipart_sweep.go`
  - Tests: `internal/controller/api_objects_test.go`, `object_requester_test.go`, `multipart_sweep_test.go`, `cachens_test.go`
- Modify:
  - `internal/controller/server.go`: a `presigner objectstore.Presigner` field, set by type-asserting `objStore` in `NewServer` (~215); route registration near 685-700
  - `internal/controller/authz_routes.go`: an `unauthenticated(mPost, "/api/v1/runs/{runID}/objects/presign")` row with a comment that the handler authenticates
  - `internal/controller/api_agent.go:1026` (`handleAgentFinishRun`: call `s.abortRunUploads` after a successful finish)
  - `cmd/controller/main.go` (start `RunMultipartSweep` next to `RunCacheCleanup` when `obj` is a `Presigner`)

**Interfaces:**
- Consumes: `objectstore.Presigner` (Task 1); `cache.Lookup/Commit/DataKey/JobPrefix` (Task 2); `s.agentRunGuard(ctx, agentID, runID, false)` (`agent_guard.go:98`); `s.verifyStoreCredentialToken` (`api_store_credentials.go`); `store.GetRunPodBinding`, `RunJobName`, `GetRunSpec`, `ListRunsByJob`.
- Produces (JSON):
  ```go
  // POST /api/v1/runs/{runID}/objects/presign
  type PresignRequest struct {
      Kind        string       `json:"kind"`                  // "artifact" | "cache"
      Op          string       `json:"op"`                    // get|head|put|multipart-start|multipart-parts|multipart-complete|multipart-abort|commit
      Name        string       `json:"name,omitempty"`        // artifact name
      SourceRunID string       `json:"sourceRunId,omitempty"` // artifact get/head from another run
      Key         string       `json:"key,omitempty"`         // cache key as written in the job (unhashed)
      RestoreKeys []string     `json:"restoreKeys,omitempty"` // cache get
      UploadID    string       `json:"uploadId,omitempty"`
      PartNumbers []int        `json:"partNumbers,omitempty"` // 1..10000, at most 100 per request
      Parts       []ObjectPart `json:"parts,omitempty"`       // multipart-complete
      TTLDays     int          `json:"ttlDays,omitempty"`     // cache commit
  }
  type ObjectPart struct{ PartNumber int `json:"partNumber"`; ETag string `json:"etag"` }
  type PresignedRequest struct {
      Method string `json:"method"`; URL string `json:"url"`
      Headers map[string]string `json:"headers,omitempty"` // must be sent as-is
      PartNumber int `json:"partNumber,omitempty"`; ExpiresAt time.Time `json:"expiresAt"`
  }
  type PresignResponse struct {
      Mode string `json:"mode"` // "direct" | "controller"
      Request *PresignedRequest `json:"request,omitempty"`
      Parts []PresignedRequest `json:"parts,omitempty"`
      UploadID string `json:"uploadId,omitempty"`
      MatchedKey string `json:"matchedKey,omitempty"`        // cache get: the entry's original key
      Size int64 `json:"size,omitempty"`; ETag string `json:"etag,omitempty"` // get/head
  }
  type ObjectError struct{ Error string `json:"error"`; Code string `json:"code"` }
  ```
  - Status codes and error codes: 400 `invalid`; 401 `unauthenticated`; 403 `forbidden`; 404 `not-found` (artifact) or `cache-miss`; 409 `binding-pending` with `Retry-After: 1`; 429 `rate-limited` with `Retry-After: 1`; 501 `multipart-unavailable`; 503 `no-object-store`.
  - When `s.presigner == nil` but `objStore != nil`, every authorized request gets `200 {"mode":"controller"}`.
  - `func (s *Server) abortRunUploads(ctx context.Context, runID, jobName string)`
  - `func RunMultipartSweep(ctx context.Context, st store.Store, p objectstore.Presigner, interval time.Duration)`
  - `const multipartSweepLockKey = int64(0x6d707274) // 'mprt'`

- [ ] **Write failing tests** (Postgres-backed like `api_artifacts_test.go`; a fake `Presigner` that records calls and returns `https://store/<key>?op=<op>` URLs):
  - Requester auth (`object_requester_test.go`):
    - `TestPresign_AgentMustOwnRun`: another agent's run gives 403; an unknown run gives 404.
    - `TestPresign_SidecarWithoutBindingGets409`: a verified token with no `GetRunPodBinding` row gives 409, `code=binding-pending`, `Retry-After: 1`.
    - `TestPresign_SidecarBoundToOtherPodGets403`.
    - `TestPresign_SidecarNamespaceNotAllowedGets403`.
    - `TestPresign_RequireRunBindingSettingIsIgnored`: with `RequireRunBinding=false`, an unbound sidecar still gets 409.
  - Scope:
    - `TestPresign_ArtifactPutOnlyOwnRun`: `put` for `name=a` signs exactly `artifacts/<runID>/a.tar.gz`; a request carrying `sourceRunId` on `put` gives 400.
    - `TestPresign_ArtifactGetCrossRun`: `get` with `sourceRunId=<other existing run>` signs `artifacts/<other>/a.tar.gz` and sets the filename to `a.tar.gz`; a missing source run gives 404.
    - `TestPresign_CacheNamespaceFromRunRecord`: the request has no job field; the signed key equals `cache.DataKey(<run.JobName>, "k")`.
    - `TestPresign_NeverSignsMeta`: no op on any kind ever produces a URL whose key ends in `.meta` (iterate every op).
  - Ops:
    - `TestPresign_CacheGetMissAndHit`: a miss gives 404 `cache-miss`; a restoreKeys hit gives `matchedKey`, `size` and `etag` with a GET URL.
    - `TestPresign_MultipartPartsBatchLimit`: 101 part numbers give 400; 3 give 3 `parts` with `partNumber` 1..3.
    - `TestPresign_MultipartUnavailable`: with `MultipartPresign()=false`, `multipart-start` gives 501 `multipart-unavailable`.
    - `TestPresign_CacheCommitWritesMetaServerSide`: after the fake store holds the data object (size 42), `commit` with `ttlDays=400` writes `.meta` with `Size=42`, the run record's `OwnerJob`, and `ExpiresAt` = now + min(400, maxDeclared) days. The run spec declares `ttlDays: 7`, so expiry is 7 days. With no data object, the response is 404.
    - `TestPresign_LocalStoreAnswersControllerMode`: with a `LocalObjectStore`, the response is `{"mode":"controller"}`.
    - `TestPresign_TerminalRunStillAcceptsUpload`, mirroring `handleArtifactUpload`.
    - `TestPresign_RateLimitedPerRequester`: burst 1, so the second call gives 429.
  - Audit: `TestPresign_AuditsWritesAndDenials`: `put`, `multipart-start` and `commit` write `objects.presign` rows; `get` writes none; a 403 writes a row.
  - Cache namespace parity (`cachens_test.go`): `TestClaimJobNameEqualsRunRecord_CallChildAndFolderJob`. `buildClaimResponse(...).JobName == store.RunJobName(runID)` holds for a `call:` child run and for a folder-qualified job `team-a/build`. If this fails, stop and report: the migration guide then needs the one-time cache-miss note (spec call 3).
  - Lifecycle (`multipart_sweep_test.go`):
    - `TestAbortRunUploads_AbortsRunArtifactsAndJobCaches`.
    - `TestAbortRunUploads_SkipsCachePrefixWhileAnotherRunOfJobIsActive` (Review Focus 1): its artifact prefix is still aborted.
    - `TestMultipartSweep_AbortsOnlyOlderThan24h`: uploads initiated 25h ago are aborted, 23h ago kept.
    - `TestMultipartSweep_LeaderOnly`: when the lock is held, nothing happens.
  - Route tables: `TestEveryRegisteredRouteIsClassified` and `TestAuditTablesMatchRouteTable` pass with the new row.
- [ ] **Run to fail:** `go test ./internal/controller/ -run 'Presign|AbortRunUploads|MultipartSweep|ClaimJobName|RouteTable|Classified' -count=1`
- [ ] **Implement:**
  - `authenticateObjectRequester(w, r, runID) (objectRequester{AgentID, PodUID string}, bool)`:
    - A bearer `uca_…` goes through the existing `agentAuth` logic (factor its body into `s.authenticateAgentBearer(r)`). The rate limit is keyed on the agent ID.
    - Any other bearer: rate-limit by client address first, then `verifyStoreCredentialToken` and the namespace/ServiceAccount allow-list. Then the binding check: no row gives 409, a mismatched `PodUID` gives 403. `RequireRunBinding` is never consulted.
    - Agents then pass `agentRunGuard(..., false)` plus `GetRun` exists (mirroring `handleArtifactUpload`). Sidecars pass `GetRun` exists.
  - TTL: `maxDeclared` = the largest `cache.ttlDays` across every step and finally step of `GetRunSpec`, recursing into nested groups as `dsl` validation does. A value of 0 counts as 30; the cap is 365. A requested value of ≤0 means `maxDeclared`.
  - `abortRunUploads`: a `go` routine with a 1-minute timeout. It lists `artifacts/<runID>/` and `cache.JobPrefix(job)`. The cache prefix is skipped if `ListRunsByJob(job, 100)` holds another non-terminal run.
  - Sweep: every hour, behind the advisory lock, prefixes `artifacts/` and `caches/`, with `metrics.ObservePass("multipart_sweep", …)`.
- [ ] **Run to pass:** the same command, then `go test ./internal/controller/ -count=1`.
- [ ] **Docs:**
  - `docs/operator-manual/audit.md`: the `objects.presign` action.
  - `docs/contributing/auth-implementation.md`: the requester section (agent bearer or projected token; binding mandatory).
  - `docs/contributing/background-workers.md`: the multipart sweep, key `'mprt'`, hourly, 24h.
- [ ] **Commit:** `feat(controller): run-scoped presign endpoint with server-side cache metadata and multipart cleanup`

### Task 4: Shared presign client (`internal/objtransfer`)

**Files:**
- Create: `internal/objtransfer/client.go`, `upload.go`, `download.go`, `retry.go`, `client_test.go`, `upload_test.go`, `download_test.go`, `garage_test.go` (`//go:build garage`, end to end against Garage, with a real controller handler over `httptest`)
- Modify: `internal/artifact/store.go` (add `ParseKey`, ~line 94)

**Interfaces:**
- Consumes: the Task 3 endpoint and JSON; `cache.Remote` (Task 2); `objectstore.ErrNotFound`.
- Produces:
  - `type Client struct { BaseURL, RunID string; Auth func(context.Context, *http.Request) error; HTTP *http.Client; TempDir string; ForceController bool }`
  - `func (c *Client) Artifacts() objectstore.ObjectStore`: `Put`/`Get` parse `artifacts/<run>/<name>.tar.gz` with `artifact.ParseKey`. `Get` from another run sends `sourceRunId`. `Delete`/`List` return `errors.ErrUnsupported`.
  - `func (c *Client) Caches() cache.Remote`
  - `func PartSize(n int) int64`; `const MinPartSize = 64 << 20; MaxPartSize = 5 << 30; MaxParts = 10000; partURLBatch = 10; bindingWait = 30 * time.Second`
  - `func artifact.ParseKey(key string) (runID, name string, err error)`
  - In this task, the controller-mode branch only returns `errControllerMode` when the server answers `{"mode":"controller"}` or `ForceController` is set. Task 5 fills it in.

- [ ] **Write failing tests** (a fake controller and a fake S3 on `httptest`; the fake S3 enforces method+path, counts requests, and returns an `ETag` per part):
  - `TestPartSize`: `PartSize(1)=64 MiB`, `PartSize(1000)=64 MiB`, `PartSize(1001)=128 MiB`, `PartSize(2001)=256 MiB`, `PartSize(9001)=5 GiB` (capped).
  - `TestUpload_SmallStreamIsSinglePut`: 1 KiB gives one `put` presign, one PUT with `Content-Length: 1024`, and no multipart ops.
  - `TestUpload_MultipartGrowingParts`: with `MinPartSize` overridden to 4 bytes and the doubling period to 2 parts via unexported vars, a 30-byte stream gives parts of 4,4,8,8,6 bytes and a complete call carrying all five ETags in order.
  - `TestUpload_ReRequestsPartURLOn403`: the fake S3 answers 403 once for part 2; the client asks `multipart-parts` for `[2]` again and re-sends identical bytes from its part temp file.
  - `TestUpload_AbortsOnFailure`: a body read error mid-stream sends `multipart-abort`.
  - `TestUpload_MultipartUnavailableSpoolsToTempFile`: a 501 on `multipart-start` gives one PUT with `Content-Length` equal to the full size, and the temp file is removed afterwards.
  - `TestUpload_TempFilesStayInTempDir`: during the upload, the files live in `c.TempDir`, and it is empty afterwards.
  - `TestCacheSave_EndsWithCommit`: the op sequence is `…put|multipart-complete` then `commit` with `ttlDays`.
  - `TestResumingReader_RangeResumeAfterDrop`: the fake S3 cuts the body at byte 10 of 20. The client re-requests `get` and sends `Range: bytes=10-` and `If-Match: <etag>`; the bytes read equal the original.
  - `TestResumingReader_ObjectReplacedMidDownloadFails` (Review Focus 4): the resumed GET answers 412, so the read fails with an error containing `changed during download`.
  - `TestClient_RetriesBindingPendingUpTo30s` (Review Focus 2): 409 `binding-pending` three times and then 200 succeeds; constant 409s fail after the overridden `bindingWait`, with the error naming `binding-pending`.
  - `TestClient_Retries429WithRetryAfter`: this is the ported broker test.
  - `TestCaches_MissIsErrCacheMiss`: a 404 `cache-miss` gives `errors.Is(err, cache.ErrCacheMiss)`.
  - `TestArtifacts_GetNotFoundIsErrNotFound`.
  - `garage_test.go`: `TestGarage_ClientEndToEnd` runs a 70 MiB stream (two parts at the real sizes), a cache save then restore by restoreKeys prefix, and an artifact round trip through a real `controller.Server` on `httptest` with Postgres and a Garage `S3ObjectStore`.
- [ ] **Run to fail:** `go test ./internal/objtransfer/ ./internal/artifact/ -count=1`
- [ ] **Implement:**
  - Upload: fill part N into a temp file up to `PartSize(N)`. If EOF arrives during part 1, do a single `put`. Otherwise `multipart-start`, fetch part URLs in batches of 10, and re-fetch a URL when it is within 1 minute of `expiresAt` or answered 403.
  - Download: a `resumingReader` that tracks the offset, re-presigns on error, and uses `Range` + `If-Match`.
  - Retry and backoff: move `parseRetryAfter`, `throttledError` and the sleep var out of `broker.go` (copy now; Task 8 deletes the originals).
- [ ] **Run to pass:** the same command; Garage as in Task 1 with `./internal/objtransfer/`.
- [ ] **Docs:** none user-facing (library). Add the Garage path to the CI job's test list.
- [ ] **Commit:** `feat(objtransfer): presigned-URL client with growing multipart parts and Range resume`

### Task 5: Controller-mode streaming

**Files:**
- Create: `internal/controller/api_object_stream.go`, `internal/controller/api_object_stream_test.go`, `internal/objtransfer/controller_mode.go`, `internal/objtransfer/controller_mode_test.go`
- Modify: `internal/controller/server.go` (routes), `authz_routes.go` (4 `unauthenticated` rows), `cmd/controller/main.go` (log once when `obj` is not a `Presigner`)

**Interfaces:**
- Produces routes. All four are authenticated by `authenticateObjectRequester` and carry the same scope rules as Task 3.
  - `PUT /api/v1/runs/{runID}/objects/caches?key=<k>&ttlDays=<n>`: the body is the archive. The controller stores it and commits `.meta` server-side, giving 204.
  - `GET /api/v1/runs/{runID}/objects/caches?key=<k>&restoreKey=<p>…`: 200 with header `X-Unified-Cache-Key: <matchedKey>`, or 404 `cache-miss`.
  - `PUT /api/v1/runs/{runID}/objects/artifacts/{name}`: own run only, giving 204.
  - `GET /api/v1/runs/{runID}/objects/artifacts/{name}?sourceRunId=<id>`: 200, or 404.
  - Body limit: the same as the existing artifact upload route (`requestBodyLimit`).
- Startup log (exact): `object store has no presigning (local filesystem): all agent and sidecar transfers stream through the controller`.
- The client fills in `errControllerMode`. With `ForceController` or `mode=controller`, `Artifacts()` and `Caches()` use these routes. The decision is remembered per `Client`.

- [ ] **Write failing tests:**
  - `TestObjectStream_CachePutCommitsMetaServerSide`: after a PUT, `.meta` exists with `OwnerJob` = the run's job and `Size` = the body length; a client-supplied job is impossible because there is no such field.
  - `TestObjectStream_CacheGetRestoreKeyHeader`.
  - `TestObjectStream_ArtifactPutOtherRunForbidden`.
  - `TestObjectStream_SidecarAuth409WithoutBinding`.
  - `TestControllerMode_AutoFromPresignAnswer`: against a real `controller.Server` with a `LocalObjectStore`, `Artifacts().Put` then `Get` round-trips, and a cache save then restore works, with no requests to any store URL.
  - `TestControllerMode_ForcedSkipsPresign`: with `ForceController=true`, the presign endpoint is never called.
- [ ] **Run to fail:** `go test ./internal/controller/ -run ObjectStream -count=1 && go test ./internal/objtransfer/ -run ControllerMode -count=1`
- [ ] **Implement:** the handlers reuse `cache.StoreRemote{Store: s.objStore, JobName: job}` (`Save` with the TTL rule from Task 3) and `s.objStore.Put/Get`. The client uses a streaming PUT with chunked transfer encoding, like `client.uploadArtifactBody`.
- [ ] **Run to pass:** the same commands.
- [ ] **Docs:** `docs/operator-manual/operations.md`: the local filesystem store forces controller streaming, with the log line.
- [ ] **Commit:** `feat: controller-streamed cache and artifact transfers for stores without presigning`

### Task 6: Sidecar and Kubernetes agent use presigned URLs

**Files:**
- Modify:
  - `cmd/unified-sidecar/main.go` (replace `s3ConfigFromEnv`); `cmd/unified-sidecar/run.go` (`storeProvider` becomes a provider of `*objtransfer.Client`; replace `extractBrokerRunID` with `extractPodRun` for `--pod-run`; `--job` becomes optional and is used only for logging; `cache restore` logs `matchedKey`)
  - `cmd/unified-sidecar/*_test.go`; delete `s3config_test.go`
  - `internal/k8sagent/podbuilder.go:54-240`: always mount the projected token (the old broker branch becomes the only branch); env `UNIFIED_CONTROLLER_URL` and `UNIFIED_POD_TOKEN_FILE`; remove the env/file secret branches and their constants
  - `internal/k8sagent/config.go:64-77,447,488-496`: refuse `sidecarS3SecretName` and `sidecarS3SecretMode` (yaml or env `UNIFIED_K8S_SIDECAR_S3_SECRET_NAME/MODE`)
  - `internal/k8sagent/agent.go:389-430` (delete the fail-fast/warn block); `:475,:507` (kick the heartbeat after `podBindings.Add`)
  - `internal/k8sagent/backend.go:977,1050,1078,1108` (`--broker-run-id` becomes `--pod-run`)
  - `internal/k8sagent/podpolicy.go:181,651`, `podmountpaths.go:17`, `podmanager.go:102`, `volumes.go:408`
  - `internal/k8sagent/podbindings.go` (a kick channel)
  - `internal/agent/heartbeat.go:80` (`StartHeartbeatKicked`)
  - `cmd/k8s-agent/main.go:75` (drop the warning)
  - `internal/k8sagent/artifact_k8s_test.go` (no Secret prerequisite)
  - `charts/unified-cd/templates/_helpers.tpl:74-86,157-159`: render `kubernetesStoreCredentialPolicies` whenever `controller.enabled`, not only with `bundledStores`; drop `sidecarS3SecretMode`
  - `charts/unified-cd/values.yaml:40-55,96`
  - `internal/helmchart/*_test.go` (the expectations)

**Interfaces:**
- Consumes: `objtransfer.Client` (Tasks 4 and 5). The sidecar's `Auth` sets `Authorization: Bearer <token file contents, re-read per request>`. `TempDir` is `os.TempDir()`.
- Produces:
  - Sidecar flag `--pod-run <runID>`: the Pod's own run, which is the presign path's `{runID}`. `artifact download --run` stays the source run and maps to `sourceRunId`.
  - `func StartHeartbeatKicked(ctx, client, agentID, interval, build, onResponse, kick <-chan struct{}) <-chan struct{}`. `StartHeartbeat` delegates to it with a nil kick. `podBindingSet.Add` does a non-blocking send on a 1-buffered kick channel.

- [ ] **Write failing tests:**
  - `TestRun_CacheRestoreUsesPresignClient`: a fake controller sees a presign `get` on `/api/v1/runs/<pod-run>/objects/presign` with the bearer equal to the token file's contents. `UCD_CACHE_RESULT=hit` is printed.
  - `TestRun_ArtifactDownloadCrossRunSendsSourceRunID`: `--run other --pod-run mine` gives the path `mine` and `sourceRunId=other`.
  - `TestRun_JobFlagOptional`: `cache save` without `--job` exits 0 and calls `commit`.
  - `TestRun_ControllerModeFallback`: the fake answers `{"mode":"controller"}`, so the transfer goes to `/objects/caches`.
  - `TestBuildPod_SidecarAlwaysGetsProjectedToken`: audience `api.KubernetesStoreCredentialAudience`, the token mounted only in the sidecar container, and no `envFrom` secret.
  - `TestConfig_RefusesSidecarS3Secret`: setting `sidecarS3SecretName` or `sidecarS3SecretMode` (yaml or env) makes `validate()` return exactly the k8s-agent string from Global Constraints.
  - `TestExecuteRun_ArtifactStepsNoLongerNeedSidecarSecret` (Review Focus 3): a claim with an unconditional upload step and no secret config creates a Pod and does not fail the run.
  - `TestExecuteRun_HeartbeatKickedAfterPodBinding` (Review Focus 2): after `podBindings.Add`, a heartbeat carrying the binding is sent within 1s, before the 15s ticker.
  - Chart: `TestAgentConfig_NoSidecarS3SecretMode` and `TestControllerConfig_StoreCredentialPoliciesWithoutBundledStores`.
- [ ] **Run to fail:** `go test ./cmd/unified-sidecar/ ./internal/k8sagent/ ./internal/agent/ -run 'Run_|BuildPod|Config_Refuses|ExecuteRun_|Heartbeat' -count=1 && go test ./internal/helmchart/ -count=1`
- [ ] **Implement:** `newClient(runID)` reads `UNIFIED_CONTROLLER_URL`/`UNIFIED_POD_TOKEN_FILE`. If either is missing, a cache/artifact subcommand fails with `cache requires UNIFIED_CONTROLLER_URL and UNIFIED_POD_TOKEN_FILE (set by the k8s agent)` (or `artifact requires …`), exit 1. `idle` is unaffected.
- [ ] **Run to pass:** the same commands; `go build ./...`; then `go test -tags k8s ./internal/k8sagent/... -run ArtifactRoundTrip` on kind if available. Otherwise note it for the CI `k8s` job.
- [ ] **Docs:**
  - `docs/operator-manual/kubernetes-integration.md`: replace the "S3 credentials" and broker sections with "How the sidecar reaches the object store" (projected token, presign, binding, why `kubernetesStoreCredentialPolicies` keeps its name, and that job Pods must reach the store endpoint).
  - `charts/unified-cd/README.md` and the `values.yaml` comments.
  - `examples/config/k8s-agent.yaml`, `examples/jobs/k8s/pod-reuse.yaml:14`.
  - `test/edgecase/k8s/w4*-agent-config*.yaml`: drop the removed keys. The scenario `.md` files and `FINDINGS.md` stay as historical records.
- [ ] **Commit:** `feat(k8s)!: artifact sidecar uses controller-issued URLs; sidecarS3SecretName/Mode removed`

### Task 7: Host agent uses presigned URLs; `objectTransfer`; old cache flags refused

**Files:**
- Modify:
  - `internal/config/agent.go:110-113,236-239,325-335`: add `ObjectTransfer`; keep reading the four old keys only to refuse
  - `cmd/unified-cd-agent/main.go:102-105` (the old flags stay registered with usage `removed; see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md`), `:317-328` (delete `CacheStore` construction)
  - `internal/agent/agent.go:92`: `CacheStore` becomes `Objects *objtransfer.Client`
  - `internal/agent/backend_host.go:216-302`: `cache.*(ctx, b.a.Objects.Caches(), …)`; cache is never "disabled"
  - `internal/agent/client.go:386-470`: `uploadArtifactBody`/`downloadArtifact` go through `c.objects.Artifacts()` in direct mode and keep the existing routes in controller mode
  - `internal/agent/orchestrator.go:1618`: the stale nil-CacheStore comment
  - `docker-compose.yaml:266-270`, `test/ha/docker-compose.ha.yaml:186-198`, `test/edgecase/compose/*.override.yaml` (drop `UNIFIED_CACHE_*`)
  - `examples/config/agent.yaml:72`, `examples/jobs/cache.yaml:3`

**Interfaces:**
- Consumes: `objtransfer.Client{BaseURL: server, RunID: per-claim, Auth: client.authorize, TempDir: os.TempDir(), ForceController: cfg.ObjectTransfer == "controller"}`, built per claim (it needs `RunID`).
- Produces: config `ObjectTransfer string yaml:"objectTransfer"` with validation error `objectTransfer: must be "direct" or "controller", got %q`. `UNIFIED_CACHE_KEY`/`UNIFIED_CACHE_SECRET` stay in the reserved-env lists (`internal/agent/stepenv.go:26`, `internal/dsl/vars_parse.go:55`) as defence.

- [ ] **Write failing tests:**
  - `TestLoadAgent_OldCacheFlagsRefused`: each of yaml `cacheEndpoint`, env `UNIFIED_CACHE_BUCKET`, and flag `--cache-key x` makes startup return exactly the host-agent string from Global Constraints. Test the function `main` calls, e.g. `validateAgentConfig`.
  - `TestLoadAgent_ObjectTransferValues`: the default is `direct`; `controller` is accepted; `bogus` gives the validation error above.
  - `TestHostBackend_CacheDirectUsesPresign`: a fake controller sees `presign get` then `commit`; no `UNIFIED_CACHE_*` is needed.
  - `TestHostBackend_CacheControllerModeStreams`: `objectTransfer: controller` sends no presign and hits `/objects/caches`.
  - `TestClient_ArtifactControllerModeUsesExistingRoutes`: PUT `/api/v1/runs/<id>/artifacts/<name>` as today.
  - `TestClient_ArtifactDirectModeUsesPresign`.
  - The existing `executeDownloadArtifact` cross-run test passes in both modes.
- [ ] **Run to fail:** `go test ./internal/config/ ./internal/agent/ ./cmd/unified-cd-agent/ -run 'OldCacheFlags|ObjectTransfer|HostBackend_Cache|Client_Artifact' -count=1`
- [ ] **Implement:** described above; no new algorithm.
- [ ] **Run to pass:** the same command, then `go test ./internal/agent/ ./internal/config/ ./test/e2e/ -count=1`.
- [ ] **Docs:**
  - `docs/operator-manual/agents.md`: `objectTransfer`, and that the agent must reach the store endpoint in `direct` mode.
  - `docs/reference/configuration.md`: add `objectTransfer`; mark the four cache keys removed, with the migration link.
  - `docs/user-guide/writing-jobs/artifacts-and-cache.md`: caches need no agent configuration.
  - `docs/getting-started/quickstart.md`: if it mentions `UNIFIED_CACHE_*`.
- [ ] **Commit:** `feat(agent)!: host agent caches and artifacts via controller-issued URLs; objectTransfer setting`

### Task 8: Removals, migration guide, operator docs, TODO

**Files:**
- Delete:
  - `internal/controller/api_store_credentials.go`: move `StoreCredentialCluster`, `verifyStoreCredentialToken` and `KubernetesStoreCredentialRequestTimeout` into `object_requester.go`; delete `handleStoreCredentials` and `runBindingRejects`
  - `api_store_credentials_test.go`: port its TokenReview/audience tests to `object_requester_test.go`
  - `internal/objectstore/broker.go`, `broker_test.go`; `credfile.go`, `credfile_test.go`, `env.go`, `env_test.go` (no callers remain; verify with `grep -rn "S3ConfigFromEnv\|NewFileCredentials"`)
- Modify:
  - `internal/api/types.go`: delete `StoreCredentialsRequest/Response`
  - `internal/controller/server.go:55,83-131,177,215,690-700`: drop `StoreCredentialS3` and `RequireRunBinding`; the limiter becomes `objectLimiter`
  - `internal/controller/authz_routes.go:198`
  - `cmd/controller/main.go:214,376-400,455`: `--store-credential-require-run-binding` stays parsed; when true, `slog.Warn("storeCredentialRequireRunBinding is deprecated and ignored: run binding is always required for presigned object access")`
  - `internal/config/controller.go:162-166,291,390`: same deprecation
  - `internal/store/store.go`, `migrations/020_*`: comments only
- Create: `docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md`; `mkdocs.yml` nav entry (~line 130)

- [ ] **Write failing tests:**
  - `TestStoreCredentialsRouteGone`: `POST /api/v1/store-credentials` gives 404 with body `404 page not found`. That is the string the migration guide quotes; if it differs, fix the guide, not the test.
  - `TestRequireRunBindingDeprecatedWarns`: config `storeCredentialRequireRunBinding: true` loads without error and yields one warning containing `deprecated and ignored`.
  - Route-table tests pass without the row.
- [ ] **Run to fail:** `go test ./internal/controller/ ./internal/config/ ./cmd/controller/ -run 'StoreCredentialsRouteGone|RequireRunBindingDeprecated|Route' -count=1`
- [ ] **Implement:** the deletions; `go build ./... && go vet ./...`.
- [ ] **Run to pass:** `go test ./... -short -count=1`
- [ ] **Docs:**
  - **Migration guide**: why; a before→after table (store-credentials endpoint; `UNIFIED_S3_BROKER_*`; `--broker-run-id` becomes `--pod-run`; `sidecarS3SecretName/Mode`; host `--cache-*`/`UNIFIED_CACHE_*`/`cache*` yaml; `storeCredentialRequireRunBinding`; new `objectTransfer`, `s3UseSSL`, `s3Region`, `objectStore.artifactDownload`); upgrade the controller and Kubernetes agents together; the old sidecar error quoted exactly (both the `artifact` and `cache` prefixes); the host-agent refusal string; that no cache-namespace change happens (per the Task 3 parity test, or the one-time-miss note if it failed); and that hosts behind NAT should use `objectTransfer: controller` or a private endpoint.
  - `docs/operator-manual/operations.md`: a networking and cost section (presigning is free; a few more requests with multipart; egress unchanged; controller bandwidth and CPU drop; S3 gateway VPC endpoint or Private Google Access; agents and job Pods must reach the store endpoint as the controller names it; temp space for one part, 64 MiB growing to 4 GiB at about 5 TB).
  - `docs/troubleshooting/artifacts-and-storage.md`: entries for the old-sidecar 404 string, `binding-pending`, `multipart-unavailable`, the host-agent refusal string, the k8s-agent refusal string, and `changed during download`.
  - `docs/operator-manual/reverse-proxy.md` (the rate-limit wording names the presign endpoint), `docs/getting-started/installation.md`, `docs/contributing/invariants.md`, `docs/contributing/auth-implementation.md`, `docs/reference/configuration.md` (`storeCredentialRequireRunBinding` deprecated).
  - `TODO.md`: delete item 56.
  - The final grep must return only historical files (old migration guides, `test/edgecase/*.md`, `docs/superpowers/`): `grep -rn "store-credentials\|UNIFIED_S3_BROKER\|broker-run-id\|sidecarS3Secret\|UNIFIED_CACHE_ENDPOINT\|cacheEndpoint\|RequireRunBinding" --exclude-dir=superpowers .`
- [ ] **Commit:** `feat!: remove store-credentials broker and agent-held cache keys; migration guide (TODO 56)`

### Task 9: Optional redirect for human artifact downloads

**Files:**
- Modify:
  - `internal/config/controller.go`: `ObjectStore *ControllerObjectStoreConfig yaml:"objectStore"` with `ArtifactDownload string yaml:"artifactDownload"`; env `UNIFIED_ARTIFACT_DOWNLOAD`; validation
  - `cmd/controller/main.go`: `--artifact-download`; exit with the exact Global Constraints string when it is `redirect` and `obj` is not a `Presigner`
  - `internal/controller/server.go`: `Config.ArtifactDownload`
  - `internal/controller/api_artifacts.go:84-113` (`handleArtifactDownload`)
  - `internal/cli/artifact.go:66-103`: a `CheckRedirect` that deletes `Authorization` whenever `req.URL.Host != via[0].URL.Host`
  - `web/src/routes/RunDetail.svelte:713-741` and `RunDetail.test.js`

**Interfaces:**
- Behaviour after the unchanged auth, permission and audit middleware:
  - Agent principals (`agentPrincipalFromContext`) always get `proxy`.
  - Humans with `redirect` get `302 Location: <PresignGet(key, "<name>.tar.gz")>` with `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. A missing object (checked with `Stat`) gives 404 as today.
  - `?as=url` (the UI): in `redirect` mode the answer is `200 {"url":"…"}` with the same headers. In `proxy` mode the parameter is ignored and the controller streams.
  - UI: fetch with `?as=url`. If the response is `application/json`, navigate a temporary `<a href=url>`; otherwise keep the blob path.
  - Config error for other values: `objectStore.artifactDownload: must be "proxy" or "redirect", got %q`.

- [ ] **Write failing tests:**
  - `TestArtifactDownload_RedirectForHumans`: 302, `Location` has `X-Amz-Expires=900` and `response-content-disposition=attachment; filename="a.tar.gz"`; both headers set; one audit row as in proxy mode.
  - `TestArtifactDownload_RedirectModeStillProxiesAgents` (Review Focus 5): an agent bearer gets 200 with the bytes.
  - `TestArtifactDownload_AsURLJSON`.
  - `TestArtifactDownload_ProxyDefault`.
  - `TestControllerConfig_RedirectNeedsS3`: exact string.
  - `TestArtifactDownload_CLIDropsAuthorizationOnSubdomainRedirect` (Review Focus 5): the controller is at `ci.example.test` and the store at `s3.ci.example.test` (`httptest` plus a custom dialer). The store sees no `Authorization`. Go's default policy forwards it to subdomains, so the `CheckRedirect` is required.
  - `TestArtifactDownload_CLIFollowsRedirect`: the CLI extracts the archive.
  - `RunDetail.test.js`: a JSON answer navigates to the URL and creates no blob.
- [ ] **Run to fail:** `go test ./internal/controller/ ./internal/cli/ ./internal/config/ -run 'ArtifactDownload|RedirectNeedsS3' -count=1 && (cd web && npx vitest run src/routes/RunDetail.test.js)`
- [ ] **Implement:** described above.
- [ ] **Run to pass:** the same commands.
- [ ] **Docs:**
  - `docs/reference/configuration.md`: `objectStore.artifactDownload` / `UNIFIED_ARTIFACT_DOWNLOAD`.
  - `docs/operator-manual/operations.md`: the store endpoint must be reachable from users' browsers, so keep `proxy` for an internal-only Garage hostname; an interrupted download after expiry must be restarted from the UI.
  - `docs/reference/cli.md`: `artifact download` follows redirects without forwarding credentials.
  - The migration guide's table gets this row.
- [ ] **Commit:** `feat(controller): optional presigned redirect for human artifact downloads`

---

## Self-review against the spec

- **Coverage:**
  - Every required test in "Left to the builder" maps to a task:
    - own run and namespace: Task 3 scope tests
    - binding 409: Tasks 3 and 4
    - one object, one op, 15 minutes: Task 1
    - multipart complete and abort: Tasks 1, 3 and 4
    - Range resume: Task 4
    - controller mode: Task 5
    - old flags refused: Task 7
    - Garage: Tasks 1 and 4
    - S3/GCS: Task 1 finding plus the `gcs` tag
  - Every Designer's call is covered:
    - 1: Task 4
    - 2: Tasks 2 and 3
    - 3: Task 3 parity test
    - 4: Task 3 guards and Task 9
    - 5: Task 3 and Task 8 deprecation
    - 6: Task 5
    - 7: Tasks 7 and 8
    - 8: Task 3
    - 9: Tasks 1 and 4
  - Every doc in the spec's list is assigned.
- **Type consistency:** `PresignRequest/PresignResponse/ObjectPart/PresignedRequest/ObjectError` are defined once (Task 3) and consumed in Tasks 4-6. `cache.Remote` is defined in Task 2 and implemented by `StoreRemote` (Task 2) and `objtransfer` (Tasks 4 and 5). `objectstore.Presigner`/`Statter` come from Task 1.
- **Proportion:** each task names its decisions and test assertions; code appears only for the JSON contract and the part-size formula.
- **Rulings on spec gaps** (each reflected in the tasks above):
  - Sidecar env/file secret modes are removed, not just the broker. Goal 4 ("no bucket key leaves the controller") would otherwise be false for every operator still using them.
  - Cache TTL is `min(requested, largest ttlDays declared in the run's spec)`: the controller bounds the request by the run record, so a client cannot extend it.
  - Presign guards mirror `handleArtifactUpload` (terminal runs still accepted) per call 4, over the Why section's "still running". Cross-run downloads keep the download route's agent bypass.
  - The binding retry window is 30s, not the old ≈6s broker window, which is shorter than the 15s heartbeat. The k8s agent also kicks a heartbeat on binding.
  - Every upload uses the growing-part scheme (all uploads are streams), so the spec's "from the file size" formula is not needed.
  - Run-end abort skips the job's cache prefix while another run of that job is active.
  - The UI uses `?as=url` JSON rather than following a cross-origin 302 inside `fetch`, which would need store CORS.
  - The controller gains `s3UseSSL`/`s3Region` because signed URLs are handed to clients.
