# Schema Drift Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fail controller startup with a precise, actionable error when `schema_migrations.version` claims a migration is applied but its schema objects are missing (migration-renumbering drift).

**Architecture:** A sentinel table in `internal/store/verify.go` maps each migration version to one `information_schema` existence probe. `(*Postgres).Migrate` runs the verification right after a successful `Up()` on its existing `*sql.DB` handle, so both callers (controller startup, test-template setup) are covered without signature changes. A count test pins sentinels to the embedded migration files so future migrations cannot ship without one.

**Tech Stack:** Go, database/sql over pgx stdlib driver, golang-migrate (unchanged), testify, real-Postgres tests via `store.NewTestPostgres`.

Spec: `docs/superpowers/specs/2026-07-06-schema-drift-guard-design.md`

## Global Constraints

- English only (code, comments, docs, commit messages).
- No `store.Store` interface changes; no `Migrate` signature change.
- Drift error message must name the migration, the missing object, the recorded version, the renumbering cause, and point to `docs/troubleshooting.md` ("Schema drift").
- Probe errors (connection loss etc.) must be returned distinctly — never phrased as drift.
- Fresh DB (no `schema_migrations` row) skips verification.
- Tests need Docker; `gofmt -w` all touched Go files.

---

### Task 1: sentinel verification wired into Migrate, with tests and runbook

**Files:**
- Create: `internal/store/verify.go`
- Create: `internal/store/verify_test.go`
- Modify: `internal/store/postgres.go` (Migrate, lines 43-66)
- Modify: `docs/troubleshooting.md` (append a section)

**Interfaces:**
- Consumes: `migrationsFS` (embedded FS in postgres.go), existing `Migrate`, `NewTestPostgres(t)`.
- Produces: `schemaSentinels []sentinel` and `verifySchema(db *sql.DB) error` (package-private; nothing outside `internal/store` depends on them).

- [ ] **Step 1: Write the failing tests**

`internal/store/verify_test.go`:

```go
package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every embedded up-migration must have exactly one sentinel, so drift
// detection cannot silently fall behind new migrations.
func TestSchemaSentinelsCoverAllMigrations(t *testing.T) {
	entries, err := migrationsFS.ReadDir("migrations")
	require.NoError(t, err)
	ups := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups++
		}
	}
	require.Equal(t, ups, len(schemaSentinels),
		"every migrations/*.up.sql needs a sentinel entry in internal/store/verify.go")
	for i, s := range schemaSentinels {
		assert.Equal(t, i+1, s.version, "sentinel versions must be 1..N in order")
		assert.NotEmpty(t, s.migration)
		assert.NotEmpty(t, s.table)
	}
}

// openSQL opens a database/sql handle onto the same clone DB the test Postgres
// uses (the pgx stdlib driver is registered by the stdlib import in postgres.go).
func openSQL(t *testing.T, pg *Postgres) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", pg.pool.Config().ConnString())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestVerifySchemaCleanAndDrifted(t *testing.T) {
	pg := NewTestPostgres(t)
	db := openSQL(t, pg)

	// Freshly migrated clone verifies clean.
	require.NoError(t, verifySchema(db))

	// Simulate renumbering drift: version still claims 007, column gone.
	_, err := db.Exec(`ALTER TABLE step_reports DROP COLUMN child_run_id`)
	require.NoError(t, err)

	err = verifySchema(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema drift")
	assert.Contains(t, err.Error(), "007_step_call_link")
	assert.Contains(t, err.Error(), "step_reports.child_run_id")
	assert.Contains(t, err.Error(), "docs/troubleshooting.md")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestSchemaSentinels|TestVerifySchema' -v`
Expected: FAIL — `undefined: schemaSentinels`, `undefined: verifySchema`.

- [ ] **Step 3: Implement verify.go**

`internal/store/verify.go`:

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// sentinel names one schema object a migration creates. verifySchema probes
// it to confirm the migration's effects actually exist, catching databases
// whose schema_migrations.version matched an older file numbering (branch
// renumbering) and silently skipped the current file's contents.
type sentinel struct {
	version   int
	migration string
	table     string
	column    string // empty = probe for the table itself
}

// schemaSentinels must contain exactly one entry per migrations/*.up.sql,
// in version order. TestSchemaSentinelsCoverAllMigrations enforces this:
// adding a migration without a sentinel fails the suite.
var schemaSentinels = []sentinel{
	{1, "001_init", "runs", ""},
	{2, "002_add_role", "pats", "role"},
	{3, "003_appsource_managed_resources", "app_sources", "managed_resources"},
	{4, "004_audit_logs", "audit_logs", ""},
	{5, "005_matrix_variant", "step_reports", "variant"},
	{6, "006_appsource_sync_status", "app_sources", "sync_status"},
	{7, "007_step_call_link", "step_reports", "child_run_id"},
}

// verifySchema cross-checks schema_migrations.version against the sentinel
// objects of every migration it claims applied. It runs after Migrate's Up()
// on the same database handle. A missing sentinel is reported as drift; probe
// failures are returned as plain errors and never phrased as drift.
func verifySchema(db *sql.DB) error {
	var version int
	var dirty bool
	err := db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // fresh database: nothing is claimed applied
	}
	if err != nil {
		return fmt.Errorf("schema verification: read schema_migrations: %w", err)
	}
	for _, s := range schemaSentinels {
		if s.version > version {
			continue
		}
		var exists bool
		if s.column == "" {
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM information_schema.tables
				 WHERE table_schema = current_schema() AND table_name = $1)`,
				s.table).Scan(&exists)
		} else {
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM information_schema.columns
				 WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2)`,
				s.table, s.column).Scan(&exists)
		}
		if err != nil {
			return fmt.Errorf("schema verification probe for %s: %w", s.migration, err)
		}
		if !exists {
			obj := s.table
			if s.column != "" {
				obj = s.table + "." + s.column
			}
			return fmt.Errorf(
				"schema drift: schema_migrations.version=%d claims %s is applied, but %s does not exist; "+
					"migration files were likely renumbered after this database was migrated - "+
					"see docs/troubleshooting.md (\"Schema drift\") for recovery",
				version, s.migration, obj)
		}
	}
	return nil
}
```

- [ ] **Step 4: Wire verification into Migrate**

In `internal/store/postgres.go`, change the end of `Migrate` from:

```go
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
```

to:

```go
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	// Guard against migration-renumbering drift: version numbers alone can
	// claim a file is applied when its schema objects never materialized.
	return verifySchema(db)
}
```

(The existing `db` handle stays open until the deferred Close; reusing it for
read-only probes is safe.)

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestSchemaSentinels|TestVerifySchema' -v`
Expected: PASS (2 tests).
Then the full store suite (Migrate now verifies on every template setup):
Run: `go test ./internal/store/ -count=1`
Expected: PASS.

- [ ] **Step 6: Append the runbook to docs/troubleshooting.md**

Append (match the file's existing heading level for top-level sections):

````markdown
## Schema drift (migration renumbering)

**Symptom:** the controller exits at startup with an error like:

```
schema drift: schema_migrations.version=7 claims 007_step_call_link is applied,
but step_reports.child_run_id does not exist; migration files were likely
renumbered after this database was migrated - see docs/troubleshooting.md
("Schema drift") for recovery
```

**Cause:** migration files were renumbered (typically when parallel branches
merged) after this database had already been migrated. golang-migrate compares
only version numbers, so a database whose recorded version matches an older
numbering silently skips the current file with that number.

**Diagnosis:** compare `SELECT version FROM schema_migrations;` against
`internal/store/migrations/`. The startup error names the first migration
whose objects are missing; later ones may be missing too.

**Recovery:**

1. For each missing migration (start with the one named in the error), apply
   its `.up.sql` statements manually, e.g.:

   ```
   psql "$DSN" -f internal/store/migrations/007_step_call_link.up.sql
   ```

2. Leave `schema_migrations.version` as-is when it already equals the highest
   migration number; only the schema objects were missing.
3. Restart the controller. Startup verification re-runs and confirms the fix.
````

- [ ] **Step 7: Full verification and commit**

Run: `gofmt -l internal/store/verify.go internal/store/verify_test.go` (expect no output), then `go build ./... && go test ./internal/store/ ./internal/controller/ -count=1`
Expected: PASS.

```bash
git add internal/store/verify.go internal/store/verify_test.go internal/store/postgres.go docs/troubleshooting.md
git commit -m "feat(store): verify schema sentinels after migrate to catch renumbering drift

schema_migrations.version alone can claim a migration is applied when
branch renumbering left its objects missing (seen 2026-07-06:
version=7 without step_reports.child_run_id). Migrate now probes one
sentinel object per applied migration and fails startup with a
recovery pointer; a count test forces a sentinel for every new
migration file."
```

---

## Self-check notes for the implementer

- `sql.Open("pgx", ...)` works because `postgres.go` imports
  `github.com/jackc/pgx/v5/stdlib`, which registers the `pgx` driver.
- If `docs/troubleshooting.md` uses a different top-level heading style,
  match it; the runbook text itself must keep the four subsections
  (Symptom/Cause/Diagnosis/Recovery).
