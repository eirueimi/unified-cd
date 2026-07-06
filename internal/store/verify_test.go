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

func TestVerifySchemaReportsDirtyState(t *testing.T) {
	pg := NewTestPostgres(t)
	db := openSQL(t, pg)

	_, err := db.Exec(`UPDATE schema_migrations SET dirty = true`)
	require.NoError(t, err)

	err = verifySchema(db)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dirty")
	assert.NotContains(t, err.Error(), "claims") // drift message's unique verb; dirty message must not use drift phrasing
}
