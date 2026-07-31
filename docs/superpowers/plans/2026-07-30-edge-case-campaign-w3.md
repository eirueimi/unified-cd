# Edge-Case Campaign: Wave W3 (Storage / Keys) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Wave W3 (storage / key-management scenarios) of the edge-case campaign: six live scenarios against object storage and secret encryption, recording findings.

**Architecture:** Same pattern as W0-W2 — per-scenario runbooks under `test/edgecase/scenarios/`, findings appended to `test/edgecase/FINDINGS.md`, raw captures to the session scratchpad and copied to `<project parent>/edgecase-evidence/w3/` at the checkpoint.

**Tech Stack:** docker compose (`test/ha` + overlays), **Garage** (S3-compatible) newly added to the rig, an **S3 interposer** for method-selective faults, curl against LB `localhost:18080` (token `ha-admin-token`), psql, `test/edgecase/tools/inject.sh`.

## Task ordering — deliberate, do not reorder

**Two scenarios run on today's rig and four are blocked on infrastructure that does not exist yet.** The two runnable ones come first so the wave produces findings even if the Garage integration proves harder than estimated. Infrastructure is Task 3, not Task 1.

## Global Constraints

- All committed text is English (AGENTS.md).
- Work on branch `plan/edge-case-w3` in worktree `wt-edge-spec` — never commit on the main checkout.
- **No production-code changes** (spec §8). Test-only files under `test/edgecase/` and docs. **`test/ha/docker-compose.ha.yaml` is the one exception this wave needs** — see Task 3, which amends it rather than overlaying, with a stated justification. Everything else uses overlays.
- Findings record problems; they do not fix them. Classification: **violation** = contradicts an invariant (I1-I7) or a documented contract (`docs/*.md`; an unexported helper's own doc comment does NOT count); **observation** = as-designed but reveals risk. Third bucket: defects in the campaign's own assets, reported outside both tallies.
- Observation entries say "observation" in the **title** and repeat it in the Severity line as "minor (observation)" (`FINDINGS.md:481`).
- **Quote the invariant verbatim** from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md:44-55`. W2 had four attributions corrected in review — I3 is *no lock leaks*, I2 is *at-most-once* (a zero-vs-once defect does not violate it), and I1's "exactly one terminal state" is not a liveness bound.
- **Before filing a contract violation, read the surrounding section** and check it is not scoped to a different case. W2-7 cited a passage that *sanctioned* the observed behaviour. If the docs are silent, say "silent, not contradicted" and rest on the invariant.
- Every number traceable to a capture whose window covers it. Label derived / inferred / code-read. Annotate uncaptured live observations as `(observed live, raw output not captured to scratchpad)`. **Do not write "never" for a window you ended yourself.**
- Kill every sampler and stream before teardown and **capture** that you did (`jobs` plus a `ps` grep). Tear every stack down with `-v`.
- Postgres statement logging: **one `ALTER SYSTEM` per `psql -c`** (two in one is an implicit transaction, refused silently while `pg_reload_conf()` still returns `t`). Verify `log_statement` **and** `log_line_prefix` in a **fresh** session, on both arm and revert.

## Verified code facts (do not re-derive)

**Read at HEAD `358ddbe`. Per the W1 and W2 lessons these are claims, not givens — eight of nine W2 scenarios corrected something in the equivalent block. If execution contradicts one, that contradiction is itself a finding.**

### Corrections to previously-established campaign facts

- **The archiver's `failureBackoff` DOES survive ticks.** `bo := newFailureBackoff(time.Minute, time.Hour, 10_000)` is at `internal/controller/archiver.go:28`, **before** the loop at `:29` — one instance per *process*, not per tick. It does not transfer between replicas: `internal/controller/failure_backoff.go:13-15` says "Leader-local by design: a failover or restart clears it, costing one retry per poison before it is re-excluded." So a poison run gets up to **one retry per replica per backoff window**, not one per tick and not zero. (The W2 facts block said the opposite.)
- **A drop marker mechanism DOES exist** — `internal/agent/runner.go:396-424`, `[%d log line(s) dropped: controller unreachable]` — gated on `p.droppedLines > 0` (incremented **only** by `appendPendingLocked`'s byte-cap eviction, `runner.go:432`) **and** `len(p.pending) == 0`. So **cap-overflow losses are marked; `Flush`-abandonment losses are not and cannot be.** W1-6's 266/~214 lost lines are the second class. State it this way, not "no marker exists".
- **`UploadArtifact` does not go through `c.do`.** It hand-builds the request and returns a bare `fmt.Errorf("upload artifact http %d", resp.StatusCode)` (`internal/agent/client.go:367-368`), which is **not** an `*HTTPError` — so `retryUntilSuccess`'s `errors.As` probe would not match it. Moot in practice (artifact upload is never retried), but do not carry the "any 4xx is permanent" rule onto this path.
- **WAVE-LEVEL FACT, ADDED AFTER W3-3 EXECUTION — the agent destroys every controller error message in transit, so no agent-side failure is diagnosable from the run's own logs.** `Client.do` replaces the body of **every** response with status >= 400 by the literal string `"response omitted"` (`internal/agent/client.go:107-108`: `return resp.StatusCode, &HTTPError{StatusCode: resp.StatusCode, Body: "response omitted"}`; `HTTPError.Error()` formats it at `:26-28`, and the unused helper `safeResponseBody` at `:134` returns the same string). Whatever the controller wrote — e.g. the precise `decrypt <name>: decrypt dek: ...` built at `internal/controller/api_secrets.go:142` — never reaches the agent's logger, so the run's own log reads `http <code>: response omitted` and nothing more. **W3-3 measured this: 23 failures, one distinct log line, zero diagnostic content.** The string has now been hit in **W1-6** (`FINDINGS.md:169`, `:311`, `:378`, `:391`), **W3-3** and **W3-4** (`FINDINGS.md:1567`). **Tasks 4, 5, 6 and 7 (S3 outage, seal-vs-flush, retention, cache) all plan to read agent-side failure messages: do not budget for a diagnosable one.** The controller's own `slog` line is the only place the real message exists — capture it container-side, and capture it *before* any `up -d --force-recreate`, which erases that container's log history. (The policy itself is deliberate secrets hygiene — a body from an arbitrary intermediary should not be copied into a log — so it is a constraint on measurement, not a defect to re-file.)

### The rig as it stands — the wave's central problem

`test/ha/docker-compose.ha.yaml` has **no object storage**. Controller env is only `UNIFIED_DB_DSN`, `UNIFIED_TOKEN`, `UNIFIED_CONTROLLER_KEY_FILE` (`:19-23`). So `cmd/controller/main.go:303-322` takes neither branch, `obj == nil`, and `slog.Warn("no object store configured — log archival disabled")` fires. Consequences:

| Subsystem | State on today's rig |
|---|---|
| Log archiver | **not started** (`main.go:399`, `if obj != nil`) |
| Cache cleanup | **not started** (`main.go:401`) |
| Artifact upload | **503** "object store not configured" (`internal/controller/api_artifacts.go:21-24`) |
| Agent cache | **disabled**, steps are no-ops (`internal/agent/agent.go:68`) |
| Run retention | runs but skips all object deletion (`run_retention.go:132`, `:161`) |
| Seal / `run_log_archives` | **never populated** |

**`UNIFIED_S3_*` is read once at startup and a bad endpoint is FATAL** — `NewS3ObjectStore` calls `BucketExists` eagerly (`internal/objectstore/s3.go:41-49`) and `main.go:311-313` does `os.Exit(1)`. So a controller that restarts while object storage is down **crashloops**. That is a finding in its own right and it constrains injection ordering for every scenario that also restarts a controller.

### W3-1 — cache

- **The cache has no database presence at all.** `internal/cache/cache.go` touches only an `objectstore.ObjectStore`; no `store.Store` parameter anywhere in the package. Two objects per entry: `caches/<b64url(sha256(jobName))>/<b64url(sha256(key))>.tar.zst` and `.meta` (keys at `cache.go:49-53`, `base64.RawURLEncoding`).
- **The TTL lives in the `.meta` object**: `ExpiresAt: time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour)` (`cache.go:90`). Not a column, not object metadata, not an S3 lifecycle rule.
- `DeleteExpired` (`cache.go:181-214`) deletes the **archive first** (`:204`) then the `.meta` (`:207`), `continue` on either failure — two deletes, no transaction. A failure between them leaves `.meta` orphaned with no payload.
- **Half-done states:** `.meta` without payload is handled cleanly (`findBestMatch` skips expired entries, `cache.go:244`, and the orphan is expired by construction → clean `ErrCacheMiss`). **Payload without `.meta` leaks forever** — invisible to both `findBestMatch` and `DeleteExpired` (both `continue` on non-`.meta`, `:188`/`:232`).
- **There is no lease, refcount, generation or ETag guard anywhere in the package.** `Restore` opens a stream (`cache.go:142`) and extracts incrementally (`extract`, `:265-313`); `DeleteExpired` can `Delete` that key mid-stream. **This is the real W3-1 window** — a read-during-delete, not a DB/S3 ordering race.
- **`extract` writes files as it goes**, so a torn restore leaves the workspace **partially populated**; the error surfaces as `tar next: %w` (`:286`) → non-`ErrCacheMiss` → step fails → `markFailed` (`orchestrator.go:390`). **What S3 does to an in-flight GET whose key is deleted is NOT established** — measure it.
- **The agent uses its own S3 client**, separate from the controller's: `UNIFIED_CACHE_ENDPOINT`/`_KEY`/`_SECRET`/`_BUCKET` (`internal/config/agent.go:152-155`), assigned to `a.CacheStore` at `cmd/unified-cd-agent/main.go:214`. **Cache faults are injected against the agent's path, artifact/archival faults against the controller's** — they break independently.
- **BLOCKING: the cleanup interval is hardcoded and not configurable by any means.** `internal/controller/scheduler.go:223`: `ticker := time.NewTicker(24 * time.Hour)`; `RunCacheCleanup` takes **no interval parameter** (`:219`), unlike every other sweeper. Zero hits for `CACHE_CLEANUP` / `cleanup-interval` repo-wide. `time.NewTicker` does not fire immediately, so a fresh controller's first cleanup is at t+24h.
- **TTL floor is effectively 1 day via the DSL**: `cache.ttlDays` (`internal/dsl/types.go:444`, "default 30, max 365"), `<0` and `>365` rejected (`parse.go:614-619`), and **`0` is silently rewritten to 30** (`orchestrator.go:980-982`). The sidecar CLI is unconstrained (`cmd/unified-sidecar/run.go:103`) but only reachable on the k8s/isolated backend.
- **`UNIFIED_CACHE_BUCKET` is read (`config/agent.go:155`) but is NOT set in the dev compose** — whether it has a default or the dev cache is silently disabled is **not established**. Verify before relying on the agent cache path; a silent no-op here would invalidate the whole scenario.

### W3-2 — archival and artifacts

- Log archiver: `logArchiverLockKey = 0x6C6F6761` (`archiver.go:15`), interval **30 s** (`main.go:400`, defaulted at `archiver.go:20-22`). Eligibility `ListRunsNeedingArchival` (`postgres.go:1458-1467`): terminal status, no `run_log_archives` row, not in the excluded set, `ORDER BY updated_at LIMIT $1` with `limit=20` (`archiver.go:55`).
- **The object/record window exists and the order is object first**: `runLogArchiveKey(runID)` = `runs/<runID>/logs.ndjson` (`:76-78`) → `obj.Put` (`:96`) → `st.CreateLogArchive` (`:106`). On a `CreateLogArchive` failure it does a best-effort `obj.Delete` + `slog.Warn` (`:111-114`). `CreateLogArchive` is `ON CONFLICT (run_id) DO UPDATE` (`postgres.go:1523-1525`), so re-archival is idempotent. **There is no state where a record points at a missing object.**
- **An unarchivable run is retried with backoff, never marked.** `bo.Failure(run.ID, ...)` (`:62`), base 1 min → max 1 h, cap 10,000 (`:28`); excluded ids go into the SQL `!= ALL($2)` (`:55`). The only trace is `slog.Error("failed to archive Run logs", ...)` (`:61`) — nothing on the run, no log line, no status change. **The run itself is unaffected**; archival is post-terminal, so permanent failure means logs stay in `logs` and are never trimmed.
- **Artifacts have NO DB record.** No artifacts table, no `CreateArtifact`; `handleArtifactList` answers from `objStore.List(prefix)` and reconstructs names from keys (`api_artifacts.go:126-144`). **So the object/record question is inapplicable to artifacts.**
- **Artifact upload is ONE SHOT, no retry.** `executeUploadArtifact` calls `b.UploadArtifact` once (`orchestrator.go:815`), not wrapped in `retryUntilSuccess`; the caller likewise (`:401`); `Client.UploadArtifact` has no internal retry. On failure: `objStore.Put` error → 500 (`api_artifacts.go:80`) → step `Failed` (`orchestrator.go:817-820`) → `markFailed` (`:403`) → **run Failed**, cleanly, with an operator-visible failed step.
- A configured-but-down store yields **500**, not the 503 that `s.objStore == nil` produces (`api_artifacts.go:21-24`).
- **Trimmed-run log reads hang then 503**: `archivedLogsFetchTimeout = 3 * time.Minute` (`internal/controller/archived_logs.go:32`), singleflight-joined (`:41-47`), then 503 at `sse.go:83`. Only applies once `--log-trim-days` has trimmed a run.

### W3-3 — mixed-KEK replicas

- Envelope encryption, two layers, both version-prefixed (`internal/secrets/crypto.go:30-50`): random 32-byte DEK → `aesGCMEncrypt(dek, plaintext, b.canonical())` (AAD-bound to a `Binding`); DEK wrapped by `km.EncryptKey`; both blobs get a leading `CryptoVersion = 0x02` (`:14`, `withVersion` `:84-88`).
- KEK source: exactly one of three, ambiguity **rejected** rather than resolved by precedence (`internal/config/keysource.go:38-43`, `:77-107`) — `UNIFIED_CONTROLLER_KEY_FILE`, `UNIFIED_KMS_URI` (`hashivault://` only), or `UNIFIED_DEV_MODE=1` (ephemeral, new key per restart, warned at `:98-99`). **None → hard error + `os.Exit(1)`** (`:101-106`, `main.go:287-291`).
- Key file: `TrimSpace` then **exactly 64 hex chars** (`keysource.go:208-211`) → 32 bytes. The HA rig already does this — `test/ha/kek` mounted read-only into all three replicas at `/run/secrets/kek` via a **YAML anchor** (`docker-compose.ha.yaml:22-31`). Generate a second with `unified-cli keygen --out` (`internal/cli/keygen.go`).
- **THE SHARPEST AVAILABLE FINDING: there is no key id and no key version in the ciphertext.** The blobs carry only a *format* version (`0x02`, mismatch → `ErrUnsupportedVersion`, `crypto.go:90-98`) and a *provider* tag (`localKeyPrefix = "local:"`, `keymanager.go:25`, mismatch → `ErrProviderMismatch`). `LocalKeyManager` is `struct{ kek []byte }` (`:32-34`). **Two replicas with different key bytes are indistinguishable at the ciphertext level — the wrong-key replica cannot tell it has the wrong key.**
- The failure lands in the **DEK-unwrap** layer: `km.DecryptKey` (`crypto.go:63`) → `local:` matches → GCM auth fails → wrapped as `decrypt dek: %w` (`:65`). **This is NOT `ErrBindingMismatch`** (only produced at `:79`, after a clean unwrap). So `logSecretDecryptFailure` (`api_secrets.go:161-173`) falls past both special cases to the generic **`slog.Warn("secret decrypt failed", ...)`** at `:172` — **Warn, not Error**, and less loudly reported than a provider or binding mismatch, both of which get `slog.Error` with a diagnostic sentence.
- HTTP **500**, body prefixed `decrypt <name>: decrypt dek:` (`api_secrets.go:142`). The trailing stdlib text (`cipher: message authentication failed`) is **expected but unverified**.
- **The run fails cleanly and immediately.** `client.FetchSecrets` is called **once**, not retried (`orchestrator.go:162`); on error the agent appends a System `stderr` line at `stepIndex: -1` carrying `fetch secrets for run %s: %v` (`:175-184`), then `retryUntilSuccess(FinishRun(..., RunFailed))` (`:185-187`) and returns without running the DAG. ~~**The reason is visible in the run's own logs** — unusually well-instrumented; record that.~~ **CORRECTED AFTER EXECUTION (W3-3): the struck clause is FALSE, and it is the opposite of what the path delivers.** The agent's `Client.do` replaces the body of every response >= 400 with the literal `"response omitted"` (`internal/agent/client.go:107-108`), so the controller's precise message — built at `internal/controller/api_secrets.go:142` — is destroyed in transit before `orchestrator.go:175-184` can log it. All **23** failures logged one identical, content-free line: `fetch secrets for run <id>: http 500: response omitted` (a `SELECT DISTINCT` over all 23 returns exactly one value). The path is well-*structured* — one line, right stream, right `stepIndex`, written before `FinishRun` — and carries **no diagnostic content at all**. See the wave-level `"response omitted"` fact added to the facts block above; it applies to Tasks 4-7 too.
- Only runs with `len(c.SecretsNeeded) > 0` take this path (`:161`) — the fixture must actually reference a secret.
- `nginx.conf` round-robins with no affinity (`test/ha/nginx.conf:3-9`) and **`proxy_next_upstream` does not include 500** (`:23`), so nginx will **not** retry the decrypt failure against a healthy replica. Roughly 1-in-3 of secret-using runs should fail outright — **measure it**.
- ~~**The docs are explicit and correct here**: `docs/high-availability.md:204-206` ("Every replica must be given the same key... a replica started with a different key cannot read secrets written by another"), plus `:225` and the checklist at `:544`. So this is a **conformance / blast-radius measurement, not a contradicted contract**. Set the expectation accordingly.~~ **CORRECTED AFTER EXECUTION (W3-3): right about the HA guide, wrong about `docs/` as a whole — and the miss was caused by a truncated survey.** The three HA-guide citations are accurate and were not contradicted. But the survey backing "not a contradicted contract" was piped through `head` and kept **40** of the first grep's **55** hits; hit **#55** is **`docs/secrets.md:420`**, the Troubleshooting table's HA row, which prescribes "Give every replica the identical key file (or the same KMS URI)" as the unconditional fix for "`decrypt` errors in HA setup". Applied verbatim at `00:46:03.977Z`, it left **9 of 9 runs failing on all three replicas**, and its stated cause is false of the repaired cluster. Per `FINDINGS.md:478-479` a statement in `docs/` is a contract limb, so W3-3 filed a **major violation** there. **Method rule for Tasks 4-7: survey `docs/*.md` in full and print `| wc -l` next to the output before writing "the docs are silent" or "not a contradicted contract" into a plan or an entry.**

### W3-4 — bulk log append

- **Row by row, no transaction.** `handleAgentLogBulk` (`api_agent.go:697-739`) loops and calls `s.store.AppendLog` **once per line** (`:725`); each is a standalone `p.pool.QueryRow` (`postgres.go:926`), autocommitted individually.
- **A mid-batch error returns 500 with the prefix already committed**: `http.Error(...); return` from inside the loop (`:726-729`). No rollback, no compensation, no indication of how far it got.
- **No idempotency of any kind.** `api.LogAppendRequest` carries `RunID, StepIndex, Stream, Timestamp, Line` — no client id, no batch id, no nonce. The insert (`postgres.go:919-923`) has no `ON CONFLICT` and no unique constraint on any content column. **A retry after a lost ack duplicates the committed prefix exactly.**
- The agent does retry: `flushLocked` re-sends every batch still in `p.pending` (`runner.go:361-366`), and a batch lands there on **any** error (`:390-392`) — including a 500 raised after a partial commit and a client-side timeout on a request the server completed. Both are lost-ack shapes.
- `seq` is **server-side**, `RETURNING seq` (`postgres.go:923`); "Real seqs start at 1, so 0 is unambiguous" (`:917`). It appears to be **global, not per-run** — `TailLogs` filters `WHERE run_id = $1 AND seq > $2` (`:942`), which only works if `seq` is monotonic within a run. **Confirm from the migration if the plan leans on it.**
- **SSE reads from the DB by `seq`; it does not forward what was appended.** Backfill `TailLogsRecent` (`sse.go:69`), capped with a `truncated` event (`:88-91`); live via `LISTEN "log_appended:"+id` (`:117-118`) whose payload **is** the seq but whose callback **ignores it** and re-queries `TailLogs(dbCtx, id, lastSeq, 10_000)` (`:120`). Dropped lines emit **no** notify (`postgres.go:933-934`).
- **A duplicate prefix therefore appears as new lines with strictly higher `seq`, delivered normally** — the `seq > lastSeq` filter dedupes *transport* retries, not *content* duplicates. Visible in SSE/UI as repeated identical text in order; in the DB via `GROUP BY line HAVING count(*)>1`; **and in the archive** — `archiveRunLogs` encodes whatever `TailLogs` returns (`archiver.go:81-92`), so `lineCount`/`maxSeq` record the inflated count and log-trim's coverage check still passes. **The duplication is permanent.**
- **`Timestamp` is set once when the batch is built** (`runner.go:376`), not per attempt, so duplicates share a timestamp and differ only in `seq` — a clean discriminator.

### W3-5 — seal vs final flush

- **The seal is the mere existence of a `run_log_archives` row.** No boolean, no `sealed_at`. The guard is **inside the INSERT** (`postgres.go:919-923`): `... SELECT ... WHERE NOT EXISTS (SELECT 1 FROM run_log_archives WHERE run_id = $1::uuid) RETURNING seq`, with `pgx.ErrNoRows` → `return 0, nil` (`:927-928`). Atomic per line, no extra round trip.
- **Only `CreateLogArchive` (`postgres.go:1519-1528`) sets it, called only from `archiveRunLogs` (`archiver.go:106`)** — the archiver, and nothing else, seals.
- **Sealed-run appends are dropped with a 204 and a controller-side warning.** Single: `seq == 0` → `slog.Warn("dropping log line for sealed run", ...)` then 204 (`api_agent.go:564-570`), rationale at `:565-567` ("204 keeps unmodified agents from retry-storming"). Bulk: aggregate `slog.Warn("dropping log lines for sealed run", "run", droppedRun, "dropped", dropped)` (`:719-737`) — note it logs only **the last run id seen** (`:732`), so in a mixed-run batch earlier runs' drops are counted but unattributed.
- **The 204 means the agent believes it succeeded and clears the batch.** So the loss is **silent on the agent side and visible only in controller logs** — no drop marker, no run log line, nothing in the DB. **This is a different loss channel from W1-6's** (which was abandonment before the socket); here the write reaches the controller and is discarded server-side with a success ack. Do not merge them and do not re-measure W1-6's tail.
- **NOTHING orders terminal-status publication against the seal.** A run becomes sealable the instant `FinishRun` commits (candidate set is terminal status, `postgres.go:1462`), and the next 30 s tick can seal it. **But `CloseScopes` runs AFTER `FinishRun`**: `defer b.CloseScopes(context.WithoutCancel(ctx))` at `orchestrator.go:209` executes when the body returns, i.e. after `FinishRun` at `:787`; it stops the sidecar pump (`backend_host.go:182-184`) whose goroutine does `stdout.Flush` / `stderr.Flush` on a `context.WithoutCancel` so final lines ship (`sidecar_logs.go:82-85`).
- ~~**So for any run with a sidecar, log lines are flushed strictly after the run is terminal, by construction, on every run.** The codebase states this ordering itself at `api_agent.go:744-748` — but applies it to sidecar *status* (given `rejectTerminal=false` to accommodate it). **The same post-terminal window applies to sidecar *logs*, which have no such accommodation.**~~ **CORRECTED AFTER W3-5 EXECUTION, on both halves.** (i) **The ordering is real but the race is NOT structural.** `defer b.CloseScopes(...)` (`orchestrator.go:209`) does run after `FinishRun` (`:787-788`) — but by **microseconds to milliseconds**, being the deferred call on the same return — while the archiver can only seal on its next **30 s** tick after that same `FinishRun` commits (`cmd/controller/main.go:400`, hardcoded, no flag, no env). **The sidecar flush therefore wins the race essentially always**, losing only if a tick lands inside that sub-millisecond gap; "every run" is an over-read of a real ordering. The realistic producer of this loss is **delayed delivery** (partition, reaper, restart), which is what W3-5's Part B produced naturally on its first attempt. (ii) **The "logs have no accommodation" half is wrong too**: both log endpoints already pass `rejectTerminal=false` (`api_agent.go:551`, `:709`) and **neither is an F2 endpoint** — what stops a late log line is the *seal*, a different and strictly later gate. The status comment spans `:741-752`, not `:744-748`. Sidecars are in any case unavailable on this rig (`test/edgecase/README.md`), so no arm could test the structural shape.

  | Source | Flush point vs `FinishRun` | Seal-race exposure |
  |---|---|---|
  | Main step stdout/stderr | per-step, before run end (`backend_host.go:382-386`) | only via Flush abandonment (W1) |
  | Post-hook output | `finishPostLogs` (`orchestrator.go:706`), before `:787` | same |
  | `finally` steps | within `RunPipeline` (`:727`), before `:787` | same |
  | **Sidecar logs** | **`CloseScopes`, after `:787`** | ~~**structural, every run**~~ **CORRECTED (W3-5): not structural — the flush precedes the next 30 s seal tick essentially always; see above** |

- The archiver interval **is** a parameter (`archiver.go:19`) but is hardcoded at the call site (`main.go:400`) with no flag or env — widening it needs a code change. **Sealing by hand (`INSERT INTO run_log_archives`) at a chosen instant is the recommended lever.**

### W3-6 — retention vs in-flight upload

- `runRetentionLockKey = 0x7272746E` (`run_retention.go:17`), interval `time.Hour` (`main.go:426`, defaulted at `:30-32`), **hardcoded at the call site**. `runRetentionDaysDefault()` returns **0** on empty env (`main.go:47-58`) and the job early-returns on `retentionDays <= 0` (`:27-29`).
- `deleteRunEverywhere` (`run_retention.go:131-173`), also used by the manual DELETE: (1) `GetLogArchive` → `obj.Delete(arch.ObjectKey)` (`:133-141`); (2) `obj.Delete(runLogArchiveKey(runID))` **unconditionally** (`:145-147`); (3) `obj.List("artifacts/"+runID+"/")` then delete each (`:148-156`); (4) `st.DeleteRun` **last** (`:158-160`); (5) **`obj.Delete(runLogArchiveKey(runID))` AGAIN**, best-effort (`:167-170`).
- Objects before the DB row, deliberately (`:102-106`): "a failure leaves the run intact for a later retry, never an orphaned object." `Delete` is nil for missing keys, so retries are idempotent.
- **THE FINDING IS AN ASYMMETRY.** Step 5 exists specifically to catch a **log archive** written during the deletion window — the code names this "race (b)" and closes it (`:120-130`, `:161-166`). **There is no equivalent post-`DeleteRun` re-sweep of `artifacts/<runID>/`.** Step 3 runs once, before `DeleteRun`. An artifact `Put` committing after it leaves an object under a prefix that will never be listed again: `ListExpiredRuns` is driven off `runs` rows (`postgres.go:1494-1502`) and the row is gone; **no orphan-object reconciler exists anywhere** (the only prefix lists in the tree are `caches/`, `jobPrefix`, and `artifacts/<runID>/`). **ADDED BY W3-6 REVIEW (code-read, NOT executed): there is a third producer of this class with no race at all** — `DELETE /api/v1/jobs/{name}` calls only `store.DeleteJob` (`api_jobs.go:220-231` → `postgres.go:2168-2171`, a bare `DELETE FROM jobs`), never reaches `deleteRunEverywhere` (which has exactly two callers, `api_runs.go:433` and `run_retention.go:80`), and cascades every `runs` row away (`001_init.up.sql:620`), so **every artifact and every log archive of every run of a deleted job survives unconditionally**. Filed as its own **major** entry at the end of `FINDINGS.md`; **a later wave should measure it** (trigger → archive → upload → job delete → `mc ls`).
- **The guard that partially mitigates it is a TOCTOU.** `handleArtifactUpload` checks run existence before the `Put` (`api_artifacts.go:55-67`), with a comment naming this exact hazard ("A late upload for a deleted run would create an orphaned object nothing ever cleans up"). But `GetRun` is at `:55` and `Put` at `:79` with no lock or lease between, and the upload is chunked with no Content-Length (~~`client.go:352-360`~~ — **CORRECTED, twice: the function is `client.go:349-371`; W3-2 filed this first and W3-6 re-confirmed it**), so **`Put` duration is bounded only by payload size**. **CONFIRMED LIVE BY W3-6 (Task 5):** 32 MiB fed at `--limit-rate 1M` gave a controller-side `duration_ms` of **32374** on one `PUT`, and a `DELETE` fired 591 ms into that window (on an `mc ls --incomplete` signal, not a sleep) left a 32 MiB object behind with no `runs` row. Hit on attempt 1 of a cap of 6. **This is the W3-6 window, and it is a gap the code comment believes it has closed.**
- **Terminal-but-existing runs still accept uploads** (`api_artifacts.go:59-61`; the comment as a whole spans `:57-61` and its first half is the orphan warning), which is what makes the scenario constructible. **CONFIRMED LIVE BY W3-6.** But note what the plan did not say and Task 5 had to work out: `DELETE /api/v1/runs/{id}` needs a terminal run (409 otherwise) while an agent-driven run is `Running` for the whole of its `uploadArtifact` step. **Task 5 first concluded from that pair that no ordinary run can supply the precondition; that conclusion was wrong and is corrected here.** `handleCancelRun` marks the row terminal **synchronously** (`MarkRunFinished(..., api.RunCancelled)`, `api_runs.go:374`) while the agent only notices on a poll — `var CancelPollInterval = 5 * time.Second` (`internal/agent/orchestrator.go:37`), poller `:122-147`, terminal branch `:137-146`, which also covers reap and failover — so **an ordinary agent-driven run is terminal-while-still-uploading for up to 5 s and the manual DELETE is legal throughout**. **That window was never attempted**, so it is untested rather than unreachable, and whether the `Put` beats `cancelRun()` is open. W3-6 instead built a curl-driven **synthetic agent** (`test/edgecase/workloads/w36-probe.yaml`, `agentSelector: [kind:w36probe]`, enroll → claim → finish → upload; five API calls, no SQL) because it makes the window as wide as the payload and repeatable on demand — **a convenience, not the only way in.** A later wave with spare rig time should produce the cancel-driven window.
- **Use the manual route, not the sweeper.** `DELETE /api/v1/runs/{id}` (`server.go:375`) calls the **identical** `deleteRunEverywhere` synchronously (`api_runs.go:433`), requires the run to be terminal (`:427-432`, else 409 — which the scenario needs anyway) and `requireMinRole("developer")` (`server.go:359`), satisfied by `ha-admin-token`. Instant, precisely-timed, no config change, no backdating, no rebuild.

### Injection

- **No endpoint-level lever exists.** `UNIFIED_S3_*` is read once at startup, no reload path, no fault hook in `internal/objectstore/`. A bad endpoint at startup is fatal, so "start a replica with broken S3" yields a crashloop, not a degraded replica.
- **`inject.sh` works against a `garage` service unmodified** — its container pattern is `unified-cd-ha-$svc-1` (`inject.sh:25-26`). `pause`/`unpause` gives **hangs/timeouts** (the realistic outage, and the one that exercises timeout paths); `kill-hard` gives **fast refusals**; `partition`/`heal` gives a **blackhole**. `nginx-block` and `steplock` do **not** apply — the HA nginx fronts controllers only.
- **Missing: any partial or method-selective S3 fault.** Every lever is all-or-nothing. W3-1 needs `Delete` to succeed while a `Get` stream is open; W3-2 wants `Put` to fail while reads work. **Task 3 builds an interposer for this.**
- **Verify Garage is on `unified-cd-ha_default`** or `partition` silently no-ops (`inject.sh:25`).
- **No `mc`/`aws` client in the stack** for out-of-band object manipulation. Whether Garage's own `/garage` CLI can delete an individual S3 object is **not established**.

## Facts NOT established — open questions, not givens

- What S3/Garage does to an in-flight GET whose key is deleted mid-stream (W3-1's core mechanism).
- Whether `UNIFIED_CACHE_BUCKET` has a default; the dev compose does not set it, so the dev agent cache may be a silent no-op. **Task 3 must settle this.**
- Whether the `logs.seq` column is a shared sequence (global) or per-run. Read the migration.
- The exact stdlib error text after `decrypt <name>: decrypt dek:`.
- Whether the HA agents can run **sidecars** at all (`agent.Dockerfile`, `kind:linux`, `native: true`). **This is a real risk to W3-5 and Task 3 must settle it early.**
- ~~Whether `handleArtifactList`'s route group enforces run existence.~~ **SETTLED BY W3-6 (Task 5): it does NOT.** The group is `server.go:496-499` with only `agentOrServerAuth` and no `requireMinRole`; `handleArtifactList` (`api_artifacts.go:120-147`) never calls `GetRun`. Measured against a deleted run: `GET /runs/{id}` **404** while `GET /runs/{id}/artifacts` **200** with contents and `GET /runs/{id}/artifacts/{name}` **200** with 33554432 bytes — for a server token *and* for an agent credential. Filed as a W3-6 observation.
- Garage's SIGTERM drain behaviour.

---

### Task 1: W3-4 — bulk log append partial commit + lost ack

**Runs on today's rig with no infrastructure change. Do it first.**

**Files:** Create `test/edgecase/scenarios/w3-4-bulk-append-duplication.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I4

- [ ] **Step 1: Settle the open question first** — read `internal/store/migrations/` and determine whether `logs.seq` is a shared sequence or per-run. Record the answer with file:line in the runbook; several claims depend on it.
- [ ] **Step 2: Write the runbook.** Stack: plain `test/ha`, no overlay.
  - **Part A — the duplicate.** Use a chatty workload (`sideeffect.payload.json` emits 120 lines; `tick.payload.json` 30). Inject a **lost ack**: the request completes server-side but the agent sees a failure. The natural instrument is the campaign's existing per-URI nginx idiom (`test/edgecase/compose/nginx-steplink.conf` is the template) applied to `POST /api/v1/agents/*/logs/bulk` — but note the W2-5 lesson: **`nginx -s reload` is not guaranteed to take effect on an already-connected agent**, and W2-5 has a captured counter-example. Probe-confirm the arm before relying on it, and bracket the window with logged denials.
  - **Part B — the partial commit.** A mid-batch `AppendLog` failure returns 500 with the prefix committed. Injecting a failure *inside* the loop needs the DB to fail mid-batch — `inject.sh pause postgres` timed into a bulk request is the available lever. Cap attempts, report the count either way, and if you cannot hit it file the code-read argument with an explicit "not reproduced live" label (W2-3's Arm D did exactly this at 0/10 and it was accepted).
  - **Measure the duplication precisely:** `SELECT line, ts, count(*), array_agg(seq ORDER BY seq) FROM logs WHERE run_id=... GROUP BY line, ts HAVING count(*)>1`. Duplicates share a `ts` and differ only in `seq` — use that as the discriminator, and say so.
  - **Then follow it downstream**, which is what makes this I4 rather than a curiosity: confirm the duplicate appears in the SSE stream (`GET /runs/{id}/events`), and confirm it is **archived** — but note archival needs an object store, which this rig lacks, so the archive limb is **code-read only** (`archiver.go:81-92`) unless deferred to after Task 3. Say which.
  - Recording: duplicated committed log content = judge against I4's exact wording, quoted. A 500 that commits a prefix with no way for the client to know how far it got = judge on whether any doc promises atomicity; if silent, say "silent, not contradicted" and rest on I4.
- [ ] **Step 3: Commit runbook.** **Step 4: Execute.** **Step 5: Findings + teardown `-v` + commit** (scenario id w3-4).

---

### Task 2: Secrets fixture + W3-3 — mixed-KEK replicas

**Runs on today's rig plus a second key file. No object store needed.**

**Files:** Create `test/edgecase/workloads/secret-user.payload.json`, `test/edgecase/compose/mixedkek.override.yaml`, `test/edgecase/scenarios/w3-3-mixed-kek.md`; Modify `test/edgecase/FINDINGS.md`, `test/edgecase/README.md`

**Invariants:** I1, I7

- [ ] **Step 1: Build the fixture.** Adapt `examples/jobs/secrets.yaml` to the campaign envelope (`{"yaml":"..."}`, `native: true`, `agentSelector: [kind:linux]`) as `secret-user.payload.json`, job `edge-secret-user`. It must actually reference a secret so `len(c.SecretsNeeded) > 0`. Register the secret via the secrets API first — find the route and record it in the runbook. **Verify the payload through the real `dsl.Parse` with `KnownFields(true)`** and paste the output; W1 shipped two payloads that 400'd because a key path was wrong.
- [ ] **Step 2: Build the overlay.** `mixedkek.override.yaml` gives **`controller3` a different key file**. This requires breaking the YAML anchor at `docker-compose.ha.yaml:15` for that one service — do it in the overlay, not by editing `test/ha/`. Generate the second key with `unified-cli keygen --out` and commit it as a fixture under `test/edgecase/compose/` (it is a test KEK for a throwaway stack, like `ha-admin-token`; say so in a comment). Confirm the file is exactly 64 hex chars after `TrimSpace`.
- [ ] **Step 3: Write the runbook.**
  - **BASELINE GATE:** all three replicas up; a secret written and read back successfully through the LB **before** the overlay is applied; then with the overlay, confirm `controller3` started (it must not crashloop — a *wrong* key is still a *valid* key file).
  - **Part A — the blast radius.** Trigger `edge-secret-user` N times (N ≥ 20) and measure the failure fraction. Expected ~1/3 given round-robin with no affinity and no `proxy_next_upstream` on 500 — **measure it, do not assume it**, and attribute each failure to a replica from the controller logs.
  - **Part B — what the operator sees.** Capture the full chain for one failure: the HTTP 500 body, the `slog.Warn("secret decrypt failed", ...)` line, and the System `stderr` log line at `stepIndex: -1` in the run's own logs. **The point of this part is the asymmetry**: a wrong local KEK gets `Warn` while a provider or binding mismatch gets `Error` with a diagnostic sentence (`api_secrets.go:161-173`). Verify and record it.
  - **Part C — the key-identity question.** Confirm from the captured ciphertext that it carries only `0x02` and `local:` and **no key id or version** — so the bad replica cannot self-diagnose. Read a `secrets` row and show the prefix bytes. This is the entry's strongest limb.
  - Recording: **the docs are explicit and correct here** (`docs/high-availability.md:204-206`, `:225`, `:544`), so this is a **conformance / blast-radius measurement**, not a contradicted contract. Expect observations. A violation would require something the docs do *not* promise — the absence of key identity, or a failure mode noisier/quieter than documented. Judge honestly; do not manufacture a violation to make the scenario feel productive.
- [ ] **Step 4: Commit fixture + overlay.** **Step 5: Execute.** **Step 6: Findings + teardown `-v` + commit** (scenario id w3-3).

---

### Task 3: Infrastructure — Garage in the HA rig, the S3 interposer, and the remaining fixtures

**This unblocks Tasks 4-7. It is the wave's riskiest task; treat its open questions as deliverables.**

**Files:** Modify `test/ha/docker-compose.ha.yaml`; Create `test/edgecase/compose/s3proxy.override.yaml`, `test/edgecase/compose/nginx-s3.conf`, `test/edgecase/workloads/artifact-large.payload.json`, `cache-user.payload.json`, `sidecar-logger.payload.json`; Modify `test/edgecase/tools/inject.sh`, `test/edgecase/README.md`

- [ ] **Step 1: Add Garage to `test/ha/docker-compose.ha.yaml`.** This is the one sanctioned edit to `test/ha/` this wave — justify it in the commit message: four of six scenarios are unrunnable without it, and an overlay cannot add the `depends_on` the controllers need. Copy the service verbatim from `docker-compose.yaml:47-64` (image `dxflrs/garage:v2.3.0`, `--single-node --default-bucket`, the `GARAGE_DEFAULT_*` env, `deployments/garage/garage.toml` mount, the healthcheck). Add to the `&ctrl` anchor: `UNIFIED_S3_ENDPOINT: garage:3900`, `UNIFIED_S3_BUCKET: unified-cd-logs`, `UNIFIED_S3_KEY: garageadmin`, `UNIFIED_S3_SECRET: garageadmin12345`. **Two load-bearing requirements:** `depends_on: garage: {condition: service_healthy}` on every controller (because `BucketExists` is eager and failure is `os.Exit(1)`), and a **dedicated named volume** — cache and artifact state is now persistent across restarts in a way this rig has never had, so `down -v` between scenarios becomes mandatory.
- [ ] **Step 2: Settle three open questions and record each answer with file:line or captured output.** These are deliverables, not preliminaries.
  - **(a) `UNIFIED_CACHE_BUCKET`** — read `cmd/unified-cd-agent/main.go` around `:214` and determine whether it defaults or whether an unset bucket silently disables the agent cache. Then wire the agents (`UNIFIED_CACHE_ENDPOINT: garage:3900`, `_KEY`, `_SECRET`, `_BUCKET`) and **prove a real cache hit end to end** — `examples/jobs/cache.yaml`'s own header warns the step is a no-op with no store, so a fixture that "passes" proves nothing.
  - **(b) Can the HA agents run sidecars at all?** Read `test/ha/agent.Dockerfile` and the backend selection for `native: true` / `kind:linux`. **If they cannot, W3-5's sidecar vehicle is unavailable** — say so now and propose the alternative (seal by hand against a post-hook or `finally` flush, accepting that those flush *before* `FinishRun` so the structural window is absent and the scenario becomes a hand-timed demonstration rather than a natural race). Do not discover this in Task 6.
  - **(c) Out-of-band object manipulation** — establish whether Garage's `/garage` CLI can delete an individual S3 object. If not, add a one-shot `minio/mc` service in the style of the existing `agent-enroll` one-shot (`docker-compose.ha.yaml:53-95`). W3-1's injection and W3-6's orphan verification both need it.
- [ ] **Step 3: Build the S3 interposer.** An nginx service between the controllers and Garage, with `UNIFIED_S3_ENDPOINT` pointed at it, following the campaign's existing overlay idiom (`nginx-edge.conf`, `nginx-steplink.conf`). It must support **method- and prefix-selective** failure — at minimum: fail `PUT` while `GET` succeeds; fail a given key prefix; and add latency. Drive it by a blocklist-file-plus-reload like `nginx-block` already does, and add `inject.sh` verbs (`s3-block <METHOD|prefix>`, `s3-clear`, `s3-latency`). **Carry the W2-5 caveat into the tooling docs:** a reload's effect on an already-established connection is not guaranteed, so every arm needs a probe confirmation.
- [ ] **Step 4: Build the remaining fixtures**, adapted from `examples/jobs/` to the campaign envelope and verified through `dsl.Parse`:
  - `artifact-large.payload.json` — job `edge-artifact-large`, uploads an artifact **big enough that the `Put` window is measurable** (W3-6's TOCTOU width is bounded by payload size). State the size and how long the upload takes.
  - `cache-user.payload.json` — job `edge-cache-user`, a cache save+restore pair with the **lowest TTL the DSL permits** (note `0` is silently rewritten to 30, so the floor is 1).
  - `sidecar-logger.payload.json` — job `edge-sidecar-logger`, a sidecar that emits output continuously so its post-`FinishRun` flush is observable. **Only if Step 2(b) says sidecars are available.**
- [ ] **Step 5: Verify the whole rig end to end** and record it as the baseline every later task cites: all three controllers up with Garage healthy; a run archived (`run_log_archives` row + the `runs/<id>/logs.ndjson` object); an artifact uploaded and listed; a cache hit. **Any of these failing blocks the dependent task — say which.**
- [ ] **Step 6: Commit** (compose amendment, interposer, `inject.sh` verbs, fixtures, README updates — separate commits are fine and preferred).

---

### Task 4: W3-2 — S3 outage during log archival and artifact upload

**Files:** Create `test/edgecase/scenarios/w3-2-s3-outage.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I4, I5

- [ ] **Step 1: Write the runbook.**
  - **Keep this on the controller side of the socket.** The agent-side log-loss half is W1-6 territory — cite it, do not re-measure it.
  - **Part A — archival under a full outage.** `inject.sh pause garage` (hangs) and separately `kill-hard` (fast refusals) while runs are finishing. Confirm: the run is **unaffected** (archival is post-terminal), the only trace is `slog.Error("failed to archive Run logs", ...)`, and the run stays in the candidate set. Then measure the **backoff behaviour across three replicas** — per the corrected fact, `bo` is per-process, so expect up to one retry per replica per window, and each replica that has never been leader has an empty exclusion set. **Measure the actual retry count and attribute each attempt to a replica**; this is the arm that tests a fact this plan corrected.
  - **Part B — the object/record window.** Use the interposer to fail **`CreateLogArchive`'s side** while `Put` succeeds — i.e. let the object land, then break the DB, or simply pause postgres timed into the archive. Confirm the compensating `obj.Delete` (`archiver.go:111-114`) and that a failure of *that* delete leaves an orphan which the later `deleteRunEverywhere` step-5 re-sweep would catch. State which interleavings you actually produced and which are code-read.
  - **Part C — artifact upload, one shot.** With the interposer failing `PUT`, confirm the upload is attempted **once**, the step is reported `Failed`, and the run fails — cleanly and visibly. Then confirm the contrast that makes this notable: a **configured-but-down** store gives **500**, not the 503 of an unconfigured one.
  - **Part D — the fatal-startup finding.** With Garage down, restart a controller and confirm it **crashloops** (`os.Exit(1)` from `main.go:311-313`). Judge severity against the docs: does anything document that a controller cannot start without object storage? If not, "silent, not contradicted" and rest on I5.
  - Recording: a run failed as collateral damage of an archival failure = major (I4/I5). An orphaned object with no reconciler = judge against W3-6's finding and **cross-reference rather than double-file**. The crashloop = judge on the documented startup contract.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w3-2).

---

### Task 5: W3-6 — run retention racing an in-flight artifact upload

**The cheapest of the Garage-dependent scenarios. Use the manual DELETE route.**

**Files:** Create `test/edgecase/scenarios/w3-6-retention-vs-upload.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I4

- [ ] **Step 1: Write the runbook.**
  - **Part A — the TOCTOU.** Start a large artifact upload (`edge-artifact-large`) on a terminal run, then `DELETE /api/v1/runs/{id}` timed to land **after** `handleArtifactUpload`'s `GetRun` (`api_artifacts.go:55`) and **before** its `Put` completes (`:79`). The upload is chunked with no Content-Length, so the window is as wide as the payload. Confirm the object exists under `artifacts/<runID>/` after the run row is gone.
  - **Part B — prove it is unreachable.** This is what turns a plausible story into a finding: show that nothing will ever find the orphan. `ListExpiredRuns` is driven off `runs` rows and the row is gone; there is no orphan reconciler anywhere. Verify by listing the prefix out-of-band (needs the `mc` service or `/garage`) **and** by confirming the API cannot enumerate it.
  - **Part C — the asymmetry, which is the finding.** Show that a **log archive** written in the same window IS caught, by step 5's second `obj.Delete` (`run_retention.go:167-170`), and that there is **no equivalent artifact re-sweep**. Cite the code's own "race (b)" comment (`:120-130`) — the comment believes it closed this class, and it closed half of it.
  - **Part D — the sweeper path**, optional and only if cheap: `UNIFIED_RUN_RETENTION_DAYS=1` plus `UPDATE runs SET updated_at = NOW() - INTERVAL '2 days'`, then wait up to 1 h for a tick. **Note a controller restart re-arms the ticker at t+1h rather than firing immediately.** If you skip it, say so and note that Part A already exercises the identical `deleteRunEverywhere`.
  - Recording: an orphaned object no code path can ever reach = judge against I4's exact wording. The TOCTOU itself = judge on whether the guard's own comment constitutes a contract (it is an inline comment on an exported handler — **decide and justify**; the campaign's rule disqualifies an *unexported helper's* doc comment, which this is not).
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w3-6).

---

### Task 6: W3-5 — archive seal racing the final log flush

**Files:** Create `test/edgecase/scenarios/w3-5-seal-vs-flush.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I4

- [ ] **Step 1: Write the runbook.**
  - **State the W1 boundary in the preamble, before anything else.** W1-6 measured loss where the agent never got its bytes to the controller (5 s `Flush` budget). **W3-5 is loss where the bytes arrive, are accepted with a 204, and are discarded by the seal guard** — different mechanism, different evidence, different fix. If a W3-5 run also loses a step-log tail, attribute that half to W1-6 and cite it.
  - **Part A — the structural race** (only if Task 3 Step 2(b) confirmed sidecars). `edge-sidecar-logger`: `FinishRun` commits → archiver seals within 30 s → `CloseScopes` flushes the sidecar's buffered lines → 204 + `slog.Warn` + silent loss. Measure how many lines are lost and confirm **nothing** on the agent side or in the DB records it. If sidecars are unavailable, run the hand-sealed variant Task 3 proposed and **label it a demonstration of the code path, not a natural race**.
  - **Part B — the exact seal semantics.** Confirm the guard is inside the INSERT (`postgres.go:919-923`) and atomic per line, that `seq == 0` is the sentinel, and that the agent's 204 clears the batch from `pending` so it never retries. **The silence on the agent side is the finding's core** — a 204 means it believes it succeeded.
  - **Part C — the mixed-batch attribution gap.** The bulk warning logs only the **last** run id seen (`api_agent.go:732`), so in a batch spanning runs, earlier runs' drops are counted into `dropped` but their ids never appear. Construct a mixed batch if you can; if not, file it code-read with the file:line.
  - **Part D — the codebase's own inconsistency.** `api_agent.go:744-748` documents the post-`FinishRun` ordering and gives sidecar *status* an accommodation (`rejectTerminal=false`); sidecar *logs* get none. That is the sharpest available framing — the project knows about the window and closed it for one of the two things that go through it.
  - Recording: log lines accepted with a success ack and then discarded = judge against I4 quoted verbatim, and against the loud-loss contract in `docs/troubleshooting.md` that W1-2 used. Distinguish "documented" from "silent".
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w3-5).

---

### Task 7: W3-1 — cache TTL expiry racing a restore

**The hardest scenario. Its premise was wrong and its scheduler lever does not exist.**

**Files:** Create `test/edgecase/scenarios/w3-1-cache-delete-vs-restore.md`; Modify `test/edgecase/FINDINGS.md`

**Invariants:** I4

- [ ] **Step 1: Write the runbook.**
  - **Amend the premise in the preamble.** There is no DB row — the cache is entirely object-resident, TTL lives in the `.meta` object, and the real window is a **read-during-delete with no lease, refcount or ETag guard anywhere in `internal/cache/`**. The scenario as originally specified (a DB/S3 ordering race) does not exist.
  - **Do not wait for the cleanup job.** Its interval is hardcoded at 24 h with no configuration lever of any kind (`scheduler.go:223`), and `time.NewTicker` does not fire immediately. Drive the delete by hand: compute `caches/<b64url(sha256(jobName))>/<b64url(sha256(key))>.tar.zst` per `cache.go:49-53` and delete the payload object while a restore is streaming it. **This tests the actual window and skips the scheduler entirely.** Record the hardcoded interval as its own finding — every other sweeper takes an interval parameter.
  - **Part A — the torn restore.** Delete the payload mid-`extract`. Measure: what error surfaces, whether the step fails, and — the part that matters — **whether the workspace is left partially populated** (`extract` writes files as it goes, `cache.go:265-313`). A partially-restored workspace that a later step then builds on is materially worse than a clean miss.
  - **Part B — what Garage actually does to an in-flight GET whose key is deleted.** This is listed as NOT established. Measure it and report it as a fact about the backend, clearly labelled as such rather than as a unified-cd behaviour.
  - **Part C — the two half-done states.** Produce each and confirm the asymmetry: `.meta` without payload is handled cleanly (expired by construction, skipped by `findBestMatch`); **payload without `.meta` leaks forever**, invisible to `findBestMatch` and `DeleteExpired` alike. `Save` compensates for its own failure (`cache.go:98-107`) but nothing reconciles this state if it arises another way.
  - **Part D — the deletion order.** `DeleteExpired` deletes archive then `.meta` with `continue` on either failure (`:204`, `:207`) — no transaction. Show that an interrupted cleanup produces the *benign* orphan, not the leaking one, and say whether that ordering looks deliberate.
  - Recording: a partially-populated workspace presented to a subsequent step = judge against I4 quoted verbatim, and consider whether it reaches I2 (does a step observe half-restored inputs and act on them?) — but remember I2 is *at-most-once* and a zero-vs-once shape does not violate it. The leaking orphan = judge on whether any doc promises cache-storage bounds.
- [ ] **Step 2: Commit runbook.** **Step 3: Execute.** **Step 4: Findings + teardown `-v` + commit** (scenario id w3-1).

---

### Task 8: W3 checkpoint

**Files:** Modify `test/edgecase/FINDINGS.md`, `test/edgecase/README.md`, `<project parent>/edgecase-evidence/README.md`

- [ ] **Step 1: Append `## Checkpoint: W3 complete`** following the W2 checkpoint's format and classification rule. Report **violation entries** and **new violations** separately if any entry completes another wave's defect. State at minimum:
  - (a) **Whether I4 was actually exercised this wave** — W2's checkpoint recorded I4 as a coverage gap rather than a pass, and closing it was W3's main purpose. Say plainly which scenarios produced I4 evidence and which did not.
  - (b) The `RunGitResolver` item carried from W2-1: it needed a git-backed fixture. W3 added object storage but not a git server — say whether it is still open and which wave should take it.
  - (c) What the Garage addition to `test/ha/` means for later waves (W4 runs on kind, W6 needs it for the log/NOTIFY hotspot).
  - (d) The methodological record: this plan's facts block **corrected three previously-established campaign facts** before the wave began (archiver backoff scope, the drop-marker gating, `UploadArtifact` bypassing `c.do`). Note whether execution corrected any further, and whether the "claims-to-check" discipline is still earning its keep in a third consecutive wave.
- [ ] **Step 2: Archive the evidence** to `<project parent>/edgecase-evidence/w3/`, one subdir per scenario, verify with `diff -r`, and update both READMEs' layout tables and coverage notes. Prefer `.gz` for statement logs and **state uncompressed sizes if an entry cites a file by exact size** (W2-9's captures are cited that way).
- [ ] **Step 3: Commit** (`test(edgecase): record W3 checkpoint`).
