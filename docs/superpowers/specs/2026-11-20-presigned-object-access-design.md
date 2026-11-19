# Run-scoped object access with presigned URLs (TODO 56) — design

Status: agreed in conversation (2026-11-20); written spec awaiting approval

## Why

Today two kinds of job-running machines hold a key to the whole artifact and cache bucket:

- **Kubernetes job Pods.**
  - The artifact sidecar (`cmd/unified-sidecar`) exchanges its projected ServiceAccount token at `POST /api/v1/store-credentials` for the controller's own bucket-wide, non-expiring S3 key.
  - It then reads and writes the store directly.
- **Host agents.**
  - Caches use an S3 key configured on the agent (`--cache-endpoint/--cache-key/--cache-secret/--cache-bucket`).
  - A job running on that machine can usually read the key.
  - Artifacts already stream through the controller (`PUT/GET /api/v1/runs/{runID}/artifacts/{name}`), which loads the controller.

Anyone who obtains such a key can read and overwrite every run's artifacts and caches.

The user wants each job to reach only its own run's objects, without routing bulk data through the controller (network and cost), on S3 and GCS in production and on the bundled Garage locally.

## Goals

1. **The controller issues object-scoped URLs instead of keys.** It hands out short-lived presigned URLs, each valid for one object and one operation (GET, PUT, a multipart part, or HEAD), and only after checking that the requester may touch that object.
2. **Data goes direct by default.** Data flows directly between the agent or sidecar and the object store.
   - Host agents can opt out with an agent setting (`objectTransfer: direct | controller`, default `direct`) for machines that cannot reach the store.
   - `controller` keeps today's controller-streamed path for artifacts and adds the same for caches.
3. **Large files.** Uploads use multipart, with presigned URLs per part. Downloads use one presigned GET and may resume with Range requests. Objects up to the store's limit (5 TB on S3) work.
4. **No bucket key leaves the controller.**
   - `POST /api/v1/store-credentials` is removed.
   - The host agent's own cache S3 settings are removed.
5. **Stores.** S3, GCS (through its S3-compatible XML API with HMAC keys) and Garage are all supported, and all three are exercised in tests or a documented manual check.

## Non-goals

- Changing what is stored, or the key layout, beyond what scoping needs.
- Encryption at rest (unchanged; the store's responsibility).
- A non-S3 API for GCS. If GCS's S3-compatible multipart cannot be presigned, see the open question in Decided design.

## Anti-goals

- A URL that grants access beyond one object and one operation, or that lives longer than needed.
- A job reaching another run's artifacts, or a cache outside its job's namespace (the existing `jobName` namespacing rule).
- Bulk data through the controller when `direct` is selected.

## Decided design

### Authorization of a URL request

The controller signs a URL only after checking who is asking and what for.

- **Requesters.**
  - **The agent.** It uses its agent credential (`uca_` bearer), for a run it has claimed and that is still running.
  - **The Kubernetes sidecar.**
    - It authenticates with its projected ServiceAccount token, the same TokenReview the store-credentials endpoint performs today.
    - It is bound to the run by the existing PodBinding (which Pod runs which run).
    - Run binding becomes mandatory for these requests, because a URL is for a specific run's object. The bounded start-up window the old design accepted (`RequireRunBinding` defaulting to false) no longer applies. A request before the binding is known gets a retryable 409, and the sidecar retries with backoff for up to the existing broker window.
- **Objects.** These are object keys the controller derives itself from the request:
  - **Artifacts** of the requester's run: upload and download its own. Downloading another run's artifact keeps today's rule (`downloadArtifact.runId` with its existing permission check). The controller resolves the key; the client never sends a raw key.
  - **Caches** in the requester's job namespace: restore (HEAD/GET, including `restoreKeys` prefix lookups) and save (PUT or multipart).
    - The controller performs the existence and prefix lookups with its own key and returns the chosen key's GET URL.
    - The client never lists the bucket.

### Endpoints (new, controller)

All of these are authenticated as above, rate-limited per requester, and audited as agent actions where the existing audit model covers agent writes.

- `POST /api/v1/runs/{runID}/objects/presign`, with body `{kind: artifact|cache, op: get|put|head|multipart-start|multipart-parts|multipart-complete|multipart-abort, name|key, restoreKeys?, partNumbers?, size?}`.
  - It returns URLs plus headers that must be sent.
  - The builder may split this into several endpoints. The contract is: one object per request, with parts batched.
- **Multipart.**
  - The controller starts the upload with its key and returns the upload ID.
  - It presigns `UploadPart` URLs for the requested part numbers, in batches; a client re-requests parts whose URLs expire.
  - It completes or aborts with its key.
  - Abandoned uploads are aborted by the controller on run end, plus a periodic sweep.
- **URL lifetime:** 15 minutes. A part URL is requested shortly before use. GET URLs are long enough to start the transfer; a resumed download re-requests.
- **The `controller` transfer mode.**
  - Artifacts keep the existing upload and download routes.
  - New routes stream caches the same way.

### Clients

- **Sidecar** (`cmd/unified-sidecar`): replaces `objectstore.NewBroker` credentials with a presign client, for cache restore/save and artifact upload/download.
- **Host agent:** the cache code (`internal/agent` `cache.Restore`/`Save` over `objectstore.ObjectStore`) and artifact upload/download use the presign client in `direct` mode, or controller streaming in `controller` mode.
- **Shared code:** a small `ObjectStore`-shaped client over presigned URLs lets the existing cache and artifact code paths stay largely unchanged. It covers Put with multipart above a threshold, Get, Head and prefix resolution through the controller.
- **Part size:** chosen from the file size so that parts stay under 10,000, with a minimum of 64 MiB, and capped by the store's maximum part size.

### Stores

- The controller signs with the existing S3 configuration (minio-go presign APIs).
- **GCS** uses its S3-compatible endpoint with HMAC keys.
  - Open item for the builder to verify first: whether GCS accepts presigned `UploadPart` and `CompleteMultipartUpload` through that API.
  - If it does not, GCS uses single-PUT uploads up to its 5 TiB object limit. GCS's single-PUT limit is larger than S3's 5 GB, so this is acceptable.
  - Record the result in the docs.
- **Garage:** presigned GET, PUT and multipart are expected to work. Verify in an integration test with the docker-compose Garage.
- **The filesystem `LocalObjectStore`** (dev/test) has no presigning. With it, the controller forces `controller` mode for all transfers, and logs that once at startup.

### Removals (breaking)

- `POST /api/v1/store-credentials`, `objectstore.NewBroker`, the sidecar's broker flags, and the related controller config (`RequireRunBinding` stays only if still used by something else).
- The host agent's `--cache-endpoint/--cache-key/--cache-secret/--cache-bucket` flags and config keys. A host agent with them set refuses to start and points to the migration guide. Caches then go through the controller-issued URLs.
- The controller and Kubernetes agents (whose sidecar image ships with the agent version) must be upgraded together.

### Cost and networking notes (docs)

- Presigning is free. Request counts rise slightly with multipart.
- Egress is roughly unchanged versus controller streaming, and the controller's bandwidth and CPU drop.
- Recommend an S3 gateway VPC endpoint or Private Google Access for agents in private networks, to avoid NAT data charges.

## Designer's calls (settled after the builder check)

1. **Streams of unknown size (cache archives are tar+zstd streams).**
   - The presign client uploads with multipart and a growing part size. It starts at 64 MiB and doubles every 1,000 parts. S3 allows unequal part sizes; every part except the last must be ≥5 MiB. This keeps uploads under 10,000 parts well past 1 TB with no size known up front.
   - Where multipart presign is unavailable (the GCS fallback), the client first spools the stream to a temp file in the agent's or sidecar's temp dir, then does one PUT with a known Content-Length.
2. **Cache `.meta` is written by the controller, never by the client.**
   - Presign never returns a PUT URL for a `.meta` key.
   - A cache save ends with a `commit` operation, after the multipart complete or the single PUT. In `commit` the controller:
     - checks that the data object exists (HEAD with its key);
     - writes `.meta` itself, with `OriginalKey`, `OwnerJob` and `Size` from that HEAD;
     - sets `ExpiresAt` from its configured cache TTL, never from the client.
   - A data object without `.meta` is invisible to restore and is removed by the existing GC as an orphan. If GC does not handle orphans today, the builder adds that.
   - `findBestMatch` and GC keep trusting `.meta`, because now only the controller writes it.
3. **Cache namespace.**
   - The controller takes the job name from the run record and ignores any client value. The sidecar's `--job` and the host agent's `b.jobName` become inputs only for logging.
   - The builder must verify that today's host `b.jobName` equals the run record's job name for `call:` child runs and folder-qualified jobs, and add a test, so existing cache entries keep their namespace. If they differ, the run record's value wins, and the migration guide notes the one-time cache miss.
4. **Which runs and requesters may get URLs.** Presign mirrors the existing artifact handlers' guards exactly (`agentRunGuard`, the terminal-run rules of `handleArtifactUpload`/`handleArtifactDownload`).
   - **Cross-run downloads** (`downloadArtifact.runId`, `call:` reading a child's artifacts) use the same permission checks as the existing download route.
   - **Humans (UI, CLI).** The download route is unchanged, with the same authentication, permission check and audit entry. User decision (2026-11-20): a new controller setting, `objectStore.artifactDownload: proxy | redirect` (env `UNIFIED_ARTIFACT_DOWNLOAD`), chooses what happens after those checks.
     - **`proxy`** (the default, today's behaviour): the controller streams the object.
     - **`redirect`**:
       - The controller answers `302` to a 15-minute presigned GET for that one object, with `Cache-Control: no-store` and `Referrer-Policy: no-referrer`. The response sets `response-content-disposition`, so the browser saves the file under the artifact's file name.
       - The CLI follows the redirect without forwarding its `Authorization` header to the store host. Go's http client already drops it on a cross-host redirect; add a test.
       - The docs note that the store endpoint must be reachable from users' browsers. An internal-only Garage hostname is not, so keep `proxy` there.
       - The docs also note that a download interrupted after the URL expires must be restarted from the UI.
     - With the `LocalObjectStore`, `redirect` is not available. The controller refuses to start if it is set, with `objectStore.artifactDownload: redirect requires an S3-compatible object store`.
5. **Controller config.**
   - The existing sidecar-auth settings are kept with their names, and now govern the presign endpoint for sidecars: `StoreCredentialClusters`, the namespace/ServiceAccount allow-list, its Helm values, and the `StoreCredentialRate*` limits.
   - `StoreCredentialRequireRunBinding` is removed, because binding is always required. If it is still set, the controller logs a deprecation warning and ignores it; it does not refuse to start.
   - The docs explain the reuse of the old names.
6. **`controller` mode.**
   - The controller does the cache prefix lookup and writes `.meta` server-side, as in direct mode.
   - The sidecar supports `controller` mode too, chosen automatically when the controller answers presign with `{"mode":"controller"}` (the `LocalObjectStore`). There is no sidecar flag.
7. **Rollout and errors.**
   - No grace period for `/api/v1/store-credentials`. An old sidecar gets 404. The migration guide quotes the error the old sidecar prints (find the exact string in today's broker code) and says to upgrade the Kubernetes agent.
   - A host agent with the old cache flags refuses to start with:
     `agent config: the cache store flags (--cache-endpoint, --cache-key, --cache-secret, --cache-bucket) are no longer supported; caches use controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)`
8. **Abandoned multipart uploads: no DB table.**
   - On run end, the controller aborts in-flight uploads under that run's artifact prefix and that job's cache prefix, using `ListMultipartUploads` with its key.
   - A leader-elected sweep (a new advisory-lock key) aborts uploads older than 24 hours.
9. **URL lifetime and resume.**
   - The lifetime is a fixed 15 minutes.
   - Clients treat a 403 or an expiry mid-transfer as "re-request the URL and resume" (Range for GET, re-upload the part for PUT), bounded by the existing retry policy.

## Left to the builder

- The exact endpoint split and JSON shapes, the rate-limit numbers, the audit action names, and the sweep interval for abandoned multipart uploads.
- The test layout. At least these must be tested:
  - a URL is only issued for the requester's own run or cache namespace;
  - the sidecar without a run binding gets a retryable error;
  - each URL covers exactly one object and operation, with a 15-minute expiry;
  - a multipart upload of more than one part completes, and an abandoned one is aborted;
  - Range resume works;
  - `controller` mode works;
  - a host agent with the old cache flags refuses to start;
  - integration against Garage;
  - S3 and GCS checked as documented (manual is acceptable for GCS).
- **Documentation:**
  - `docs/operator-manual/kubernetes-integration.md` (store credentials removed)
  - the agents and configuration references (`objectTransfer`, removed cache flags)
  - `operations.md` (networking, VPC endpoint, cost)
  - the troubleshooting entries with exact error strings
  - a migration guide `docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md` with a before→after table
  - `TODO.md`: remove item 56.
