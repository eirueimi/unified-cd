# W5-1 — the migration backward-compatibility audit: **NOT EXECUTED (code-read only)**

**Wave W5, Task 1.** `docs/operations.md:191` tells an operator that during a
rolling deploy old and new controller binaries may both run against the
already-migrated schema, on the stated ground that unified-cd's migrations are
additive "columns/tables — this is the norm for unified-cd's migration
history". This audit reads all seventeen migrations and asks whether that
ground is true. It is not.

**Every claim in this runbook is a read.** Nothing was executed: no rig was
built, no controller was started, no Postgres was touched. The precedent for
filing a code-read-only entry is `FINDINGS.md:1864` (W3-6), whose title carries
`NOT EXECUTED (code-read only)` and whose **Repro** line opens by saying so;
this scenario matches that labelling and applies it to all three of its filed
entries. Where an effect is derived from reading rather than observed, the
sentence says **derived** or **code-read**. **No number in this runbook comes
from a measurement, because there are none.**

**Why this is a legitimate filing and not a shortfall.** The wave plan
(`docs/superpowers/plans/2026-08-03-edge-case-campaign-w5.md:12-18`) records the
decision: reconnaissance found that three of the design doc's four W5
sub-scenarios are answered by reading, that running them would produce
confirmations rather than findings, and that two cold controller image builds
were the price. §"Step 6 — refuted scenarios, recorded as RESULTS" below records those
three as **results about the product**, which is what they are.

---

## Captures

All four are code surveys, held in the session scratchpad and archived at the W5
checkpoint under `edgecase-evidence/w5/w5-1/` (the path every citation below and
in `FINDINGS.md` uses):

| File | What it is |
|---|---|
| `migration-sweep.txt` | the destructive-operation regex over **all 17** `*.up.sql`, untruncated, 256 lines |
| `docs-compat-survey.txt` | the `docs/` compatibility survey with its hit count, untruncated |
| `sentinels-per-tag.txt` | `schemaSentinels` extracted from every git tag **and** HEAD |
| `v030-dropped-column-refs.txt` | v0.3.0's `internal/store` references to columns 015/016 drop |

---

## Corrections to the wave plan's reconnaissance

Seven consecutive waves have had a plan's "verified code facts" corrected by
execution, with the pattern that `file:line` claims hold and *mechanism* claims
fail. **This task is pure reading, so checking those claims IS the task.** The
pattern held an eighth time, and in exactly that shape: **every `file:line` in
the plan resolved to what the plan said was there. Three counts and one
mechanism did not.**

### CORRECTION 1 — "**Four** of the last five migrations are backward-incompatible" is **three**, and "only 017 is clean" omits 013

Plan `:46` and `:60`. The last five migrations are **013, 014, 015, 016, 017**.
Backward-incompatible among them: **014, 015, 016 — three, not four.** The
plan's own table (`:48-58`) lists exactly those three plus **003** and **005**,
which are not in the last five; the headline appears to have counted table rows
rather than migrations in the window.

**013 is purely additive and belongs with 017 in the clean column.**
`013_agent_identity_auth.up.sql` is 65 lines of four `CREATE TABLE`s
(`agent_identities` `:1-12`, `agent_credentials` `:18-32`,
`agent_enrollment_tokens` `:37-48`, `agent_enrollment_policies` `:50-65`) and
three `CREATE INDEX`es (`:14-16`, `:34`, `:35`). **It contains no `ALTER`, no
`DROP`, no `DELETE` and touches no pre-existing table.** Its `CHECK` constraints
(`:4`, `:21`, `:23`, `:24`, `:40`, `:53`, `:64`) are all inline in tables it
creates in the same statement, so no older binary can be holding a row that
violates one. **So the clean set among the last five is {013, 017}, not {017}.**

### CORRECTION 2 — the plan's destructive-op table is **accurate line-for-line but not the full class**; the sweep finds a fourth class it does not name

Every row of `plan:48-58` was checked against the file and **every one resolves
to the operation named at the line named** (see §"Step 1 — the enumeration" below, which
reproduces the table with the sweep's own line numbers). But the sweep found a
class the table has no row for: **newly-tightened constraints**, which break an
older binary's *writes* rather than its *reads* and therefore do not show up
under a "column drop / constraint drop" lens at all. Three instances:
`005:3` and `005:7` (widened PKs), `016:18` (`ADD CONSTRAINT secrets_name_key
UNIQUE (name)`), `014:37-38` (a narrower `CHECK` than `013:64`'s). This is the
class that "additive columns/tables" is least equipped to describe, because
**every one of these three is an `ADD`.**

### CORRECTION 3 — the docs survey is **27 hits, not 20**; substantive is **13, not 8**

Plan `:191` records reconciliation at "20 hits of which 8 are substantive".
**Refuted, in the direction of more.** The survey run here
(`docs-compat-survey.txt`, regex printed in the file's header, `docs/` only,
`docs/superpowers/` excluded, untruncated) returns **27** hits. The regexes are
not identical and the reconciliation's was not recorded, so **this is a
different measurement rather than a refutation of the same one** — but the
substantive fraction is what the step is actually asking about, and that is
**13**, listed in §"Step 2 — the contract, and its 12 neighbours" below. The 14
non-substantive hits are, exhaustively: seven `S3-compatible`
(`configuration.md:33`, `:68`, `getting-started.md:55`, `high-availability.md:29`,
`kubernetes-integration.md:433`, `operations.md:14`, `jobs.md:1299`), two
`GitHub-compatible` webhook-header references (`resources.md:397`,
`troubleshooting.md:276`), two DSL-semantics uses of "incompatible with holding
one isolated environment" (`jobs.md:1260`, `resources.md:195`), one bare
cross-link (`getting-started.md:455`), one checklist item
(`high-availability.md:553`), and one **table-of-contents line**,
`high-availability.md:13` (`- [Rolling Deploys and Graceful Shutdown](#…)`).
**That last item is a correction: an earlier version of this list ended with
"one `N-1`-shaped false positive absorbed by the same lines", and there is no
`N-1` hit in `docs-compat-survey.txt` at all** — the regex alternation includes
`N-1` but nothing in `docs/` matches it. The phantom accounted for the 14th slot
that `high-availability.md:13` actually occupies. **7 + 2 + 2 + 1 + 1 + 1 = 14,
and 27 − 14 = 13, which reconciles against the substantive list in §"Step 2 —
the contract, and its 12 neighbours".** In a task whose deliverable is
enumerations, "exhaustively" has to be earned by an enumeration that adds up.
**The count is reported rather than the survey truncated**, per the standing
rule.

### CORRECTION 4 (mechanism) — the `verifySchema` race needs a **stricter** precondition than "a mixed-version boot", and the failure it produces is **already documented, with a remedy the rig cannot deliver**

Plan `:73-86`. The `file:line` chain is exactly right and re-derives (§"Step 5 —
`verifySchema` runs outside the advisory lock" below). Two things about the
*mechanism* are not.

**(a) The window needs the later-locking replica to hold the NEWER migration
set, not merely a different one.** golang-migrate sets `dirty=true` only from
inside `runMigrations`, immediately before executing a migration
(`migrate.go:738`, `SetVersion(migr.TargetVersion, true)`, cleared at `:750`).
A replica whose embedded set is already satisfied returns `ErrNoChange` from
`readUp` and **never writes `dirty` at all**. So three same-version replicas
cold-starting a fresh database cannot produce this: the first to take the lock
applies everything, and the second and third find nothing to apply. The window
opens only when replica A finishes and unlocks while a replica B that has
*strictly more* migrations then acquires the lock. **The plan's "only during a
mixed-version boot" is right, and it is right for a narrower reason than it
gives.**

**(b) The failure is documented — and the documented remedy is "restart the
controller", which nothing in the shipped rig does.** `docs/troubleshooting.md:1204`
already names this exact case: *"another replica's migration is currently in
flight (this can happen transiently during a mixed-version rolling deploy).
Restart the controller first"*. The controller's own error string says the same
(`internal/store/verify.go:64-66`). **That reframes the finding**: it is not an
unanticipated race, it is an anticipated one whose recovery is delegated to a
restart that `cmd/controller/main.go:257-259` does not perform and that
`test/ha/docker-compose.ha.yaml` does not arrange. That is how it is filed.

### CORRECTION 5 — the plan's hypothesis "a v0.3.0 controller in this rig exits at startup on `EnsureControllerKey`" is **true, and true for a second reason the plan does not give**

Plan `:145` lists it under "explicitly inferred, not verified", with the
`UNIFIED_CONTROLLER_KEY_FILE`-vs-`UNIFIED_CONTROLLER_KEY` reason
(`docker-compose.ha.yaml:84`). That reason is a **rig** property. There is a
second, **rig-independent** one: `v0.3.0:cmd/controller/main.go:215` calls
`pg.EnsureControllerKey`, whose SQL reads and writes `controller_key_hex`
(`v0.3.0:internal/store/postgres.go:1876`, `:1879`, `:1881`, `:1883`) — the
column `015_secrets_v2.up.sql:11` drops. **Against any schema at version ≥ 15 a
v0.3.0 controller cannot get past boot, on any rig, with any environment.**
Recorded because it settles the hypothesis *and* widens it.

### Checked and HELD, recorded so a later reader does not re-check them

- `docs/operations.md:191` is at line **191** and reads verbatim as quoted (§below).
- `internal/store/verify.go:25-27` carries the unenforced rule; `:21-23` carries
  the enforced one. Both at the stated lines.
- `internal/store/postgres.go:137` is `m.Up()`; `:142` is `verifySchema(db)`.
  Nothing between them re-acquires a lock.
- `cmd/controller/main.go:257-259` is `pg.Migrate` → `slog.Error("migrate")` →
  `os.Exit(1)`. **Exactly** those lines.
- `internal/store/verify.go:55` reads `version, dirty`; `:62-68` is the hard-fail.
- golang-migrate **v4.19.1** (`go.mod`): `func (m *Migrate) Up()` at
  `migrate.go:265`, opening with `m.lock()` and returning through
  `m.unlockErr(...)` at `:283`. The postgres driver's `Lock()` is
  `SELECT pg_advisory_lock($1)` at `database/postgres/postgres.go:241`, keyed by
  `GenerateAdvisoryLockId(DatabaseName, migrationsSchemaName,
  migrationsTableName)` at `:235` — **identical across replicas by
  construction**, and the comment at `:240` says it waits indefinitely.
- v0.3.0's `schemaSentinels` is exactly entries **1-12** (`sentinels-per-tag.txt`).
- `test/ha/docker-compose.ha.yaml:79-81` is `controller1: &ctrl` with `build:`;
  `:103-104` are the two anchor aliases.

---

## Step 1 — the enumeration, over all 17 migrations

**Method.** `internal/store/migrations/` holds **34** files, **17** `*.up.sql`
and 17 `*.down.sql`; the up files are `001_init` through `017_run_detached`,
numbered contiguously. `internal/store/verify_test.go:30-46`
(`TestSchemaSentinelsCoverAllMigrations`) independently pins the same 17 by
counting `.up.sql` entries in the embedded FS and requiring
`len(schemaSentinels)` to equal it — so **the denominator is enforced by the
product's own suite, not only by this sweep.** Every up file was then read in
full and swept with one regex (`migration-sweep.txt` header carries it verbatim).

**The denominator is the POST-SQUASH set, and this is said because
`docs/operations.md:191` speaks of "migration history".**
`internal/store/migrations/001_init.up.sql:1-4` is itself the squash of a
*former* 001-017 (commit `79c1074`, "squash migrations 001-017 into a single
consolidated init", 2026-07-04, whose own header records that it is a breaking
change for databases created by the old numbered migrations). So every "of the
17" in this runbook means **of the 17 currently embedded**; the pre-squash
history is not in the tree and was not swept. **It does not weaken anything** —
an unswept history can only add backward-incompatible operations to the true
total, never remove one from this count — but the scope of the claim is stated
rather than left to be assumed.

**Definition used throughout.** An operation is **backward-incompatible** if a
controller binary whose embedded migration set stops at some version < *n* can
fail or misbehave against a schema on which *n* has been applied. Read failures
(`42703 undefined_column`), write failures (constraint violations) and silent
data loss all count; a *transient* drop that the same file compensates before
committing does not, and is marked as such.

### Verdict per migration — all 17

| # | file | verdict |
|---|---|---|
| 001 | `001_init.up.sql` | **baseline** — creates the whole schema; there is no earlier binary for it to be incompatible with |
| 002 | `002_add_role.up.sql` | additive (`:1`, `:2`: `ADD COLUMN ... NOT NULL DEFAULT 'admin'`) |
| 003 | `003_appsource_managed_resources.up.sql` | **INCOMPATIBLE** |
| 004 | `004_audit_logs.up.sql` | additive (one `CREATE TABLE`) |
| 005 | `005_matrix_variant.up.sql` | **INCOMPATIBLE** |
| 006 | `006_appsource_sync_status.up.sql` | additive (`:2-3`, two `ADD COLUMN ... NOT NULL DEFAULT ''`) |
| 007 | `007_step_call_link.up.sql` | additive (`:2-3` two nullable `ADD COLUMN`, `:4` index) |
| 008 | `008_run_indexes.up.sql` | additive (`:10-12`, three `CREATE INDEX IF NOT EXISTS`) |
| 009 | `009_agent_capabilities.up.sql` | additive (`:1` nullable, `:2` `NOT NULL DEFAULT`) |
| 010 | `010_sidecar_status.up.sql` | additive (one `CREATE TABLE`) |
| 011 | `011_runs_terminal_updated_idx.up.sql` | additive (`:11-12`, one partial index) |
| 012 | `012_run_log_archives_trimmed_at.up.sql` | additive (`:5`, `:14`, `:15`, all `ADD COLUMN IF NOT EXISTS`, the two `NOT NULL` ones with `DEFAULT 0` **retained**) |
| 013 | `013_agent_identity_auth.up.sql` | additive — **see CORRECTION 1** |
| 014 | `014_agent_enrollment_policies.up.sql` | **INCOMPATIBLE** |
| 015 | `015_secrets_v2.up.sql` | **INCOMPATIBLE** |
| 016 | `016_drop_secret_scope.up.sql` | **INCOMPATIBLE** |
| 017 | `017_run_detached.up.sql` | additive (`:1`, `ADD COLUMN ... NOT NULL DEFAULT false`) |

**5 of 17 migrations carry at least one backward-incompatible operation:
003, 005, 014, 015, 016.** Twelve do not (001 counted as the baseline).

### The operations, by class

**Class A — column drop (6 columns, 4 migrations).** Each makes every older
binary's `SELECT`/`INSERT` naming it fail with `42703 undefined_column`.

| Column | Statement |
|---|---|
| `app_sources.managed_jobs` | `003_appsource_managed_resources.up.sql:11` |
| `agent_enrollment_policies.access_token_ttl` | `014_agent_enrollment_policies.up.sql:23` |
| `controller_settings.controller_key_hex` | `015_secrets_v2.up.sql:11` |
| `sessions.refresh_token` | `015_secrets_v2.up.sql:15` |
| `secrets.scope` | `016_drop_secret_scope.up.sql:16` |
| `secrets.scope_ref` | `016_drop_secret_scope.up.sql:17` |

**Class B — constraint drop (5 statements, 3 migrations), of which 2 are
compensated in the same file.**

| Statement | Note |
|---|---|
| `005:2` `DROP CONSTRAINT step_reports_pkey` | **compensated** — re-added widened at `005:3` |
| `005:6` `DROP CONSTRAINT step_outputs_pkey` | **compensated** — re-added widened at `005:7` |
| `014:21` dynamic `DROP CONSTRAINT` loop over constraints mentioning `access_token_ttl` | count not statically determinable; `013:64` created exactly one such `CHECK` |
| `014:30` dynamic `DROP CONSTRAINT` loop over constraints mentioning `access_token_ttl_seconds` | idempotency guard |
| `016:15` `DROP CONSTRAINT IF EXISTS secrets_name_scope_scope_ref_key` | replaced by a **narrower** constraint at `016:18` — see Class E |

**Class C — `NOT NULL` with the default removed (3 columns, 2 migrations).**
This is the class that breaks an older binary's `INSERT`: the column is
mandatory and the database will not supply a value.

| Column | Statements |
|---|---|
| `agent_enrollment_policies.access_token_ttl_seconds` | `014:35` `SET NOT NULL`, `014:36` `DROP DEFAULT` |
| `sessions.refresh_token_dek` | `015:16` add `NOT NULL DEFAULT ''::bytea`, `015:18` `DROP DEFAULT` |
| `sessions.refresh_token_ct` | `015:17` add `NOT NULL DEFAULT ''::bytea`, `015:19` `DROP DEFAULT` |

**Class D — data deletion (3 operations, 2 migrations).** Two are unqualified
`DELETE FROM` with **no `WHERE`**; the third destroys data via DDL.

| Operation | Statement | What is destroyed |
|---|---|---|
| `DELETE FROM public.sessions` | `015:13` | every OIDC session; all users must log in again |
| `DROP COLUMN ... controller_key_hex` | `015:11` | the **KEK itself**, i.e. the key for every wrapped DEK still in the database |
| `DELETE FROM public.secrets` | `016:13` | **every secret**, deliberately (`016:7-12` explains: the AAD changes, so no existing ciphertext can be authenticated) |

**Class E — newly-tightened constraints (3, 3 migrations). Not in the plan's
table; see CORRECTION 2.** These reject writes an older binary makes
*successfully today*.

| Constraint | Statement | What it now rejects |
|---|---|---|
| `step_reports_pkey (run_id, step_index, variant)` | `005:3` (and `005:7` for `step_outputs`) | an older binary's `ON CONFLICT (run_id, step_index)` would have no matching unique index → `42P10`. **INFERRED — see the note below the table** |
| `secrets_name_key UNIQUE (name)` | `016:18` | two secrets sharing a name across scopes — which `001_init.up.sql`'s `UNIQUE (name, scope, scope_ref)` permitted |
| `access_token_ttl_seconds BETWEEN 300 AND 14400` | `014:37-38` | TTLs between 4h and 24h, which `013:64`'s `CHECK` allowed |

**Row 1 is INFERRED and is labelled so, because it cannot be checked against any
tag.** No tag predates 005 (`v0.0.1` already embeds 001-007), so there is no
released binary whose `step_reports` upsert can be inspected, and nothing in this
audit shows any binary ever wrote `ON CONFLICT (run_id, step_index)`. It is the
shape of the class, not evidence for it. **The evidenced instance is
`v0.3.0:internal/store/postgres.go:1541`** — a real `ON CONFLICT (name, scope,
scope_ref)` in a real released binary, invalidated by `016:15`/`:18`. Rows 2 and
3 plus that site carry the class on their own; row 1 is retained only as the
illustration.

**Reachability caveat, stated because it bounds two of the classes.** `v0.0.1`
— the earliest tag — already embeds migrations **001-007**
(`git ls-tree v0.0.1 internal/store/migrations/`). So **003 and 005 predate
every released tag**, and their breakages (Class A row 1, Class B rows 1-2,
Class E row 1) are **not reachable through any tag pair this repository can
assemble**. They are counted because the enumeration is of the migration
history, which is what `docs/operations.md:191` makes its claim about. The
classes that *are* reachable through a real pair (v0.3.0 ↔ HEAD) are 014, 015
and 016 — and those are three of the last five migrations.

---

## Step 2 — the contract, and its 12 neighbours

`docs/operations.md:191`, quoted verbatim from the file (the whole numbered
item; the operative clause is bolded here and is unbolded in the source):

> 1. **Controller** — database migrations run automatically at startup
> (`internal/store`, via `golang-migrate` against the embedded migration set).
> Roll controller replicas one at a time in an HA deployment; the new version's
> migrations apply once, and old and new controller binaries can both be running
> against the already-migrated schema during a rolling deploy **as long as the
> migration is backward-compatible (additive columns/tables — this is the norm
> for unified-cd's migration history)**.

The sentence has two limbs and only the second is falsifiable by reading:

- **The conditional** — "*as long as* the migration is backward-compatible" — is
  correct and is not what this audit contradicts.
- **The parenthetical** — "*this is the norm for unified-cd's migration
  history*" — is a **factual claim about this repository**, and it is the one
  the enumeration above refutes.

### The 13 substantive hits

`docs-compat-survey.txt`, **27 hits**, of which these 13 say something about
version skew, upgrade ordering or cross-version compatibility:

| Hit | What it says | Bearing |
|---|---|---|
| `operations.md:189` | "Upgrade order: **controller first, then agents.**" | the ordering rule the contract sits under |
| `operations.md:191` | the contract | **contradicted** |
| `operations.md:191` (2nd sentence, the `79c1074` squash **Exception**) | a pre-squash database is silently not upgraded | already-known separate hazard; not this finding |
| `operations.md:195` | k8s-agent and sidecar image "**must be upgraded in lockstep**" | a correctly-stated incompatibility — the doc *can* say this when it means it |
| `troubleshooting.md:375` | the same sidecar mismatch from the symptom side | consistent |
| `troubleshooting.md:1204` | the dirty flag "can happen transiently during a mixed-version rolling deploy … Restart the controller first" | **load-bearing for Step 5** |
| `agents.md:316-322` | legacy/no-capability agents are capability-agnostic so "a rolling upgrade … never strands a run" | the **real** compatibility mechanism, and it is about agents, not schema |
| `agents.md:654` | "**There is no backward-compatibility shim for this change.**" | again, the doc states breaks plainly elsewhere |
| `jobs.md:638` | same phrasing for a DSL change | ditto |
| `authorization.md:190-194` | §"Migration / backward compatibility": PATs and sessions "migrate to `role = 'admin'` (column default), so current single-token workflows keep working" | true — `002:1-2` is additive with a default; **this is what a backward-compatible migration actually looks like in this repo** |
| `configuration.md:277` | compatibility for a legacy credential id form | narrow, holds |
| `high-availability.md:271-279` | rolling deploys, "no downtime", drain sequencing | promises availability across a roll; §Step 5's race is a counter-example that this passage does not qualify |
| `high-availability.md:132` | graceful-stop row of the shutdown table | consistent |

**The pattern worth naming: the docs are good at declaring breaks and bad at
one summary sentence.** `operations.md:195`, `agents.md:654` and `jobs.md:638`
all state incompatibilities bluntly and correctly. The defect is localised to
`operations.md:191`'s parenthetical, which characterises the *whole history*
in five words — and gets it wrong for 5 of 17 migrations, including three of
the last five.

---

## Step 4 — the unenforced sentinel rule

`internal/store/verify.go:21-27` is one comment block stating **two** rules:

> `// schemaSentinels must contain exactly one entry per migrations/*.up.sql,`
> `// in version order. TestSchemaSentinelsCoverAllMigrations enforces this:`
> `// adding a migration without a sentinel fails the suite.`
> `//`
> `// A later migration must never drop or rename a sentinel object; if one`
> `// must, the sentinel entry has to be changed in the same commit, or older`
> `// binaries verifying a newer database will report false drift.`

The first rule (`:21-23`) names its enforcer and the enforcer does enforce it:
`internal/store/verify_test.go:30-46` counts `.up.sql` entries in the embedded
FS, requires `len(schemaSentinels)` to match, and requires versions `1..N` in
order with non-empty `migration` and `table`. **The second rule (`:25-27`) names
no enforcer and has none** — the survey `grep -rn "schemaSentinels\|verifySchema"
--include=*_test.go internal/` returns 7 lines, all inside `verify_test.go`, and
none of them compares a sentinel against any later migration's text.

**Adjudication: this is a `Notes` clause on the main violation, not a second
violation and not a standalone observation.** The reasoning, against the
campaign's own classification rule (`FINDINGS.md:479` — a contract limb is an
invariant's own text or a statement in `docs/*.md`; **not** an inline comment
inside a function body and **not** an unexported helper's doc comment):

- `schemaSentinels` is an **unexported package-level variable** and the text is
  its doc comment, so it fails `:479`'s **inclusion** limb outright: a contract
  limb must be *"an exported API field, a schema column, or a statement in
  `docs/`"*, and a comment on an unexported var is **none of the three**. **That
  is the ground used, and it is deliberately not the exclusion limb** — `:479`
  also excludes "an unexported helper's own doc comment", but `schemaSentinels`
  is a **var**, not a helper, so the exclusion would be reached by analogy where
  the inclusion is an exact fit. Same verdict, no stretch; and W6 re-banded four
  entries for reasoning looser than an analogy.
- It is also not an *independent* observation, because **its subject is the same
  fact as the violation's**: the rule exists precisely because migrations in
  this repository drop things. It is corroboration of intent — the codebase
  knows destructive migrations happen and wrote a rule about one consequence —
  and it belongs where corroboration goes.
- **And the hazard is latent rather than live** (§Step 6, refuted scenario 2):
  no tag's sentinel list names an object any later migration drops, so the
  false-drift the comment warns about has never been reachable.

Filed accordingly, in the **Notes** of the violation at `FINDINGS.md` (W5-1,
first entry), with the enumeration that shows the hazard is latent.

---

## Step 5 — `verifySchema` runs outside the advisory lock

### The chain, every link re-read

1. `internal/store/postgres.go:137` — `if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) { return err }`.
2. `migrate.go:265-283` (golang-migrate **v4.19.1**) — `Up()` opens with
   `m.lock()` (`:266`) and every exit is through `m.unlockErr(...)` (`:283`).
   **The lock covers `Up()` and nothing after it.**
3. `database/postgres/postgres.go:233-248` — `Lock()` is
   `SELECT pg_advisory_lock($1)` (`:241`) with the id from
   `GenerateAdvisoryLockId(DatabaseName, migrationsSchemaName,
   migrationsTableName)` (`:235`) — the same three inputs on every replica, so
   the same id. Session-scoped; the comment at `:240` says it "will wait
   indefinitely".
4. `internal/store/postgres.go:142` — `return verifySchema(db)`. **This is
   after `Up()` returned, therefore after `Unlock()` ran.** Nothing between
   `:137` and `:142` re-acquires anything.
5. `internal/store/verify.go:55` — `SELECT version, dirty FROM schema_migrations`.
6. `internal/store/verify.go:62-68` — `if dirty { return fmt.Errorf(...) }`.
   **Unconditional. No retry, no backoff, no sleep-and-reread anywhere in the
   function or its caller.**
7. `cmd/controller/main.go:257-259` — `if err := pg.Migrate(*dsn); err != nil {
   slog.Error("migrate", "error", err); os.Exit(1) }`.

### Who sets `dirty`, and why that narrows the window

`migrate.go:738` — `m.databaseDrv.SetVersion(migr.TargetVersion, true)`,
immediately before running each migration, cleared at `:750`. A replica with
nothing to apply never reaches `:738`. **`:738` is the only `SetVersion(…, true)`
in the package**: `grep -n SetVersion migrate.go` returns exactly three sites —
`:374` (inside `Force`, passing `false`), `:738` (`true`) and `:750` (the
clear). **And the corollary is sharper than "the lock is scoped to `Up()`":
`Up()` re-reads `dirty` itself at `migrate.go:270-277`, INSIDE the lock, and
returns `ErrDirty` before applying anything — so `verifySchema` at
`internal/store/postgres.go:142` is the ONLY unlocked `dirty` read in the
process.** The library serialises its own read of the flag correctly; the
product's read is the one that is not. **So the interleaving requires the
replica that acquires the lock second to hold a strictly larger migration set**
(CORRECTION 4a). Concretely: replica **A** (older binary, set ends at 16)
finishes `Up()` and unlocks → replica **B** (newer, set ends at 17) acquires and
writes `dirty=true` → **A**'s `verifySchema` at `:142` reads `dirty=true` →
`:62-68` hard-fails → `main.go:259` `os.Exit(1)`.

### **The window width was NOT measured.**

Stated plainly because the campaign's rule requires it. No rig was built, no
`Up()` was timed, and no interleaving was observed. **Nothing in this section is
a measurement.** What is *read* is that the window is bounded below by zero and
bounded above by the duration of one migration's SQL plus two `SET VERSION`
statements — an interval this audit did not time and will not estimate.

### The `restart:` claim, verified by enumeration rather than asserted

The plan says `test/ha/docker-compose.ha.yaml` "sets **no** `restart:` policy on
the controllers". **Enumerated** (`ha-restart-enumeration.txt`):
`grep -rn "restart" test/ha/` returns **2** hits over the whole directory —
`docker-compose.ha.yaml:168` and `hardfailure_test.go:115` (a comment). The
compose file declares **10** services (`postgres:3`, `garage:25`, `mc:58`,
`controller1: &ctrl:78`, `controller2: *ctrl:103`, `controller3: *ctrl:104`,
`nginx:106`, `agent-enroll:126`, `agent1:179`, `agent2:216`). The single
`restart:` directive is `restart: "no"` at `:168`, inside the `agent-enroll`
block (`:126-168`). **So the claim is true of the controllers and needs one
qualification: the file is not silent about `restart:` — it names it once,
explicitly, on a different service, and the value is `"no"`.** The three
controllers inherit `&ctrl` (`:78-101`), which contains `build:`, `environment:`,
`volumes:` and `depends_on:` and no `restart:`. **Docker Compose's default
restart policy is `no`**, so an exited controller stays exited.

### How to measure it, if the campaign ever wants to

**Not a compose scenario.** A Postgres-backed unit test in `internal/store`:
open one test database, construct two `Migrate` instances over two *different*
embedded migration sets (one truncated at 16, one full at 17), run the truncated
one's `Migrate()` to completion, and — with a hook or a deliberately slow
seventeenth migration — call `verifySchema` on the first handle while the second
instance is inside `runMigrations`. `verify_test.go` already builds and probes
throwaway schemas (`:63`, `:69`, `:83`, `:88`, `:102`), so the harness exists.
That test would answer "does it fire" and, with timing, "how wide"; a compose
rig would answer neither reliably and costs two cold controller image builds.

---

## Step 6 — refuted scenarios, recorded as RESULTS

**These are findings about the product, not gaps in the wave.** Each cost a read
and each closes a question a future wave would otherwise pay a rig to answer.

### Result 1 — concurrent migration startup is **safe by construction**, and the construction is nameable

Three replicas booting simultaneously against one database **cannot** race in
`Up()`. `migrate.go:265-266` takes `m.lock()` as the first statement of `Up()`
and releases only through `m.unlockErr` at `:283`, so the lock spans version
read, dirty check, `readUp` and every applied migration.
`database/postgres/postgres.go:241` implements that lock as
`SELECT pg_advisory_lock($1)` with an id derived at `:235` from
`(DatabaseName, migrationsSchemaName, migrationsTableName)` — **identical on
every replica, because all three come from the same DSN and the same driver
defaults** — and the driver's own comment at `:240` records that it blocks
indefinitely rather than failing. Late replicas therefore **queue**; they do not
collide, and they do not error.

**This is a positive result and it is worth having in writing**: the campaign
has repeatedly found that unified-cd's HA safety comes from Postgres primitives
rather than from application logic (W0-1's advisory-lock leader election is the
counter-example where a *pooler* broke exactly that assumption,
`FINDINGS.md:22`), and this is the same primitive used correctly. **The
corollary that makes it non-trivial: it is also why the Step 5 race exists at
all** — the lock is scoped to `Up()` precisely because `Up()` is the dangerous
part, and `verifySchema` was appended after it without the scope being widened.

### Result 2 — the sentinel guard **cannot** fire for any tag pair this repository can assemble, verified by enumerating every tag

Not "does not fire for the tags the plan checked" — **does not fire for any of
them**, established by extracting `schemaSentinels` from **every** tag
(`sentinels-per-tag.txt`):

| Tag | sentinels | note |
|---|---|---|
| `v0.0.1`, `v0.0.2` | — | **`internal/store/verify.go` does not exist**; there was no guard to fire |
| `v0.0.3`, `v0.1.0` | 1-8 | |
| `v0.2.0` | 1-9 | |
| `v0.2.1` | 1-10 | |
| `v0.3.0` | 1-12 | confirms the plan's figure |
| `v0.4.0` | 1-16 | |
| HEAD | 1-17 | |
| `backup/regression-review-pre-rebase` | — | the ninth and last name `git tag` returns; **`internal/store/verify.go` does not exist at that ref either**, so the empty row is genuine |

**The last row is named rather than omitted**, because "every tag" is the
load-bearing word in this result: `git tag` returns nine names, an earlier
version of this sweep listed eight, and a silently skipped ref makes the claim
unverifiable even when — as here — it changes nothing.

**Every list is a strict prefix of the next.** No entry is ever removed,
renumbered, or repointed at a different object. And the objects themselves
survive: of the 17 sentinel objects at HEAD, the ones an older binary would
probe — `runs`, `pats.role`, `app_sources.managed_resources`, `audit_logs`,
`step_reports.variant`, `app_sources.sync_status`, `step_reports.child_run_id`,
`runs_job_name_created_idx`, `agents.capabilities`, `sidecar_status`,
`runs_terminal_updated_idx`, `run_log_archives.line_count` — **are touched by
none of migrations 013-017.** So a v0.3.0 binary booting against a HEAD schema
reads `version=17` and probes all twelve of its own sentinels — the skip guard
at `verify.go:70` is `if s.version > version { continue }`, which exists to skip
sentinels for migrations **not yet applied** and therefore skips nothing when the
database is *ahead* of the binary — finds all twelve, and **`verifySchema`
returns nil.**

**The result is the important part: `verifySchema` passing is not the same as
the binary working.** It passes, and then the binary dies on its first query
against a dropped column. Which leads directly to:

### Result 3 — a v0.3.0 controller alongside HEAD was **deliberately not built**, and reading settles what building would have shown

The plan's rig reason (`:134-136`) is that v0.3.0 predates
`UNIFIED_CONTROLLER_KEY_FILE` and so cannot use `test/ha` unmodified. **The
stronger reason is that it would fail on any rig** (CORRECTION 5).
`v0.3.0:cmd/controller/main.go:215` calls `EnsureControllerKey` on the boot path;
its SQL names `controller_key_hex` four times
(`v0.3.0:internal/store/postgres.go:1876`, `:1879`, `:1881`, `:1883`);
`015_secrets_v2.up.sql:11` drops that column. **Boot-time `42703`, guaranteed,
zero window.**

And it is not the only one. `v030-dropped-column-refs.txt` enumerates v0.3.0's
`internal/store` references to columns 015/016 remove:

| Dropped column | v0.3.0 sites | Reached by |
|---|---|---|
| `controller_settings.controller_key_hex` | `postgres.go:1876`, `:1879`, `:1881`, `:1883` | **boot** (`cmd/controller/main.go:215`) |
| `sessions.refresh_token` | `postgres.go:1769`, `:1771`, `:1783`, `:1800` | every OIDC login, lookup and refresh |
| `secrets.scope` / `.scope_ref` | `postgres.go:1539`, `:1541`, `:1545`, `:1557`, `:1558`, `:1569`, `:1570`, `:1589` | every secret set/get/list/delete, **including `ON CONFLICT (name, scope, scope_ref)`** at `:1541`, which is Class E as well as Class A |

**So the mixed-version pair the design doc imagined is not degraded, it is
impossible** — and the failure is at boot rather than under load. **But the pair
that is impossible is N-2 beside N, NOT N-1 beside N, and the distinction is
load-bearing for the finding's blast radius.** `git ls-tree v0.4.0
internal/store/migrations/` returns **001-016** (matching the 1-16 sentinel row
above), and `017_run_detached.up.sql` is purely additive — one
`ADD COLUMN detached BOOLEAN NOT NULL DEFAULT false` plus a partial index — so
**a v0.4.0 controller boots and serves against a HEAD schema**. The breakage
needs an embedded set that stops **below 015**, and the newest release meeting
that is **v0.3.0, which is N-2** (`git tag` orders the releases `v0.0.1`,
`v0.0.2`, `v0.0.3`, `v0.1.0`, `v0.2.0`, `v0.2.1`, `v0.3.0`, `v0.4.0`). **That is
a cleaner answer than a rig would have produced**, and it is the concrete
instance of the violation filed below: `docs/operations.md:191` tells an
operator that this configuration works.

**Recorded as a decision, with its cost.** Two cold controller images (npm ci +
vite + Go, no layer sharing, `docker-compose.ha.yaml:79-81` builds from source
and `:103-104` alias the same build) were not spent to observe a `42703` that
four `git grep`s establish.

---

## Findings filed

| # | `FINDINGS.md` | Kind | Severity | Subject |
|---|---|---|---|---|
| 1 | `:2959` | **violation** (`docs/operations.md:191`) | **major** | 5 of 17 embedded migrations are backward-incompatible, so the documented rolling-upgrade guarantee's stated ground is false and an **N-2** (pre-v0.4.0) controller cannot boot; carries the secret/session-destruction band argument and the unenforced-sentinel-rule limb in Notes |
| 2 | `:2979` | **observation** | minor (observation) | `verifySchema` runs outside the migration advisory lock; a mixed-version boot can `os.Exit(1)` a replica with no retry and no restart policy |
| 3 | `:2993` | **observation** | minor (observation) | the three refuted scenarios, recorded as results |

**Tally: 1 violation (major) + 2 observations.**

**Entry 2 is an observation, not a violation, and the reason is worth stating
because the plan called it "one real mixed-version-only availability defect"
(`plan:73`).** Under the campaign's classification rule (`FINDINGS.md:479`)
a violation needs an invariant's own text or a *published* promise to
contradict, and three candidates were checked and all three fail: **(a)**
`docs/high-availability.md:279` ("one-at-a-time rolling deploys with no
downtime") describes the **SIGTERM drain sequence** at `:271-278`, not startup,
and with three replicas the loss of one is not service downtime — its own text
does not cover this. **(b)** `docs/troubleshooting.md:1204` **anticipates** the
case and prescribes "Restart the controller first" — an instruction to the
operator, which the operator can carry out; a remedy that requires manual action
is not a promise the product breaks. **(c)** No invariant applies: I1 is about
run accounting and no run is involved; I5 (bounded recovery) is the closest and
recovery *is* bounded — by an operator restart the docs name. **The defect is
real and the entry is filed at the top of the minor band; what it is not is a
contradiction of anything published.**

**Entry 1 carries a SECOND subject — the unconditional, undocumented destruction
of every secret and every session on upgrade — and the decision to keep it
inside entry 1 rather than split it out is recorded here because it is a
judgement, not a mechanical step.** The review's complaint was sound: entry 1's
`Severity` line disposed only of `:6`'s *silent corruption* disjunct while the
destruction subject inherited its band from the doc-sentence subject without
ever being argued. **Both disjuncts (and `security`) are now disposed of
separately at `FINDINGS.md:2961`, in place. The split was weighed and
declined**, on three grounds. **(1) The destruction has no contract limb of its
own.** Under `FINDINGS.md:479` a violation needs a *published* promise to
contradict, and the only published text the destruction contradicts is
`docs/operations.md:191`'s "additive columns/tables" — entry 1's existing limb.
A split entry would either file one contract twice or carry no limb at all.
**(2) Its subject is not independent**: the destruction is the single most
damaging instance of exactly what the parenthetical denies, and its evidence
(`015:11`, `015:13`, `016:13`, `016:7-12`, the 1-hit `docs/` survey) is one body
that would have to be duplicated across two entries. **(3) Internal
consistency**: §Step 4 above declines to split the sentinel-rule limb on the
*same* not-an-independent-subject test, and applying that test one way there and
the other way here inside one entry would be incoherent. **The substantive
change is therefore in the argument, not the structure — and the previously
unlabelled inference about operator practice ("secret values live in the
operator's own source of truth") is now labelled INFERRED and carries no
weight; the load is taken by `016:9-11`, which states the product's own premise
verbatim.**

**All three are labelled `NOT EXECUTED (code-read only)` on their `Repro`
lines**, per `FINDINGS.md:1864`'s precedent.

## Teardown

Nothing to tear down. No process was started, no container was built, no
database was created. **No background process was left running because none was
started.**
