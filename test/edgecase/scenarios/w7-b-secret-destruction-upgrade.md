# W7-B — a v0.3.0 -> HEAD upgrade carrying a live secret past migration 016

**Follow-up Arm B.** Discharges campaign carry-forward **7(i)**
(`FINDINGS.md:3107`) and executes the measurement `FINDINGS.md:2961` names as
*"this entry's highest-value follow-up"*, in its own words:

> upgrade a v0.3.0 deployment carrying a live secret past 016 and observe what a
> running job that references it does — **if the run fails obscurely rather than
> with a "secret not found", or if any surface still lists the secret after its
> row is gone, that is silent corruption and this is critical.**

`FINDINGS.md:2959` is **code-read only**; this scenario is the execution. It
does not re-derive the migration audit — the enumeration of 5-of-17
backward-incompatible migrations stands as filed.

---

## Getting a pre-016 schema, and why not by running old migrations

The brief flagged that running the migrations to a pinned version might be
cheaper than running an old binary, because `FINDINGS.md:2993` §(a)(3) records
that a v0.3.0 **controller** predates `UNIFIED_CONTROLLER_KEY_FILE` and reads
only `UNIFIED_CONTROLLER_KEY`.

**Running the old binary turned out to be strictly cheaper, and the reason is
worth recording.** "Predates the key file" is a one-line `environment:`
difference, not an obstacle — `v0.3.0:internal/config/controller.go:100` reads
`UNIFIED_CONTROLLER_KEY` and the value is the same 64 hex characters
`test/ha/kek` holds. And the alternative is worse than it looks: migrating to
version 12 by hand gives a **schema** but no way to *write a secret into it*,
because the encryption path, the AAD binding and the secrets API all belong to
the binary. This arm needs a secret that a v0.3.0 controller actually created,
which only a v0.3.0 controller can produce.

`ghcr.io/eirueimi/unified-cd-controller:v0.3.0` and
`ghcr.io/eirueimi/unified-cd-agent:v0.3.0` are **public and pullable** (this is
the other half of `FINDINGS.md:3046`: the v0.3.0 tags were published normally;
it is `v0.4.0` that has no images), so the wave paid no build for the old side
at all.

Rig: `test/edgecase/compose/w7b-upgrade.yaml`, a **standalone** file rather than
an overlay — the HA file's three controllers are `*ctrl` aliases of one anchor,
so an overlay cannot version one of them, and an upgrade is a swap rather than a
mixed fleet anyway. **The KEK is the same value on both sides** (env for
v0.3.0, key file for HEAD), which models an operator who carried their key
across the upgrade — the most favourable case for the product, so nothing below
is lost merely because a key was thrown away.

```bash
docker compose -p edge-armb --project-directory . \
  -f test/edgecase/compose/w7b-upgrade.yaml up -d postgres old agentv030
#  ... set the secret, run the job ...
docker compose ... stop old agentv030
docker compose ... up -d --build new agentnew-enroll agentnew
docker compose ... down -v
```

---

## Prediction, recorded before the rig came up

- **B1** v0.3.0 migrates to its own embedded maximum and accepts the secret — **YES**.
- **B2** HEAD applies 013-017 and logs **nothing** naming the deletion.
- **B3** `GET /api/v1/secrets` after the upgrade is **empty**, so the
  "any surface still lists it" limb is **NOT** met.
- **B4** the job **fails at TRIGGER time with a clear secret-not-found
  message**, because the trigger path calls `prepareRunSpec` — *"Predict the
  message names the secret … i.e. NOT obscure — so the flip condition is NOT met
  and the arm records CONFORMANCE, leaving `:2959` at major."*
- **B5** sessions: not measurable without an OIDC provider; will be recorded as
  not measured.

## Delta

**B1, B2, B3 and B5 held. B4 was wrong on both of its clauses, and its wrongness
is the arm's result.** The trigger does **not** validate — it returns `200` and
creates a `Pending` run. The failure comes later, at the agent's secrets fetch,
and the message it produces names neither the secret nor the cause. So
`:2961`'s flip condition is **met on its first limb** and **not on its second**,
and the arm is the opposite of the conformance the prediction expected.

---

## Execution

2026-08-03, host clock. Captures: `w7/w7-b/step1-pre-upgrade.txt`,
`step2-upgrade.txt`, `step3-post-upgrade.txt`, `step4-surfaces-and-docs.txt`,
`all-container-logs.txt`.

### Step 1 — the pre-upgrade state (v0.3.0 controller, v0.3.0 agent)

```
schema_migrations         version 12, dirty f
secrets columns           id, name, scope, scope_ref, encrypted_dek, ciphertext, created_at, updated_at
controller_settings       has column controller_key_hex
secrets row               EDGE_KEK_PROBE | global | '' | ciphertext 54 B | encrypted_dek 60 B
GET /api/v1/secrets/      [{"name":"EDGE_KEK_PROBE","scope":"global",...}]   HTTP 200
```

`edge-secret-user` applied and triggered; run
`c9de2111-8076-4fa3-a45a-bea1bfb67738`, `claimedBy: agentv030`, **`Succeeded`**,
log:

```
secret-user-begin
secret-len=26
secret-user-end
```

**That is the control the whole arm rests on: identical job text, green, reading
the secret, before the upgrade.**

`controller_settings` held **zero** rows — with `UNIFIED_CONTROLLER_KEY` set,
v0.3.0 never persisted a KEK to the database — so `015:11`'s `DROP COLUMN
controller_key_hex` destroyed nothing *in this configuration*. Recorded because
it means the ciphertext's wrapping key was carried across the upgrade intact and
the loss below cannot be attributed to a discarded key.

### Step 2 — the upgrade

`stop old agentv030` at `12:06:20`; HEAD controller up, `/healthz` 200 at
`12:06:53`. **Its entire boot log is ten lines**, quoted complete because the
completeness is the finding:

```
postgres pool configuration
key file /run/secrets/kek is readable by group or others (mode 0777)   [WARN]
encryption key loaded  source="key file /run/secrets/kek"
no object store configured — log archival disabled                     [WARN]
audit log retention enabled
run retention disabled (keep forever)
log trim disabled (DB log rows kept forever)
controller listening
http request GET /healthz 200
scheduler became leader
```

**There is no migration line at all** — not a version, not a count, not a name.
Migrations 013, 014, 015, 016 and 017 ran inside this window, including
`015:13 DELETE FROM public.sessions` and `016:13 DELETE FROM public.secrets`,
and the operator's entire signal is that the process started.

### Step 3 — what the operator holds afterwards

```
schema_migrations       version 17, dirty f
secrets columns         id, name, encrypted_dek, ciphertext, created_at, updated_at   (scope, scope_ref gone)
secrets rows            0
sessions                0
controller_key_hex col  0
runs                    1        (the pre-upgrade run survives intact)
jobs                    edge-secret-user   (still references {{ .Secrets.EDGE_KEK_PROBE }})
GET /api/v1/secrets/    []                 HTTP 200
```

The pre-upgrade run still reads back `Succeeded` with `secret-len=26` in its
logs. **The history is intact; only the secret is gone.**

### Step 4 — the measurement `:2961` asked for

A HEAD agent (`agenthead`, enrolled through the ordinary API) is brought up and
the **same job** is triggered:

```
POST /api/v1/runs {"jobName":"edge-secret-user"}   ->  HTTP 200
{"id":"f2557959-…","status":"Pending"}                  <- the trigger does NOT validate
... 0.6 s later ...
run f2557959-…  status Failed   claimedBy agenthead
step[0] usesecret  status Pending   (no exitCode — it never started)
```

and the run's **only** log line, on `stderr`, at `stepIndex -1`:

```
fetch secrets for run f2557959-c494-4faa-a478-3df1708d211b: http 404: response omitted
```

That line names a run id and an HTTP status. It does **not** name
`EDGE_KEK_PROBE`, does not say "secret", does not say "not found", and does not
hint at an upgrade.

**The controller composed the right message and nothing records it.**
`internal/controller/api_secrets.go:129` writes
`http.Error(w, "secret "+name+" not found", http.StatusNotFound)` — under a
comment that says *"Fail loudly instead"* — and the message dies twice over:

- the agent replaces the body of **every** `>= 400` response with the literal
  `"response omitted"` (`internal/agent/client.go:108`), which is deliberate
  secrets hygiene and is **already on the campaign's record as a constraint on
  measurement rather than a defect to re-file** (W3 plan facts block, measured
  at `FINDINGS.md:1567`); and
- the controller's own access log is `accessLogMiddleware`
  (`internal/controller/server.go:174-188`), which emits method, path, status,
  duration and remoteAddr and **never the body**. Its line for this request is
  `POST /api/v1/agents/agenthead/secrets/fetch status 404 duration_ms 2`.

So the string `secret EDGE_KEK_PROBE not found` exists, is correct, and is
written to an HTTP response body that **no product surface anywhere records**.

### Step 5 — the surfaces, and the docs

- **`GET /api/v1/secrets/` → `[]`.** The second limb of `:2961`'s flip condition
  is **not** met by the secrets surface.
- **The audit API still returns
  `{"action":"secret.set","resource":"EDGE_KEK_PROBE","status":204}`.** This is
  named rather than glossed, and then **declined** as a "surface still lists the
  secret": an audit row records that a `set` *happened*, which is true history
  and is what an audit log is for. Reading it as a live listing would make every
  audit row a stale-state bug.
- **Controller lines matching `not found|deleted|migration|migrate|016` across
  the whole 91-line log: zero.**
- **`docs/`: no warning exists.** `grep -rniE "re-?set (your )?secrets|secrets
  are deleted|secrets will be (deleted|lost)" docs/` returns nothing;
  `docs/secrets.md` has no upgrade or migration section at all (its headings run
  Overview → Prerequisites → CLI → Job YAML → Masking → Security Model → Vault →
  Troubleshooting); and `docs/operations.md`'s Upgrades section is a four-step
  numbered procedure whose only exception is the `79c1074` squash. This
  re-confirms `:2973`'s survey against a live upgrade rather than a grep.
- **`docs/troubleshooting.md` does carry `secret "<name>" not found`** — but for
  **webhook signature verification** (`:241-252`), a different path with a
  different message, so an operator grepping their run's actual error string
  reaches nothing.

---

## Findings

| # | FINDINGS.md | Kind | Severity | Subject |
|---|---|---|---|---|
| 1 | W7-B entry | observation | **minor (observation)** | the executed follow-up: the destruction is real, announced nowhere, and surfaces only as a content-free run failure — `:2961`'s flip condition is met on one limb of two, and the *consequent* does not survive being argued from `:6`'s text |

**Why this is an observation and not a second violation.** Everything wrong here
is already owned by `FINDINGS.md:2959` on the `docs/operations.md:191` limb, and
`:2975` already prescribes the fix (the Upgrades warning and the
`docs/secrets.md` warning). Filing a violation would file one contract twice —
the same test `:2961` applies to its own Class D. What is new is **measurement**,
and measurement of a stated-but-unrun condition is what an observation entry is
for.

**Why `:2959` is NOT moved to critical, although its own text says to.**
`:2961`'s conditional promises *"that is silent corruption and this is
critical"*. The campaign's most-enforced rule is that a band is argued from
`:6-8`'s own text and never inherited; a conditional written **before** the
measurement is exactly such an inheritance, and it does not survive contact with
`:6`'s words:

- **Security** — declined; 015 and 016 both *narrow* exposure.
- **Silent corruption** — declined on *corruption*, using the line `:3010` drew
  and the campaign summary endorsed: nothing the product stores is **damaged**.
  The `secrets` table is empty and every surface reports it empty, consistently.
  A row that is **absent**, and correctly reported absent, is not a corrupt row.
- **Data loss** — this is the live disjunct, and `:2961` already argued it at
  length and declined it on two reads of the migration files (`016:7-11`'s AAD
  premise and `016:9-11`'s stated deployment premise). **Nothing measured here
  disturbs either ground**, and the one thing that would — that some real
  deployment holds secrets it cannot re-set — is the inference about operator
  practice `:2961` explicitly labelled as carrying no weight, and a rig this
  scenario built cannot supply it.

**What the measurement DOES move is the entry's own closing sentence.**
`:2961` ends *"What survives both grounds is silence rather than loss — and
silence is `:8`'s own word."* This arm shows the silence is **total and
double**: nothing at the time of destruction, and nothing at the time of
consequence, because the one correct message the product composes is thrown away
by two independent, individually-defensible policies. That is a stronger version
of the same finding at the same band, and it sharpens `:2975`'s fix rather than
replacing it.

## What this scenario does NOT establish

- **`015:13 DELETE FROM public.sessions` was not exercised**: the rig has no
  OIDC provider, `sessions` held **0** rows before and after, and the limb is
  recorded as **not measured**, exactly as predicted.
- **The AAD argument was not re-derived.** `:2961`'s ground (1) — that the
  ciphertext had already ceased to be authenticatable before `016:13` ran — is
  taken as read from that entry. What this arm adds is only that the row it
  destroyed carried `scope='global'`, which is consistent with it.
- **No N-2 boot failure was reproduced.** `:2972`'s `42703` claim is about a
  v0.3.0 binary against a **>= 15** schema; this arm ran v0.3.0 against a
  v0.3.0-era schema and then swapped. Running the old binary *after* the upgrade
  would test it and was not done.
- **One replica, not three.** Nothing here speaks to concurrent migration
  startup, which `:2993` settles by reading anyway.
