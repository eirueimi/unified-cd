# Migrating: run-scoped object access (presigned URLs replace store credentials)

Artifacts and caches no longer use an object-store credential held by the job Pod's sidecar or by the
host agent. This is a breaking change for Kubernetes agents, host agents and any tooling that called
`POST /api/v1/store-credentials`. Read [Upgrade order](#upgrade-order) before you start.

## Why

Before this change, the Kubernetes artifact sidecar fetched the controller's own object-store key from
`POST /api/v1/store-credentials`. That key is bucket-wide and does not expire, so any job Pod that could
present a valid projected token could read or overwrite every other run's artifacts and every job's
caches. Host agents were configured with a store key of their own (`--cache-*`) with the same reach.

Now the controller is the only holder of the store credential. For each transfer, the sidecar or host
agent asks the controller (`POST /api/v1/runs/{runID}/objects/presign`) for a presigned URL that covers
**one object and one operation** and expires after **15 minutes**. The controller derives the object key
itself: an artifact from the run and the artifact name, a cache from the run's job and the cache key.
The caller never names a raw key. A Kubernetes sidecar is served only while its Pod is bound to that run,
and a host agent only for runs it has claimed. A stolen token or agent credential now reaches one run's
objects, not the bucket.

## What changed

| Before | After |
|---|---|
| The sidecar posted its projected token to `POST /api/v1/store-credentials` and received the controller's S3 key | The endpoint is removed. The sidecar posts to `POST /api/v1/runs/{runID}/objects/presign` and receives a URL for one object, valid for 15 minutes |
| The sidecar read `UNIFIED_S3_*` and `UNIFIED_S3_BROKER_*` variables | The sidecar gets `UNIFIED_CONTROLLER_URL` and `UNIFIED_POD_TOKEN_FILE`. It holds no store credential |
| The sidecar flag `--broker-run-id` named the Pod's run | The flag is `--pod-run`. The k8s agent sets it; you do not |
| k8s agent: `sidecarS3SecretName` / `sidecarS3SecretMode` (or `UNIFIED_K8S_SIDECAR_S3_SECRET_NAME` / `UNIFIED_K8S_SIDECAR_S3_SECRET_MODE`) copied or mounted an S3 Secret into job namespaces | Both settings are removed, and the agent refuses to start while either is set. Delete the Secrets once every agent is upgraded. The Helm chart no longer sets `agent.config.sidecarS3SecretMode` |
| Host agent: `--cache-endpoint`, `--cache-key`, `--cache-secret`, `--cache-bucket`, `UNIFIED_CACHE_ENDPOINT` / `_KEY` / `_SECRET` / `_BUCKET`, and the YAML keys `cacheEndpoint` / `cacheKey` / `cacheSecret` / `cacheBucket` | All removed. The agent refuses to start while any is set. Caches use controller-issued URLs and need no agent configuration |
| Artifacts moved through the controller; caches used the agent's own store key | Artifacts and caches both use presigned URLs. New agent setting `objectTransfer: direct` (default) or `controller` (env `UNIFIED_OBJECT_TRANSFER`, flag `--object-transfer`) |
| The controller built URLs only for its own use | New controller settings `s3UseSSL` (env `UNIFIED_S3_USE_SSL`, flag `--s3-use-ssl`) and `s3Region` (env `UNIFIED_S3_REGION`, flag `--s3-region`). Presigned URLs travel to clients, so AWS S3 and GCS need `s3UseSSL: true` and the right region |
| Human artifact downloads were always streamed by the controller | New controller setting `objectStore.artifactDownload: proxy` (default, unchanged behaviour) or `redirect` (env `UNIFIED_ARTIFACT_DOWNLOAD`, flag `--artifact-download`): after the usual authorization and audit, answer `302` to a 15-minute presigned GET |
| `storeCredentialRequireRunBinding` (`--store-credential-require-run-binding`, `UNIFIED_STORE_CREDENTIAL_REQUIRE_RUN_BINDING`) chose whether an unbound Pod was allowed | **Deprecated and ignored.** Run binding is always required. If set to `true` the controller logs `storeCredentialRequireRunBinding is deprecated and ignored: run binding is always required for presigned object access` and continues. It does not refuse to start. Remove it |

These keep their names and now govern the presign endpoint for job Pod sidecars:
`agentAuth.kubernetesStoreCredentialPolicies` (which cluster, namespace and ServiceAccount may ask),
`storeCredentialRateLimit` with `UNIFIED_STORE_CREDENTIAL_RATE_BURST` / `UNIFIED_STORE_CREDENTIAL_RATE_PER_SECOND`,
and the token audience `unified-cd-store-credentials`. Nothing to rename in existing configuration. The
Helm chart still renders the policy for the job namespace and the `default` ServiceAccount.

**Caches keep their namespace.** The cache key prefix is still derived from the run's job name, and a
test confirms the controller-derived name equals the one the agent used before (including folder-qualified
jobs and `call:` children). Existing cache entries are found after the upgrade; there is no one-time miss.

## Upgrade order

1. **Controller and Kubernetes agents together.** The sidecar image ships with the k8s agent and its
   default `sidecarImage` follows the agent's version, so upgrading the agent upgrades the sidecar. An old
   sidecar cannot talk to the new controller, and a new sidecar cannot talk to an old controller. Upgrade
   the controller and every Kubernetes agent in the same window; job Pods that start in between fail
   their artifact and cache steps.
2. **Host agents next.** A host agent keeps working against the new controller for artifacts, but its old
   `--cache-*` settings now stop it from starting, so remove them when you upgrade it. Upgrade host agents
   after the controller.
3. Clean up: delete the S3 Secrets you created for `sidecarS3SecretName`, and remove
   `storeCredentialRequireRunBinding` from the controller configuration.

A released old controller has no presign endpoint, so a new sidecar cannot work against it: every
artifact and cache step in a new job Pod fails. The rule is therefore **upgrade the controller first, or
together with the Kubernetes agents**, never the agents alone. The reverse also fails: an old sidecar
cannot work against the new controller (see below).

`objectStore.artifactDownload` arrives in this same release; until you set it, human downloads behave as
before (`proxy`).

## Rollback

- **Roll back the controller and the Kubernetes agents together.** The sidecar image ships with the agent,
  so rolling the agent back restores the old sidecar that the old controller serves. Rolling back only one
  side leaves the pair incompatible, with the errors below.
- **Host agents** rolled back to the old binary work against the old controller only with their old
  `--cache-*` settings restored (the new binary refuses to start with them, the old one needs them for
  caches).
- **Migration `033` stays applied.** Running an older controller does not revert it, and a controller that
  does not embed migration `033` cannot start against a database already at version 33: golang-migrate
  cannot find the migration for the database's version and the controller exits at startup. Roll the schema
  back first, using the migration files of the **new** release and the same tooling the controller uses:
  `migrate -path internal/store/migrations -database "$UNIFIED_DB_DSN" goto 32` (v0.17.0 embeds migrations
  up to `032`; for an older release use the last version that release embeds, for example `031` for v0.16.0,
  which also runs the `032` down migration and drops the CLI login codes). Take a database backup first.
  The `033` down migration is safe: it keeps the most recently reported Pod of each run and restores the
  one-binding-per-run primary key, and the next heartbeat rebuilds any binding it dropped.

## Errors you will see

**An old sidecar against the new controller.** The old sidecar still posts to the removed endpoint. It
sends its token in the body, with no `Authorization` header, so the controller's API authentication
answers before routing does, and the sidecar prints (the `cache` group prints `cache requires` instead of
`artifact requires`; `<url>` is the controller URL):

```
artifact requires S3 configuration (UNIFIED_S3_*): store credentials broker <url>: store credentials request failed: 401 Unauthorized: unauthorized
cache requires S3 configuration (UNIFIED_S3_*): store credentials broker <url>: store credentials request failed: 401 Unauthorized: unauthorized
```

The `UNIFIED_S3_*` in that message is the old sidecar's own wording; nothing needs to be set. The fix is to upgrade the k8s agent so the sidecar image matches. See
[the troubleshooting entry](../../troubleshooting/artifacts-and-storage.md#kubernetes-the-sidecar-fails-with-artifact-requires-s3-configuration-unified_s3_-after-an-upgrade).

**A k8s agent with `sidecarS3SecretName` or `sidecarS3SecretMode` still set** exits at startup with:

```
k8s-agent config: sidecarS3SecretName and sidecarS3SecretMode are no longer supported; the artifact sidecar uses controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)
```

**A host agent with any `--cache-*` setting** exits at startup with:

```
agent config: the cache store flags (--cache-endpoint, --cache-key, --cache-secret, --cache-bucket) are no longer supported; caches use controller-issued URLs (see docs/operator-manual/migrations/2026-11-20-run-scoped-object-access.md)
```

The refusal covers the flags, the `UNIFIED_CACHE_*` variables and the YAML keys alike, so a value left in
an environment file or a config file stops the agent just as a flag does. Remove all of them.

**A Pod that is not yet bound to its run** gets `409 binding-pending` from the presign endpoint. The
sidecar retries for up to 30 seconds while the k8s agent reports the binding on its next heartbeat; the
error reaches the step only if that never arrives. Other messages (`403 forbidden`, `multipart-unavailable`,
`object changed during download`) are covered in
[Troubleshooting: Artifacts and Storage](../../troubleshooting/artifacts-and-storage.md).

## Networking and cost

- **Reachability changes.** Bytes now go straight between the client and the object store. The endpoint
  the controller names in a presigned URL (its own `UNIFIED_S3_ENDPOINT`) must be reachable, under that
  name and scheme, from every host agent in `direct` mode and from every job Pod on Kubernetes. Before,
  only the controller needed to reach the store. Check DNS, routing and TLS trust. An internal-only store
  hostname that agents cannot resolve will fail every transfer.
- **Hosts behind NAT or on another network** that cannot reach the store should set
  `objectTransfer: controller` (the controller streams their archives; it then carries all those bytes),
  or reach the store through a private endpoint.
- **Cost.** Presigning is a local computation and is free. Multipart uploads add a few requests per large
  archive. Data transfer charges are unchanged, because the same bytes move; they now move between the
  client and the store instead of through the controller, so controller bandwidth and CPU drop. On AWS,
  keep the traffic inside the VPC with an **S3 gateway VPC endpoint** (no charge for the endpoint itself);
  on GCP, enable **Private Google Access** on the subnets of your nodes. Without them, traffic from private
  subnets leaves through a NAT gateway and is billed per GB.
- **Temp space on the client.** A large upload is sent in multipart parts, and the client buffers one part
  in its temp directory at a time. The first parts are 64 MiB and the size doubles every 1,000 parts, up to
  4 GiB at roughly 5 TB. A stream that ends within the first part is sent as one PUT.

## Object store notes

- **AWS S3 and Garage:** presigned GET, PUT, HEAD and multipart parts are supported. Garage is verified in
  CI.
- **GCS (S3-compatible API): multipart presign is not yet verified.** No GCS credentials were available to
  run the check, so the controller treats GCS as not supporting it and clients send a single presigned
  PUT, up to GCS's 5 TiB object limit. Single and cross-run reads and writes work. See
  [Operations: Object store compatibility](../operations.md#object-store-compatibility) for the test that
  would change this.
- **Local filesystem store (development):** it cannot presign, so the controller answers `controller`
  mode and every transfer streams through it. Nothing to configure. `objectStore.artifactDownload:
  redirect` is not available with it, and the controller refuses to start if it is set.
