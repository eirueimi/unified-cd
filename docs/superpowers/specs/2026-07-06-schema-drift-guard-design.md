# Schema Drift Guard — Design

Date: 2026-07-06
Status: Approved

## Background

On 2026-07-05, parallel feature branches renumbered migrations
(003→004, 003→005, …). A database migrated during an interim numbering ended
up with `schema_migrations.version = 7` while missing the columns of the
current `007_step_call_link.up.sql` (`step_reports.child_run_id` /
`call_job_name`). golang-migrate compares only version numbers, so it silently
skipped the file; the failure surfaced hours later as a 500 on `/steps`. The
dev database was hand-fixed on 2026-07-06.

## Goal

Catch "version says applied, schema says otherwise" drift at controller
startup — fail fast with a message that names the drifted migration and points
at a recovery runbook — instead of failing at query time.

## Non-goals

- Detecting *content* edits to already-applied migrations (checksum tracking).
  Out of scope; sentinel presence is enough for the renumbering failure class.
- Auto-repair. The guard only detects and reports; a human applies the fix
  (the runbook covers it).
- Guarding the k8s agent or CLI — only the controller runs `Migrate`.

## Design

### Sentinel table (`internal/store/verify.go`)

One entry per migration file, keyed by version, each naming a cheap
`information_schema` existence probe for an object that migration created:

| Version | Migration | Sentinel |
|---|---|---|
| 1 | `001_init` | table `runs` exists |
| 2 | `002_add_role` | column `pats.role` |
| 3 | `003_appsource_managed_resources` | column `app_sources.managed_resources` |
| 4 | `004_audit_logs` | table `audit_logs` |
| 5 | `005_matrix_variant` | column `step_reports.variant` |
| 6 | `006_appsource_sync_status` | column `app_sources.sync_status` |
| 7 | `007_step_call_link` | column `step_reports.child_run_id` |

`verifySchema(db)` reads `schema_migrations.version` and checks every sentinel
with `version <= applied`. A missing sentinel produces an error like:

```
schema drift: schema_migrations.version=7 claims 007_step_call_link is applied,
but step_reports.child_run_id does not exist. This usually means migration
files were renumbered after this database was migrated. See
docs/troubleshooting.md ("Schema drift") for recovery.
```

### Wiring

`(*Postgres).Migrate` calls `verifySchema` after a successful
`m.Up()` (and also when `Up` returns `ErrNoChange`), using the same stdlib DB
handle. Both existing callers are covered with no signature change:
`cmd/controller/main.go` (exits on error, already logs `migrate` failures) and
`store.NewTestPostgres`'s template setup (drift in test setups fails loudly).

### Forcing future sentinel entries

A unit test asserts `len(schemaSentinels) == count of embedded
migrations/*.up.sql`. Adding migration 008 without a sentinel fails the suite
with a message telling the author to add one.

### Drift simulation test

Postgres-backed test: clone a migrated DB (`NewTestPostgres`), `ALTER TABLE
step_reports DROP COLUMN child_run_id`, run `verifySchema`, assert the error
mentions `007_step_call_link` and the missing column. A happy-path assertion
(fresh migrated DB verifies clean) rides along.

## Error handling

- Sentinel probe query errors (connection loss etc.) are returned as-is,
  distinct from drift errors — the message only claims drift when the probe
  ran and found the object missing.
- An empty/zero `schema_migrations` (fresh DB before first migration) skips
  verification — nothing is claimed applied.
- `dirty=true` in schema_migrations is reported as a distinct error (crashed
  or in-flight migration), never as drift.

## Documentation

`docs/troubleshooting.md` gains a "Schema drift (migration renumbering)"
section: symptom (startup error naming a migration), cause (renumbering),
diagnosis (compare `schema_migrations.version` against the sentinel table),
recovery (apply the missing `.up.sql` statements manually, leave the version
number as-is when it already matches the file count, verify by restarting).
