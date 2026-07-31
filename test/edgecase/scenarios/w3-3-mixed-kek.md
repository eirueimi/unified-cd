# W3-3 — mixed-KEK replicas: one replica with the wrong key, and a ciphertext that carries no key identity

> **CORRECTED AFTER EXECUTION — READ THIS BEFORE ANYTHING ELSE.**
> **The scenario's framing survived in part: this was a conformance and
> blast-radius measurement, and I1 and I7 held on the runs this scenario is
> about.** Nothing in the Invariants block is withdrawn. What execution changed
> is one inherited fact that was the load-bearing premise of Part B, one stale
> schema fact, and three procedural details. **This banner originally also said
> ~~"the docs were right"~~ and claimed a yield of three observations plus one
> minor violation. Both were corrected in review — see correction 6: the docs
> were right in the HA guide and wrong in `docs/secrets.md:420`, a difference a
> truncated survey had hidden, and the scenario yields TWO observations and TWO
> violations.**
> Superseded text is kept in place and marked, per house style, because the
> reason it was wrong is the deliverable.
>
> 1. **THE PLAN'S CLAIM THAT "THE REASON IS VISIBLE IN THE RUN'S OWN LOGS" IS
>    FALSE, AND IT IS THE OPPOSITE OF WHAT THE FACTS BLOCK PROMISED.**
>    `docs/superpowers/plans/2026-07-30-edge-case-campaign-w3.md:84` says the
>    failing path is "unusually well-instrumented" because "the reason is
>    visible in the run's own logs — record that". The run's own log line, all
>    23 times, is **`fetch secrets for run <id>: http 500: response omitted`**
>    (`w3-3/partB-runlogs.txt`, `w3-3/consolidated.txt`). `Client.do` replaces
>    the body of **every** response ≥ 400 with the literal string
>    `"response omitted"` (`internal/agent/client.go:107-108`; the same string
>    is what the unused helper `safeResponseBody` at `:134` returns), so the
>    controller's precise message — built at
>    `internal/controller/api_secrets.go:142` — is discarded by the agent
>    before it can ever be logged. The path is well-*structured* (one line, the
>    right stream, the right `stepIndex`) and carries **no diagnostic content
>    at all**. Fact 11 of §"Verified mechanism" is correct about the mechanism
>    and its final clause about the *reason* being visible is struck below.
> 2. **The live `secrets` table has six columns, not eight — and this makes
>    Part C stronger, not weaker.** Fact 5 was read from
>    `internal/store/migrations/001_init.up.sql:247-256`, which is stale:
>    `016_drop_secret_scope.up.sql:13-18` drops `scope` and `scope_ref` and
>    replaces the composite unique with `secrets_name_key UNIQUE (name)`. The
>    C2 query errored (`column "scope" does not exist`, kept in
>    `w3-3/partC-ciphertext.txt` as the only `ERROR:` in the capture — do not
>    misread it as a product fault). **Always `\d` the live table; a v1 init
>    migration is a snapshot, not the schema.**
> 3. **The HTTP 500 body was NOT captured on the wire, and could not be.** The
>    brief asks for it; the endpoint is agent-authenticated and guarded by
>    `agentRunGuard`, and the only client that reaches it throws the body away
>    (correction 1). The body is therefore reported as **derived** from two
>    captured inputs — the format string at `api_secrets.go:142` and the
>    `err.Error()` text, which the controller's own `slog.Warn` does log
>    verbatim. **That the body is unobservable from the agent side is itself
>    part of the finding**, not a gap in the measurement.
> 4. **Round-robin does not give a clean per-replica third, and the headline
>    8/24 partly conceals that.** The secrets fetch is one request among many
>    the agents send through the LB (30 s long-poll claims, heartbeats, log
>    appends, step reports), so the fetch's upstream is effectively arbitrary:
>    Part A's 24 fetches split **3 / 13 / 8** across controller1/2/3, not
>    8/8/8. **The exact, non-statistical result is the per-replica one** —
>    controller3 failed 8 of the 8 fetches it served and the other two failed
>    0 of 16 — and the fraction should be read as a consequence of that plus
>    the split, not as a measured "rate".
> 5. **A `docker compose up -d` recreate erases that container's own
>    `docker compose logs` history.** controller3's eight mixed-window
>    `secret decrypt failed` WARNs are in `w3-3/partA-fetchlog.txt` and
>    `w3-3/partB-controller.txt` because they were captured **before** Part D3
>    repaired the misconfiguration; after the recreate they are gone from
>    `docker compose logs controller3`. Capture before you repair.
> 6. **ADDED IN REVIEW — THE DOC SURVEY WAS TRUNCATED, AND IT COST THIS
>    SCENARIO ITS STRONGEST LIMB FOR A WHILE.** The capture backing the
>    key-identity entry's "the docs are silent" verdict
>    (`w3-3/docs-greps.txt`) was piped through `head` and stored **40** of the
>    first grep's **55** hits, and the truncation was never disclosed, so the
>    file read as exhaustive when it was not. Review caught it by re-running
>    the identical command and comparing line counts:
>    `grep -rn -iE 'same key|different key|key file|KEK|key id|key version|rotat' docs/*.md | wc -l`
>    returns **55**, against the 40 lines recorded. Hit **#55** — the last line
>    of the full output — is **`docs/secrets.md:420`**, the Troubleshooting
>    table's only HA row: "| `decrypt` errors in HA setup | Replicas were given
>    different key files, or different `UNIFIED_KMS_URI` values. **Give every
>    replica the identical key file (or the same KMS URI).** |" That is an
>    unconditional prescribed remedy **in `docs/`** for exactly the symptom
>    Part D3 produced; it was applied verbatim at `00:46:03.977Z` and the
>    symptom persisted at **100 % — 9/9 runs, decrypt WARNs on all three
>    replicas**, and the row's stated *cause* is false of the repaired cluster
>    besides. Per `FINDINGS.md:478-479` a documented contract is a violation
>    limb, so the key-identity entry is **now a violation, severity major**
>    (critical arguable; escalation left to the operator). `docs-greps.txt` has
>    been re-captured untruncated and now records its own hit counts; the
>    original is preserved as `w3-3/docs-greps-ORIGINAL-TRUNCATED.txt`.
>    **What is NOT withdrawn:** the refusal to read
>    `high-availability.md:218` / `operations.md:67` as an "only if" stands —
>    they are backup-and-recovery guidance, not an enumeration, and `:420` is a
>    different kind of statement (a prescribed fix, not a scope claim). So does
>    the checkpoint flag that the campaign's invariant set has **no
>    secret-store integrity clause** — I4 is logs/artifacts only. **Rule for
>    later scenarios: a survey supporting a "the docs are silent" verdict must
>    record its own line count and must never be piped through `head`.**
>
> **The scenario yields TWO observations and TWO violations** (corrected in
> review; this banner originally said three and one). The **major** violation
> is the key-identity / ineffective-documented-remedy entry, filed on the
> documented-contract limb (`docs/secrets.md:420`). The **minor I7** violation
> (a terminal run displaying a `Pending` step) is **reached by this scenario,
> not caused by it** — it is general to any run that terminates before its
> first step report. Do not merge it into the mixed-KEK entries.

**Wave W3, Task 2. Runs on today's `test/ha` rig plus one extra key file and one
per-replica env override — no object storage, so it is not blocked on Task 3.**

**Read this before designing anything: this scenario is a CONFORMANCE AND
BLAST-RADIUS MEASUREMENT, not a hunt for a contradicted contract.**
`docs/high-availability.md:204-206` states the rule explicitly and correctly —

> "Every replica must be given the **same** key. The controller no longer stores
> a key in the database, so replicas cannot converge on one by sharing a
> database — a replica started with a different key cannot read secrets written
> by another."

— and repeats it in the required-configuration table at `:225`
("**Same key on all replicas**") and in the HA checklist at `:544`. The
misconfiguration this runbook creates is therefore **documented, warned about,
and behaving as documented**. Anything filed as a violation has to be something
the docs do **not** promise. Two such candidates are named in the plan and each
gets its own Part below:

- **the absence of key identity in the ciphertext** (Part C) — a replica with
  the wrong key cannot tell that it has the wrong key, and nothing on the stored
  row records which key wrote it;
- **a failure mode reported more quietly than its siblings** (Part B) — the
  wrong-local-KEK case is the only one of three decrypt-failure classes that
  does not reach `slog.Error`.

**Expect observations.** Three of W2's nine scenarios produced only observations
and that was the right outcome each time. Do not manufacture a violation to make
the scenario feel productive. **Post-execution note (review):** this framing was
right about the *warned-about* limb — the mixed state behaves exactly as
`high-availability.md:204-206` says — and wrong about the docs as a whole. The
contradicted promise is not in the HA guide at all but in `docs/secrets.md:420`,
a Troubleshooting row prescribing a remedy that does not work once a secret has
been written during the mixed window. **Survey `docs/*.md` in full, and print
the hit count, before concluding "documented and behaving as documented".**

---

## Corrections to inherited facts, established BEFORE execution

Per the W1 carry-forward rule, a "verified facts" block is a set of claims to
check cheaply at the point of use. Three checks, three results.

- **CORRECTION 1 — the plan's citation for the KEK mount is imprecise, and the
  precise lines matter because the overlay has to break exactly one of them.**
  `docs/superpowers/plans/2026-07-30-edge-case-campaign-w3.md:80` says the key
  file is "mounted read-only into all three replicas at `/run/secrets/kek` via a
  **YAML anchor** (`docker-compose.ha.yaml:22-31`)". The span is roughly right
  but names neither the anchor nor the mount. The actual sites are:
  `controller1: &ctrl` at `docker-compose.ha.yaml:15` (the anchor, which is what
  the Task 2 brief cites and which is correct), the comment "All replicas must
  share the same key file; see docs/high-availability.md." at `:22`,
  `UNIFIED_CONTROLLER_KEY_FILE: /run/secrets/kek` at `:23`,
  `- ./kek:/run/secrets/kek:ro` at `:25`, and the two aliases
  `controller2: *ctrl` / `controller3: *ctrl` at `:30-31`. **The consequence is
  structural, not cosmetic**: a YAML alias cannot be partially overridden inside
  the file that defines it, which is precisely why the divergence has to be
  introduced from an overlay (§Stack) and why `test/ha/` is not edited.
- **CORRECTION 2 — `proxy_next_upstream` is at `test/ha/nginx.conf:24`, not
  `:23`.** The plan cites `:23`, which is the second line of the two-line
  comment above it (`:22-23`). The claim built on it is **unchanged and
  correct**: the directive reads
  `proxy_next_upstream error timeout http_502 http_503 http_504;` and **500 is
  not in the list**, so nginx will not retry a decrypt failure against a healthy
  replica. Recorded so the next reader does not conclude the fact is wrong when
  the line does not match.
- **CORRECTION 3 — "the wrong-key case is reported most quietly" is true of the
  LEVEL and false of the INFORMATION.** The plan and the brief both say the
  wrong local KEK "falls past both special cases … to a generic `slog.Warn`",
  and that is exactly right. But read all three branches of
  `logSecretDecryptFailure` (`internal/controller/api_secrets.go:161-173`)
  together: the two `slog.Error` branches log **`site` and `id` only** — they
  deliberately do not log the error — while the `slog.Warn` fall-through at
  `:172` logs `site`, `id` **and `"error", err`**. So the quietest branch by
  level is the *most* informative branch by content. **Both halves must appear
  in any finding**, or the entry overstates the diagnosability gap. Verify live
  (Part B) rather than resting on the read.

---

## Invariants

Quoted verbatim from `docs/superpowers/specs/2026-07-29-edge-case-testing-design.md`.

- **I1 (run accounting)** — `:48`:

  > "**Run accounting** — every API-accepted run reaches exactly one terminal
  > state; no phantom runs from duplicate fires/webhooks"

  **The expectation is that I1 HOLDS, and confirming that is a deliverable, not
  a formality.** The failing path is unusually well-behaved by construction:
  `client.FetchSecrets` is called once (`internal/agent/orchestrator.go:162`);
  on error the agent writes a System `stderr` line at `stepIndex: -1`
  (`:175-184`) and then calls `retryUntilSuccess(FinishRun(..., RunFailed))`
  (`:185-187`) and returns without running the DAG. So the run should reach
  `Failed` — exactly one terminal state — every time. **Two ways I1 could
  actually break here, and both are checked rather than assumed:** a run that
  never reaches a terminal state at all (the fetch error is swallowed and the
  claim is abandoned), and a run that is claimed twice because the first claim's
  failure left it re-claimable. Part A's per-run terminal-status census covers
  both.
- **I7 (state display consistency)** — `:54`:

  > "**State display consistency** — run status, approval status, and audit rows
  > never contradict each other or reality"

  **Also expected to hold, and this is the more interesting of the two**,
  because the plan calls this path "unusually well-instrumented": ~~the reason
  for the failure is written into the run's own logs, so `Failed` is not a bare
  status.~~ **SUPERSEDED BY BANNER CORRECTION 1 — the plan's premise is false
  and this sentence inherited it.** The run's own log carries exactly one line
  and it is `fetch secrets for run <id>: http 500: response omitted`
  (`internal/agent/client.go:107-108` destroys the body), so `Failed` **is** a
  bare status as far as the run's own logs are concerned. **Read the I7 question
  below against that, not against the plan's claim** — the finding it produces
  is a diagnosability gap (the union of every surface cannot name the cause),
  not a contradiction. The I7 question is therefore not "is the status right" but "does any
  surface state something that is not true". Part B enumerates every surface and
  answers it. **If a surface names a cause that is not the cause** — e.g. an
  error text that sends an operator looking for a missing secret or a corrupted
  ciphertext when the real fault is a key mismatch — that is an I7 concern and
  must be filed as one.
- **NOT I2.** I2 is "step side effects execute at most once" (`:49`). The DAG
  never runs at all on the failing path (`orchestrator.go:188`, `return` before
  `RunPipeline`), so the step body executes **zero** times. The W2 review
  correction applies directly and must not be re-litigated: **I2 is
  *at-most-once*, and a zero-versus-once shape does not violate it.** The
  `edge-secret-user` fixture deliberately writes no side-effect file so nothing
  invites the reading.
- **NOT I3.** No mutex, no semaphore, no `concurrency` block anywhere in the
  fixture; `mutex_holders` and `named_lock_slots` are never touched. I3 is not
  exercised, which is different from held — say "not exercised".
- **NOT I4.** I4 is "a Succeeded run's log line count matches what the workload
  emitted; no duplicates, no reordering; archives stay readable" (`:51`). Its
  scope clause is *"a Succeeded run's"*, and **no run in this scenario succeeds
  on the failing path** — the run fails before the DAG starts, so the workload
  emits nothing and there is no count to match. The one log line the failing run
  *does* carry (the `stepIndex: -1` System line) is evidence for Part B, not an
  I4 measurement. This rig also has no object store, so "archives stay readable"
  cannot be exercised at all. **Do not attribute anything here to I4.**
- **NOT I5.** I5 is "after fault injection the system returns to steady state
  within documented bounds … the bounds in `docs/high-availability.md` are the
  contract" (`:52`). **The reason this does not fit is worth stating precisely,
  because it is tempting:** a wrong key is not a fault the system is expected to
  recover *from* — it is a persistent misconfiguration, and there is no steady
  state to return to while it persists. I5's clock starts when a fault is
  cleared; here the "clearing" is an operator fixing the key file and restarting
  the replica, which is a deployment action, not a recovery bound. **Recording
  I5 as an explicit null result is part of the deliverable.**
- **NOT I6.** No zombie: nothing is fenced, no run is terminalised out from
  under a live agent, and the agent itself is the party that writes `Failed`.

**Contract limb — the docs are explicit and CORRECT, which is the opposite of
every previous scenario's contract limb.** W3-4 rested on "silent, not
contradicted"; W2-7 was corrected for citing a passage that *sanctioned* the
behaviour. Here the passage neither sanctions nor is silent — it **warns**, and
the observed behaviour is exactly what it warns about. Before filing anything on
a contract, re-read `docs/high-availability.md:200-232` in full and confirm the
candidate claim is genuinely outside what `:204-206` / `:225` / `:544` promise.
The two candidates that survive that test are named in the preamble.

---

## Verified mechanism — every row re-read at this branch's HEAD

| # | Fact | Site |
|---|---|---|
| 1 | Envelope encryption, two layers. A random 32-byte DEK encrypts the value with the `Binding`'s canonical encoding as AAD; the DEK is then wrapped by the `KeyManager`. **Both** blobs get a leading `CryptoVersion = 0x02` | `internal/secrets/crypto.go:30-50`, `withVersion` `:84-88`, const at `:14` |
| 2 | `LocalKeyManager` wraps the DEK as `"local:" + AES-256-GCM(kek, dek, nil)` — **key wrapping carries no AAD** | `internal/secrets/keymanager.go:60-66`, prefix const `:25` |
| 3 | **`LocalKeyManager` is `struct{ kek []byte }` — one field, no id, no version, no label** | `internal/secrets/keymanager.go:32-34` |
| 4 | So the stored `encrypted_dek` is `0x02` ‖ `"local:"` ‖ nonce ‖ ct+tag. **The only self-describing parts are a FORMAT version and a PROVIDER tag.** Neither identifies *which* local key | derived from 1+2 |
| 5 | ~~The `secrets` table has `id, name, scope, scope_ref, encrypted_dek, ciphertext, created_at, updated_at`~~ **CORRECTED BY EXECUTION (box, item 2): the live table has SIX columns — `id, name, encrypted_dek, ciphertext, created_at, updated_at`.** `scope`/`scope_ref` were dropped and the composite unique replaced by `secrets_name_key UNIQUE (name)`. Either way: **no key id, key version, key fingerprint or KMS-URI column** | `001_init.up.sql:247-256` **as amended by** `016_drop_secret_scope.up.sql:13-18`; live `\d public.secrets` in `w3-3/partC-ciphertext.txt` |
| 6 | A wrong local KEK fails in the **DEK-unwrap** layer: `km.DecryptKey` → the `local:` prefix matches → `aesGCMDecrypt(m.kek, …)` fails → wrapped as `decrypt dek: %w` | `keymanager.go:69-74`; `crypto.go:63-66` |
| 7 | **It is therefore NOT `ErrBindingMismatch`**, which is produced only at `crypto.go:79`, *after* a clean unwrap; and NOT `ErrProviderMismatch`, which needs a non-`local:` prefix (`keymanager.go:70-72`) | `crypto.go:73-80`, `keymanager.go:70-72` |
| 8 | `logSecretDecryptFailure` therefore falls past both special cases to `slog.Warn("secret decrypt failed", "site", …, "id", …, "error", err)` — **`Warn`, not `Error`, and the only branch of the three that logs the error text** | `internal/controller/api_secrets.go:161-173`, fall-through at `:172` |
| 9 | HTTP **500** with body `"decrypt " + name + ": " + err.Error()` | `api_secrets.go:140-143` |
| 10 | The agent calls `FetchSecrets` **once**, not wrapped in `retryUntilSuccess`, and only when `len(c.SecretsNeeded) > 0` | `internal/agent/orchestrator.go:161-162` |
| 11 | On error the agent writes one System `stderr` line at `stepIndex: -1` carrying `fetch secrets for run %s: %v`, then `retryUntilSuccess(FinishRun(…, RunFailed))`, then returns **without running the DAG**. ~~The reason is therefore visible in the run's own logs.~~ **STRUCK — see correction 1 in the box.** The `%v` is an `*HTTPError` whose `Body` field `Client.do` has already overwritten with the literal `"response omitted"`, so the line carries the *status* and **no reason** | `orchestrator.go:174-188`; `internal/agent/client.go:26-28`, `:107-108`, `:134` |
| 12 | `SecretsNeeded` is **not persisted** — the claim handler computes it, and the fetch handler independently recomputes the allowed set from the run's stored spec via the same `buildStages` | `api_agent.go:243-258`, `:263-275`, `collectSecretNames` `:465-474` |
| 13 | Startup does **no** key-consistency check of any kind: `KeySource.Resolve` reads the file, checks it is 64 hex chars, and returns; `main.go` logs `slog.Info("encryption key loaded", "source", resolved.Description)` where `Description` is the literal string `"key file " + path` | `internal/config/keysource.go:85-89`, `:192-217`; `cmd/controller/main.go:287-301` |
| 14 | `/readyz` checks `shuttingDown` and `store.Ping` only — **no secret round-trip, no key probe** | `internal/controller/server.go:318-333` |
| 15 | `GET /api/v1/secrets` returns `id`/`name`/`createdAt` only and **never attempts a decrypt**, so it cannot report a key mismatch | `api_secrets.go:48-61` |
| 16 | nginx round-robins with no affinity (`upstream controllers` with three plain `server` lines) and `proxy_next_upstream` **does not include `http_500`** | `test/ha/nginx.conf:3-9`, `:24` |
| 17 | nginx counts an "unsuccessful attempt" for `max_fails` only for `error`, `timeout`, `invalid_header` (always) plus whichever of `http_500/502/503/504/403/404/429` appear in `proxy_next_upstream`. **`http_500` is absent from `:24`, so a decrypt 500 does not count toward `max_fails=1` and cannot eject a controller** | nginx `ngx_http_upstream` documentation (doc-read); `test/ha/nginx.conf:6-8`, `:24` |
| 18 | The agents connect to the LB (`--server http://nginx:8080`, `docker-compose.ha.yaml:104-105`, `:125-126`), so **claim and secrets-fetch are two independent round-robin picks** — the replica that serves the fetch decides the run's fate, and it need not be the one that served the claim | `docker-compose.ha.yaml:97-137` |

**The single sentence the scenario tests:** with one replica holding a different
local KEK, roughly one in three secret-fetches is answered by a replica that
cannot unwrap the DEK, fails the run outright with no LB retry, and **has no way
to discover that its key is the wrong one** — because nothing in the ciphertext,
the row, the startup log or the readiness probe carries key identity.

---

## Stack

Plain `test/ha` for the baseline, then **plus one overlay**,
`compose/mixedkek.override.yaml`, which gives **controller3 alone** a different
key file. No product code, no change to `test/ha/`.

```bash
cd test/ha
export MSYS_NO_PATHCONV=1          # Git Bash rewrites container paths (W2-5)
export BASE="-f docker-compose.ha.yaml"
export MIXED="-f docker-compose.ha.yaml -f ../edgecase/compose/mixedkek.override.yaml"
docker compose $BASE up -d --build          # baseline: all three share test/ha/kek
# ... baseline gate ...
docker compose $MIXED up -d                 # recreates controller3 only
```

Throughout, `psql` means
`docker compose $MIXED exec -T postgres psql -U unified -tAc "<sql>"`, and `API`
means `curl -sS -H "Authorization: Bearer ha-admin-token"` against
`http://localhost:18080`.

**Why an env override and not a re-pointed mount.** The overlay adds a *second*
bind at a *new* target (`/run/secrets/kek-b`) and moves
`UNIFIED_CONTROLLER_KEY_FILE` to it, rather than re-pointing the existing
`/run/secrets/kek` mount. Compose does merge volume entries by target path — the
W3-4 overlay relies on exactly that — but a merge that silently fails to take is
the W2-5 failure shape, and here it would produce a stack that looks
misconfigured and is not. Adding a new target makes the divergence appear as two
distinct lines in `docker compose config`, and leaves the original key file
mounted so **both** files can be read from inside controller3 and diffed. That
diff is gate G3.

**Workload — one new fixture.** `workloads/secret-user.payload.json`, job
`edge-secret-user`, modelled on the known-good `tick.payload.json` (same
`apiVersion`/`kind`/`native`/`agentSelector`):

```yaml
steps:
  - name: usesecret
    env:
      EDGE_KEK_PROBE: "{{ .Secrets.EDGE_KEK_PROBE }}"
    run: |
      echo secret-user-begin
      if [ -z "$EDGE_KEK_PROBE" ]; then echo "secret is empty" >&2; exit 1; fi
      echo "secret-len=${#EDGE_KEK_PROBE}"
      echo secret-user-end
```

Rationale, point by point:

- **The secret reference is in `env:`, which is one of the two places
  `buildStages` scans** (`api_agent.go:335-341`), so `SecretsNeeded` is non-empty
  and fact 10's gate opens. Verified offline through the real `dsl.Parse`
  (`KnownFields(true)` + `Validate()`) **and** through `dsl.ReferencedSecretNames`
  — the exact call `collectSecretNames` makes (`api_agent.go:466`) — giving
  `len(SecretsNeeded)=1 names=[EDGE_KEK_PROBE]`, `RequiredCaps=[native]`.
  Capture: `$SCRATCH/fixture-dslparse.txt`. **W1 shipped two payloads that 400'd
  because a key path was wrong; this one is checked before the stack exists.**
- **The name is `EDGE_KEK_PROBE`, all underscores, on purpose.** Hyphenated
  secret names do work — `normalizeSecretsRefs` rewrites `{{ .Secrets.a-b }}` to
  `{{ index .Secrets "a-b" }}` (`internal/dsl/template.go:39-68`) — but an
  underscore name goes through the plain dot path with no rewriting, which is
  one fewer mechanism between the fixture and the thing being measured.
- **The step prints the secret's LENGTH, never its value.** `secrets.NewMasker`
  would mask a printed value anyway (`orchestrator.go:196`), but a fixture whose
  correctness depends on the masker is a fixture that cannot be used to test the
  masker. `${#VAR}` is POSIX and safe under `/bin/sh`.
- **It is fast and has no sleeps** — Part A needs N ≥ 20 runs, and the whole
  point is the fetch, not the body.
- **No side-effect file, no mutex**, deliberately: nothing must invite an I2 or
  I3 reading (§Invariants).

---

## BASELINE GATE — do not proceed past a failing check

Write every gate output to `$SCRATCH/gate.txt` unless a per-check file is named.

```bash
SCRATCH="<scratchpad>/w3-3" ; mkdir -p "$SCRATCH"
```

- **G0 — worktree.** `git rev-parse --show-toplevel` is `.../wt-edge-spec`,
  branch `plan/edge-case-w3`. `docker compose ls` shows the developer stack
  (project `unified-cd`) untouched; `test/ha`'s project is `unified-cd-ha`.
  **STOP** if the toplevel is the main checkout.
- **G1 — stack health, BEFORE the overlay.** All three controllers up;
  `API /readyz` → 200; `GET /api/v1/agents` lists **agent1 and agent2**, both
  connected, and record their **labels** (`kind:linux` is what
  `edge-secret-user` selects on). → `$SCRATCH/gate-g1-agents.txt`.
- **G2 — the job applies and the secret registers, both through the LB.**
  1. `API -X POST -H 'Content-Type: application/json' --data @workloads/secret-user.payload.json /api/v1/jobs` → **200**.
  2. `API -X POST -H 'Content-Type: application/json' -d '{"name":"EDGE_KEK_PROBE","value":"<value>"}' /api/v1/secrets/` → **204**.
     The route is `POST /api/v1/secrets/` with `requireMinRole("admin")`
     (`internal/controller/server.go:385-386`), body
     `api.SetSecretRequest{Name, Value}` (`internal/api/types.go:256-259`) — and
     the **trailing slash is load-bearing**: the sub-router registers `"/"`, not
     `""`. Record the exact URL that worked. `204 No Content` is success
     (`api_secrets.go:44`); there is no body.
  3. `API /api/v1/secrets/` → the name appears. **Note for Part C that this
     read never decrypts anything** (fact 15).
  A fixture 400ing is a branch-internal asset bug (W1-4 precedent) and must be
  fixed and recorded before any measurement. → `$SCRATCH/gate-g2-apply.txt`.
- **G3 — THE BASELINE READ-BACK, which is the check the whole scenario rests
  on.** With all three replicas still sharing `test/ha/kek`, trigger
  `edge-secret-user` **at least 6 times** and require **every one** to reach
  `Succeeded` with `secret-len=<expected>` in its logs. Six is the minimum that
  makes it improbable (≈1.4%) that a whole replica was never exercised by pure
  round-robin luck; **attribute the fetches to replicas from the controller
  access logs anyway rather than resting on the probability** — if all three
  replicas served at least one `POST /api/v1/agents/*/secrets/fetch` with
  `status 200`, the baseline is proved per-replica and the probability argument
  is not needed. → `$SCRATCH/gate-g3-baseline.txt`.
  **STOP on any failure.** A baseline that cannot read its own secret back means
  the scenario is measuring something else entirely, and every number below
  would be attributed to the wrong cause.
- **G4 — apply the overlay and confirm controller3 STARTED.** `docker compose
  $MIXED up -d`, then:
  1. `docker compose $MIXED ps` — controller3 `running`, and **not restarting**.
     Watch it for at least 30 s. **A wrong key is still a valid key file**
     (64 hex chars after `TrimSpace`, `keysource.go:208-211`), so it must not
     crashloop; if it does, the overlay produced a malformed file rather than a
     wrong key and nothing below is testing what it claims to.
  2. `docker compose $MIXED exec -T controller3 sh -c 'cmp -s /run/secrets/kek /run/secrets/kek-b; echo "same=$?"; wc -c < /run/secrets/kek-b'`
     — the two files must **differ**, and `kek-b` must be 64 or 65 bytes
     (a trailing newline is stripped by `TrimSpace`). **Check this from inside
     the container, not on the host**: `kek-b` is committed to git, and a
     CRLF-converting checkout would give 66 bytes on the host while `TrimSpace`
     still yields 64 — the container is where the answer counts.
  3. `docker compose $MIXED logs controller3 | grep "encryption key loaded"` —
     record the line. **Record the SAME line from controller1 and controller2**
     (`$SCRATCH/gate-g4-startup.txt`). Fact 13 predicts the two lines differ
     only by the file *path*, and if the overlay had re-pointed the mount
     instead of adding one they would be **byte-identical**. That is Part C's
     material, gathered here because it is free.
  → `$SCRATCH/gate-g4-overlay.txt`.
- **G5 — the LB still fronts three replicas.** After the recreate, confirm
  `/readyz` through the LB is 200 and that controller3 is still in rotation
  (it must be — a decrypt 500 is not an nginx failure, fact 17). **STOP** if
  controller3 has been ejected before any measurement starts, because then
  Part A would measure an ejected upstream rather than a wrong key.
- **G6 — API 500s.** The API on this rig has been intermittently returning 500s.
  Record, for every gate and every trigger, how many attempts it took.
  **This gate needs a sharper rule than W3-4's, because in this scenario a 500
  is the expected result**: a 500 from `POST /api/v1/runs` (the trigger) or from
  any gate command is rig flakiness and invalidates that attempt; a 500 from
  `POST /api/v1/agents/*/secrets/fetch` is the measurement. They are told apart
  by the path in the controller access log, never by the status alone.
- **G7 — Postgres statement logging is NOT armed for this scenario, and that is
  a decision, not an omission.** Nothing here needs statement-level evidence:
  Part C reads the `secrets` row directly with a single `psql` query, and the
  failure is entirely in the controller's own logs. If a re-run does arm it:
  **one `ALTER SYSTEM` per `psql -c`** (two in one is an implicit transaction,
  refused **silently** while `pg_reload_conf()` still returns `t` — W2-7), and
  verify `log_statement` **and** `log_line_prefix` in a **fresh** session on both
  arm and revert.

---

## Part A — the blast radius, measured per request

**Deliverable:** a table of **N ≥ 20** `edge-secret-user` runs, each with its
terminal status **and** the replica that served its `secrets/fetch`, taken from
the controllers' own access logs; the measured failure fraction with its
numerator and denominator; and an explicit statement that every run reached
exactly one terminal state (I1).

- **A1 — trigger N ≥ 20 runs.** `API -X POST -d '{"jobName":"edge-secret-user"}'
  /api/v1/runs` (`internal/controller/server.go:370` → `handleTriggerRun`;
  **there is no `/api/v1/jobs/<job>/trigger` route** — README). Record every run
  id and the host clock at trigger. Space them enough that each run's fetch is
  identifiable; do not fire them all in one instant, because two fetches in the
  same millisecond are harder to attribute and attribution is the point.
  → `$SCRATCH/partA-triggers.txt`.
- **A2 — wait for every run to reach a terminal state, then census.**
  ```sql
  SELECT id, status, created_at, updated_at FROM runs
   WHERE job_name = 'edge-secret-user' ORDER BY created_at;
  SELECT status, count(*) FROM runs WHERE job_name='edge-secret-user' GROUP BY status;
  ```
  → `$SCRATCH/partA-runs.txt`. **Every run must be terminal and each must be
  terminal exactly once — that is the I1 check** (fact 11 predicts `Failed`
  without the DAG running). A run stuck non-terminal is a *finding*, not a
  measurement problem: record it and say so.
- **A3 — attribute each run to a replica, per request, from a capture.** For
  every controller, extract the access-log lines for the fetch path:
  ```bash
  for c in controller1 controller2 controller3; do
    docker compose $MIXED logs --no-color --timestamps $c \
      | grep '"path":"/api/v1/agents/.*/secrets/fetch"'
  done
  ```
  Each line carries `method`, `path`, `status` and `duration_ms`
  (`internal/controller/server.go:180-186`) and `docker compose logs` prefixes
  the container name, so **each individual fetch is classified by replica and
  status**. → `$SCRATCH/partA-fetchlog.txt`.
  **This is the bracket. Do not substitute a wall-clock argument for it** —
  Task 1 was corrected for calling a wall-clock attribution a bracket. If a
  particular run's fetch line is missing from the capture, that run's
  attribution is **unbracketed** and must be labelled
  *(observed live, raw output not captured to scratchpad)* or excluded from the
  fraction, with the choice stated.
  - **Joining a fetch line to a run id.** The access log carries the *path*,
    which contains the agent id but not the run id (the run id is in the request
    body). Two joins are available and at least one must be used and named:
    (i) the failing replica's own `slog.Warn("secret decrypt failed", …)` line,
    which is one-per-failure and timestamp-adjacent; (ii) the run's own
    `stepIndex: -1` System log line, whose `ts` brackets its fetch. State which
    join was used for each run and do not silently mix them.
- **A4 — the fraction.** Report `failures / N` with both numbers, and report the
  per-replica split. **The expectation is ~1/3 and it is a prediction to test,
  not an assumption to state**: with round-robin, no affinity, and 500 excluded
  from `proxy_next_upstream` (fact 16), one replica in three should fail every
  secret-using run it serves. Report the exact count and note that binomial
  spread at N = 20 is wide (a true 1/3 gives a 95% interval of roughly 3-11
  failures), so a measurement of, say, 5/20 **corroborates** 1/3 rather than
  contradicting it. Do not present the point estimate as a precise rate.
- **A5 — the `max_fails` contamination check the brief requires.** Fact 17 says
  a 500 is not an nginx "unsuccessful attempt" because `http_500` is absent from
  `proxy_next_upstream` (`nginx.conf:24`), so controller3 should **never** be
  ejected and the three-at-once ejection W3-4 hit should **not** occur here.
  **Verify rather than assume**: grep the whole session's nginx log for
  `no live upstreams` and `upstream server temporarily disabled`, and confirm
  the per-replica split in A3 shows controller3 still receiving traffic
  throughout the window. → `$SCRATCH/partA-nginx.txt`.
  **If ejection DID occur, the fraction is contaminated and must be re-measured**
  — say so and re-run rather than adjusting the number.

**Falsification.** If the failure fraction is **0/N**, the overlay did not take
(re-check G4) or the secret is being served from somewhere other than the row
written at baseline. If it is **N/N**, the secret was written *after* the
overlay by controller3 and every other replica is now the one that cannot read
it — check `secrets.updated_at` against the overlay's apply time before
concluding anything. If it is **~2/3**, the same inversion happened partially.
**Each of these is a real possible state of the world and each has a different
finding attached; do not smooth any of them into "about a third".**

---

## Part B — what the operator actually sees, and the asymmetry

**Deliverable:** the complete operator-visible chain for **one** named failure —
the HTTP status and body, the controller log line with its level, and the run's
own `stepIndex: -1` System `stderr` line — plus a verified statement of how the
wrong-key case compares with its two siblings on **both** level and content
(CORRECTION 3).

- **B1 — the run's own logs, which is the well-instrumented part.**
  ```sql
  SELECT step_index, stream, ts, line FROM logs
   WHERE run_id = '<failed run>' ORDER BY seq;
  ```
  Expect exactly one row: `step_index = -1`, `stream = 'stderr'`, line
  `fetch secrets for run <id>: <error>` (`orchestrator.go:175-184`). **Record
  the full line verbatim** — it is the only place the operator meets the cause
  without reading controller logs. Also read it through the API
  (`GET /api/v1/runs/{id}/logs`) so the write-up can say what a *user* sees, not
  only what the DB holds. → `$SCRATCH/partB-runlogs.txt`.
- **B2 — the error text, end to end.** The plan lists "the exact stdlib error
  text after `decrypt <name>: decrypt dek:`" as **not established**, expecting
  `cipher: message authentication failed`. **Settle it and record it**, because
  the whole diagnosability argument turns on what that string tells an operator.
  ~~It reaches the agent as `client.FetchSecrets`'s error and is embedded in B1's
  line, so B1 already contains it; quote it from there.~~ **SUPERSEDED — THIS
  STEP AS WRITTEN IS IMPOSSIBLE, AND THE REASON IS BANNER CORRECTION 1.** B1's
  line does **not** contain it: `Client.do` replaces the body of every response
  ≥ 400 with the literal `"response omitted"` (`internal/agent/client.go:107-108`),
  so B1's line is `fetch secrets for run <id>: http 500: response omitted` and
  carries no error text at all. **The HTTP 500 body cannot be captured on the
  wire either** — the endpoint is agent-authenticated and run-guarded, and the
  only client that reaches it discards the body. **Do instead: take the text
  from B3's controller-side `slog.Warn`, which logs `err.Error()` verbatim
  (`decrypt dek: cipher: message authentication failed`), and DERIVE the body by
  composing it with the handler's own `"decrypt "+name+": "` prefix
  (`internal/controller/api_secrets.go:142`). Label the result *derived*, name
  both captured inputs, and do not present it as observed** — that the string is
  unobservable from the agent side is part of the finding, not a gap in the
  measurement (`FINDINGS.md:1599`). Note the refinement B3 settles: `err.Error()`
  itself carries **no** secret name; the name in the body comes from the
  handler's prefix.
- **B3 — the controller side.** Find the `secret decrypt failed` line on
  controller3 and record its **level**, its keys, and the surrounding access-log
  line:
  ```bash
  docker compose $MIXED logs --no-color --timestamps controller3 \
    | grep -E 'secret decrypt|"path":"/api/v1/agents/.*/secrets/fetch"'
  ```
  → `$SCRATCH/partB-controller.txt`. Expect
  `{"level":"WARN","msg":"secret decrypt failed","site":"agent-fetch","id":"EDGE_KEK_PROBE","error":"decrypt EDGE_KEK_PROBE: decrypt dek: …"}`
  immediately before an access line with `"status":500`.
- **B4 — the asymmetry, verified rather than asserted.** All three branches of
  `logSecretDecryptFailure` (`api_secrets.go:161-173`) are reachable, and the
  claim is about how they compare:

  | Case | Level | Keys logged | Diagnostic sentence |
  |---|---|---|---|
  | binding mismatch (`:162-165`) | `Error` | `site`, `id` | yes — "possible ciphertext tampering or substitution" |
  | provider mismatch (`:167-170`) | `Error` | `site`, `id` | yes — "check UNIFIED_KMS_URI / UNIFIED_CONTROLLER_KEY_FILE" |
  | **wrong local KEK (`:172`)** | **`Warn`** | `site`, `id`, **`error`** | no |

  **Confirm the wrong-key row live** (B3 gives it). The other two rows are
  **code-read** — producing them would need a second key *provider* or a
  tampered ciphertext, neither of which this scenario builds — and must be
  labelled code-read in the write-up. **State both halves of the comparison**:
  the wrong-key case is the only one that does not reach `Error`, *and* it is
  the only one that carries the error text. An entry that reports only the first
  half overstates the gap; one that reports only the second half misses the
  finding. **The sharpest framing available, and the one to check:** the branch
  whose message names the fix (`check UNIFIED_KMS_URI /
  UNIFIED_CONTROLLER_KEY_FILE`) is the one that does **not** fire for the most
  likely way to get a wrong `UNIFIED_CONTROLLER_KEY_FILE`.
- **B5 — every other surface, enumerated so "silent" can be scoped honestly.**
  Record each, and record what it does *not* say:
  `GET /api/v1/runs/{id}` (status), `GET /api/v1/runs/{id}/steps` (expect
  **empty** — the DAG never ran, so there is no step report at all, which is
  itself worth stating), `GET /api/v1/secrets/` (names only, no decrypt — fact
  15), `/readyz` on controller3 directly (expect **200** — fact 14), and the
  controller3 startup line from G4. → `$SCRATCH/partB-surfaces.txt`.
  **Say explicitly whether the WebUI was opened.** If it was not, say so rather
  than writing "every surface" (the W2-9 lesson).

**Falsification.** If the controller3 line is `Error` rather than `Warn`, or if
it carries a diagnostic sentence, CORRECTION 3 and fact 8 are wrong and **that
is the finding** — record the contradiction, quote the captured line, and do not
reshape the entry around the prediction.

---

## Part C — the key-identity question: the entry's strongest limb

**Deliverable:** the actual bytes of a stored `secrets` row's `encrypted_dek`
prefix, read from the database, showing `0x02` then the ASCII `local:` and
**nothing that identifies a key**; plus the enumeration of every other place a
key identity could have been recorded and was not.

**Read the row. Do not argue this from the struct alone** — fact 3 is a code
read and a reviewer is entitled to ask whether something else adds an identity
downstream.

- **C1 — the prefix bytes.**
  ```sql
  SELECT name,
         length(encrypted_dek)                        AS dek_len,
         encode(substring(encrypted_dek from 1 for 8), 'hex')   AS dek_prefix_hex,
         encode(substring(encrypted_dek from 1 for 8), 'escape') AS dek_prefix_esc,
         length(ciphertext)                           AS ct_len,
         encode(substring(ciphertext from 1 for 8), 'hex')      AS ct_prefix_hex
    FROM secrets WHERE name = 'EDGE_KEK_PROBE';
  ```
  → `$SCRATCH/partC-ciphertext.txt`. **Expected and to be confirmed byte by
  byte:** `dek_prefix_hex` begins `02` followed by `6c6f63616c3a` — which is
  ASCII `local:` — i.e. the version byte of fact 1 and the provider tag of fact
  2, and then immediately the GCM nonce. `ct_prefix_hex` begins `02` and then
  goes straight to the value ciphertext's nonce, with **no** provider tag at all
  (only the DEK is wrapped by the `KeyManager`). Read the lengths too: a
  `dek_len` of 6 + 1 + 12 + 32 + 16 = 67 accounts for every byte with none left
  over for an identifier, and **saying which byte would have to be the key id and
  showing there is no room for it is stronger than saying it is absent.**
- **C2 — the row's other columns.** `\d secrets` and a full row read. Confirm
  against fact 5 that there is no key-id, key-version, fingerprint or KMS-URI
  column — **and confirm the same for the row that is actually stored**, not
  only for the migration. → `$SCRATCH/partC-schema.txt`.
- **C3 — the negative test that makes C1 mean something.** A missing identifier
  is only a finding if its absence has a consequence. Show that the wrong-key
  replica **cannot** self-diagnose:
  - it reaches `aesGCMDecrypt` and gets an authentication failure that is
    indistinguishable from a corrupted blob (fact 6, and `crypto.go:20-24`'s own
    comment says GCM "cannot say which of them was wrong");
  - the provider tag **matched**, so `ErrProviderMismatch` — the one error that
    names the key configuration — is unreachable (fact 7);
  - nothing at startup compares the key against anything (fact 13), and the
    startup log lines from G4 prove it: the *only* difference between the good
    and bad replicas' `encryption key loaded` lines is the file path, and had
    the overlay re-pointed the mount instead of adding one they would be
    identical;
  - `/readyz` is 200 (fact 14) and the secrets list API never decrypts (fact 15),
    so **no health surface anywhere exercises the key**.
  → `$SCRATCH/partC-selfdiag.txt`.
- **C4 — the counterfactual, stated because it is what a fix looks like.** The
  codebase already knows the pattern: `localKeyPrefix`'s own comment
  (`keymanager.go:21-24`) says Vault Transit "self-describes its ciphertext as
  `vault:v1:…`" and that matching that convention "lets a provider mismatch be
  reported precisely instead of surfacing as an opaque AES-GCM authentication
  failure". **The local provider adopted the self-describing prefix but not the
  version/key-identity part of it.** That is the argument in one sentence, and
  it is made from the code's own comment rather than from an external standard.
- **C5 — judge honestly.** The docs promise that a wrong-key replica cannot
  *read* the secrets (`:204-206`), and it cannot. They do **not** promise that
  it can *detect* that it has the wrong key. Whether that absence is a
  violation or an observation is the judgement call this scenario exists to
  make. **Decide it in the write-up with the reasoning stated**, and remember
  the campaign's rule: a violation contradicts an invariant (I1-I7) or a
  documented contract, and "the docs are silent" is not by itself either.

---

## Part D — the write-side inversion, which is the consequence with teeth

**Deliverable:** a second secret, written **through the LB after the overlay is
applied**, with the replica that encrypted it identified from a capture; then a
demonstration of which replicas can and cannot read it.

This part is cheap and it is where Part C's missing identity stops being a
tidiness complaint. Part A measures a **read**-side blast radius against a
secret written when the cluster was consistent. But a mixed-KEK cluster is also
a **write**-side hazard: `handleSetSecret` encrypts with whichever replica
happens to answer (`api_secrets.go:34-43`), returns `204`, and stores the blob
with **no record of which key wrote it** (Part C). So a secret written while the
cluster is mixed is a coin flip whose outcome nothing reports.

- **D1 — write a second secret through the LB, repeatedly, and watch where it
  lands.** `POST /api/v1/secrets/` with a distinct name (e.g.
  `EDGE_KEK_PROBE_B`), then immediately identify the writing replica from the
  access log for `"path":"/api/v1/secrets/"` with `"status":204`. Repeat until
  at least one write has been served by controller3 and at least one by a
  good replica — **and cap the attempts**, reporting the count either way.
  → `$SCRATCH/partD-writes.txt`.
- **D2 — read each back.** Point a job at each name in turn (or, cheaper, read
  the same secret through several runs and attribute the fetch as in A3) and
  record which replicas succeed. **The prediction:** a secret written by
  controller3 is readable *only* by controller3 — a ~2/3 failure rate, the
  mirror image of Part A's ~1/3 — and **nothing anywhere distinguishes the two
  secrets**. Both are rows in the same table with the same `0x02local:` prefix.
- **D3 — the consequence, stated plainly.** Fixing the misconfiguration (giving
  controller3 the right key and restarting it) makes every secret controller3
  wrote **permanently unreadable by every replica**, with no error at write
  time, no marker on the row, and no way to enumerate which rows are affected.
  **Say whether this was demonstrated or is code-read**: demonstrating it needs
  only removing the overlay and re-reading `EDGE_KEK_PROBE_B`, which is one
  command — **do it, and if it is not done, say so explicitly rather than
  asserting the consequence.**
  - **SHARPENED AT BRANCH REVIEW — write it as the CONVERSION, not as the end
    state, because D2 and D3 together measure something stronger than either
    alone.** D2 establishes the blob is **readable** (as executed: controller3
    6/6 Succeeded on it). D3 establishes that after applying
    `docs/secrets.md:420` verbatim, 9/9 fail on all three. **The documented
    remedy converts a recoverable state into permanent loss** — that is the
    sentence the write-up owes, and an earlier version established both halves
    and stated neither. The mixed state is the state in which the data is still
    there; the repair is the step that removes it.
  - **AND CARRY THE QUALIFICATION, because it is what the doc fix turns on:**
    "permanent" holds only once the **wrong** key file is discarded. The blob is
    wrapped under `kek-b`, and `compose/kek-b` is a file that still exists in
    this repository — so this scenario's own secret is recoverable by re-mounting
    it. Record that. It is the reason the recommended doc fix is an *ordering*
    fix (retrieve or re-`secret set` the affected values **before** removing the
    wrong key, or retain the wrong key until you have) and not only a wording
    one, and the ordering fix depends on no code change at all.
- **D4 — scope the claim.** This is a consequence of operating in a state the
  docs tell you not to operate in. It is **not** a case of the docs being wrong.
  The candidate finding is the *undetectability*, not the misconfiguration.

---

## Part E — the `docs/*.md` survey (ADDED AT BRANCH REVIEW — MANDATORY, NOT OPTIONAL)

**This Part did not exist when the scenario was executed, and its absence is the
single largest re-runnability defect in this runbook.** The survey was run
ad hoc, piped through `head`, and truncated at 40 of 55 hits (banner correction
6) — and **hit #55 was `docs/secrets.md:420`, which is the whole difference
between three observations and this wave's second major violation**. A
re-runner following Parts A-D as previously written would reproduce every
measurement and reach the *wrong classification*, because nothing told them to
look. The instruction existed only as prose at §"Expect observations"
(`:137-138`); it is now a step.

- **E1 — run the survey in full and print the hit count with the output.** No
  `head`, no `tail`, no `| less`. If the output is long, that is the point.

  ```bash
  for pat in 'same key|different key|key file|KEK|key id|key version|rotat' \
             'decrypt|encrypt|cipher|master key' \
             'HA|replica|high.availability'; do
    printf '=== %s ===\n' "$pat"
    grep -rn -iE "$pat" docs/*.md | tee /dev/stderr | wc -l
  done
  ```
  → `$SCRATCH/docs-greps.txt`. **The rule this pays for, from `FINDINGS.md:2101`:
  a docs survey that is truncated is not a survey, and a truncation that is not
  disclosed is a spec-compliance failure.** If you must bound the output, say so
  in the capture and archive the untruncated original beside it.
- **E2 — read the Troubleshooting tables, not only the grep hits.**
  `docs/secrets.md`'s Troubleshooting table and `docs/high-availability.md`'s
  checklist are prose tables; a keyword grep reaches a row's *symptom* column and
  can miss its *remedy* column, which is where `:420` hides. Open both.
- **E3 — check every candidate remedy against what Parts C and D measured, not
  against what it says.** `docs/secrets.md:420`'s sole HA row prescribes "Give
  every replica the identical key file" for "`decrypt` errors in HA setup".
  **Applied verbatim after Part D it left 9 of 9 runs failing on all three
  replicas** — see `FINDINGS.md:1620`. A remedy that is correct as *prevention*
  and destructive as *repair* is a contradicted contract, and this is the shape
  to look for: the docs are right about the state and wrong about the exit.
- **E4 — grep `FINDINGS.md` for the finding, not only `docs/` for the passage.**
  The already-ruled check must cover both. `grep -rn 'secrets.md:4' test/edgecase/`
  and `grep -rn -i 'identical key file' test/edgecase/` before filing.

---

## Teardown

**Steps 1 and 2 were prose comments with no commands until the branch review;
they are commands now, because a teardown a re-runner has to reinvent is a
teardown that gets skipped.**

```bash
# 1. cancel any surviving run (idempotent; 204 expected, 409 if already terminal)
for id in $(API "/api/v1/runs?jobName=edge-secret-user" \
              | python -c 'import sys,json; [print(r["id"]) for r in json.load(sys.stdin) if r["status"] in ("Pending","Queued","Running")]'); do
  API -X POST "/api/v1/runs/$id/cancel" -o /dev/null -w "cancel $id -> %{http_code}\n"
done
psql "SELECT id,status FROM runs WHERE status NOT IN ('Succeeded','Failed','Cancelled');" \
  | tee -a "$SCRATCH/teardown.txt"   # must be empty

# 2. kill every background sampler and CAPTURE that, on two passes
{ echo "=== sampler pass 1 $(date -u +%FT%TZ) ==="
  while read -r p; do kill "$p" 2>/dev/null; done < "$SCRATCH/samplers.pid"
  jobs
  ps -W | grep -iE "curl|psql|python" || echo "no host samplers"
  docker compose $MIXED exec -T controller3 sh -c 'ps -o pid,args' || true
} >> "$SCRATCH/teardown.txt" 2>&1

# 3. down  (pass 2 of the sampler check goes immediately before this line)
docker compose $MIXED down -v
```

- **Kill every background sampler and *capture* that, do not assert it.** Keep
  PIDs in `$SCRATCH/samplers.pid`, `kill` them explicitly, then show `jobs`
  empty and `ps -W | grep -iE "curl|psql|python"` matching nothing, on **two**
  passes: one before teardown and one immediately before `down -v`.
  → `$SCRATCH/teardown.txt`. **Check inside the containers too** — W3-4 found a
  `docker compose exec` sampler that outlived the shell that launched it and
  appeared in neither the host's `jobs` nor its `ps`. Always pass `-m <seconds>`
  to a `curl -N` sampler.
- **`down -v`, not `down`.** The `agent-credentials` volume must go, or the next
  scenario inherits enrolled agents.
- Copy `$SCRATCH` into the campaign evidence root at the wave checkpoint
  (`test/edgecase/README.md` § "Raw evidence").

---

## Recording rules

- **The default expectation is OBSERVATIONS, and that is a successful outcome.**
  The docs are explicit and correct (`docs/high-availability.md:204-206`, `:225`,
  `:544`) and this runbook creates exactly the state they warn about. Three of
  W2's nine scenarios produced only observations. **Do not manufacture a
  violation to make the scenario feel productive.**
- **A violation here must be something the docs do not promise.** The two
  candidates are Part C (no key identity anywhere — ciphertext, row, startup log
  or health surface — so a wrong-key replica cannot self-diagnose) and Part B
  (the wrong-key case is the only one of three decrypt-failure classes that does
  not reach `Error`, and the branch that names the fix is the one that does not
  fire). **Judge each on its own**, quote the invariant verbatim if one is
  claimed, and if the docs are silent say "silent, not contradicted" and rest on
  the invariant — or file it as an observation.
- **Report the measured fraction with its numerator, denominator and per-replica
  split**, and state the binomial spread rather than presenting a point estimate
  as a rate. N ≥ 20.
- **Every attribution must be per-request from a capture.** Task 1 was corrected
  for calling a wall-clock attribution a "bracket". If a run's fetch line was
  not captured, label it *(observed live, raw output not captured to
  scratchpad)* or exclude it, and say which.
- **Label every figure**: measured, derived, code-read, or doc-read. The two
  unreached branches of `logSecretDecryptFailure` are **code-read**. nginx's
  `max_fails` accounting rule is **doc-read** (nginx's own documentation) plus a
  live absence check (A5).
- **Never write "never" for a window this runbook closed itself.** The mixed
  state exists only between G4 and teardown, by construction.
- Entry titles must say **"observation"** for observation entries
  (`FINDINGS.md:481`) and repeat it in the Severity line as `minor
  (observation)`. A defect in this campaign's own assets gets an explicit
  `Classification:` line and sits outside both tallies (`FINDINGS.md:487`).
- **Cross-references, not re-filings.** The nearest already-filed items are the
  W2-7 credential-file entry (a *local reader of a credential file* can act as
  the agent) and the memory item `unified-cd dev ephemeral-key gotcha` (the dev
  controller regenerates an ephemeral key on every restart, destroying all
  secrets — `keysource.go:90-100`, warned at `:98-99`). **Neither is this**:
  W2-7 is about agent credentials, not the KEK; the dev-mode gotcha is a
  single-replica temporal mismatch that the code warns about loudly at startup,
  whereas this is a multi-replica spatial mismatch that produces **no startup
  warning at all** (fact 13). Say so explicitly so triage does not merge them.

---

## Execution notes — 2026-07-31 run (read before re-running)

Executed against `test/ha` + `compose/mixedkek.override.yaml` on branch
`plan/edge-case-w3`, **`00:36:31Z – 00:50:22Z`**. **Four `FINDINGS` entries: 2
observations (minor) and 2 violations — 1 major on the documented-contract
limb (`docs/secrets.md:420`, reclassified in review, see banner correction 6)
and 1 minor I7.** No branch-internal asset
bug — the fixture applied `200` and the secret registered `204` on the **first**
attempt each, and **not one API 500 was seen on any trigger or gate command**
across 51 run triggers (gate G6's flakiness allowance was never spent). The
developer stack (`docker compose ls` project `unified-cd`, 7 containers) was
untouched before and after (`w3-3/gate.txt`, `w3-3/teardown.txt`).

**Four phases, 51 runs, every one terminal exactly once** (`w3-3/consolidated.txt`):

| phase | key state | secret last written by | runs | Failed | measured |
|---|---|---|---|---|---|
| 1 — baseline | all three share `test/ha/kek` | controller1 (`00:36:53.155`) | 6 | 0 | 6/6 `Succeeded`, `secret-len=27`, and **all three replicas served a `200` fetch** |
| 2 — Part A | controller3 on `kek-b` | controller1 | 24 | **8** | **8/24 = 33.3 %**; controller3 8/8 failed, controller1+2 0/16 |
| 3 — Part D2 | controller3 on `kek-b` | **controller3** (`00:44:16.402`) | 12 | **6** | polarity **inverted**: controller1 0/5, controller2 0/1, **controller3 6/6 succeeded** |
| 4 — Part D3 | repaired: all three share `test/ha/kek` | controller3 | 9 | **9** | **9/9 failed on every replica** — the repair does not undo the damage |

**Every one of the 45 secret-fetches in phases 2-4 is bracketed per request**
from the controllers' own access logs (`internal/controller/server.go:180-186`),
which carry `method`, `path` and `status`, with `docker compose logs` supplying
the replica. 24 fetch lines for 24 Part A runs, 12 for 12 in D2, 9 for 9 in D3 —
**one fetch per run, no retries**, which is fact 10 confirmed live.
**No claim in this scenario rests on a wall-clock attribution.**

**Ten things a re-run should know.**

1. **Capture before you repair.** Part D3's `up -d --force-recreate controller3`
   erased controller3's own `docker compose logs`, taking its eight mixed-window
   `secret decrypt failed` WARNs with it. They survive only because Part A and
   Part B captured them first (`w3-3/partA-fetchlog.txt`,
   `w3-3/partB-controller.txt`).
2. **The agent throws the 500 body away** (`client.go:107-108`). Do not plan on
   reading the controller's message from the run's logs; it is not there. See
   correction 1.
3. **`\d` the live table.** `001_init.up.sql` still describes a `scope`/
   `scope_ref` shape that `016_drop_secret_scope.up.sql` removed.
4. **The byte budget is the argument, not the absence.** `encrypted_dek` is
   **67** bytes and `1 + 6 + 12 + 32 + 16 = 67` accounts for every one of them
   (version, `local:`, GCM nonce, wrapped DEK, tag). There is no room for a key
   identifier, which is a stronger statement than "no identifier was found".
   The value `ciphertext` is **56** bytes for a 27-byte secret
   (`1 + 12 + 27 + 16`) and carries **no provider tag at all** — only the DEK is
   wrapped by the `KeyManager`.
5. **Zero `ERROR` lines controller-side, for the entire session, on all three
   replicas** (`w3-3/consolidated.txt`) — against 23 decrypt failures. The
   agents logged 23 `ERROR`s (10 + 13) and **zero** mentions of `decrypt`, `kek`
   or `encryption key`. The severity and the information are on opposite sides
   of the socket.
6. **`max_fails` did not contaminate anything, and phase 4 is the strong test.**
   Nine consecutive 500s spread across all three upstreams produced **zero**
   `no live upstreams` / `temporarily disabled` lines in 1,606 nginx log lines
   (`w3-3/partA-nginx.txt`, `w3-3/consolidated.txt`). `http_500` is absent from
   `proxy_next_upstream` (`test/ha/nginx.conf:24`), so it is not an
   "unsuccessful attempt". **This is the opposite of W3-4's experience** and the
   difference is the status code, not the rig.
7. **No `nginx -s reload` was used and none was needed.** The W2-5 trap does not
   apply to this scenario at all; the only reconfiguration is a container
   recreate, which every client observes by definition.
8. **Budget ~2 s per run.** Trigger → terminal was 0.2-1.1 s throughout. 24 runs
   at 2 s spacing takes under a minute. The whole scenario is ~14 minutes
   including two image-less recreates.
9. **`test/ha/kek` is 66 bytes inside the container and `kek-b` is 65** — the
   committed `test/ha/kek` is CRLF on a Windows checkout and the freshly
   generated `kek-b` is LF. Both `TrimSpace` to 64 hex
   (`internal/config/keysource.go:208-211`), so both work. **Check key length
   inside the container, never on the host.**
10. **Postgres statement logging was never armed** (gate G7's decision), and was
    confirmed at defaults in a fresh session at teardown — `log_statement=none`,
    `log_line_prefix=%m [%p]` (`w3-3/teardown.txt`). Nothing in this scenario
    needs it.
11. **ADDED IN REVIEW — never `head` a survey you are going to call
    exhaustive.** `w3-3/docs-greps.txt` was truncated at 40 of 55 hits and the
    truncation was undisclosed; the dropped tail contained
    `docs/secrets.md:420`, which is the difference between an observation and
    a major violation (banner correction 6). Every survey in a later scenario
    must print its own `| wc -l` next to the output, and a "the docs are
    silent" verdict must cite that count.

**Sampler hygiene: none were launched.** Every capture was a synchronous
foreground command; there was no SSE stream and no polling loop. Checked anyway
on two passes — host `jobs` empty, host `ps` matching nothing, and **`ps` inside
all seven containers** matching no `curl`/`wget`/`psql` (the W3-4 lesson applied
even though it could not apply) — `w3-3/teardown.txt`. Zero non-terminal runs at
teardown, so nothing needed cancelling. Torn down with `down -v`; the
`agent-credentials` volume was removed.
