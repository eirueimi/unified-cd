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
