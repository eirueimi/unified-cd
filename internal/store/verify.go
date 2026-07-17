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
	column    string // probe a column of table (when index is empty)
	index     string // probe an index on table; takes precedence over column
}

// schemaSentinels must contain exactly one entry per migrations/*.up.sql,
// in version order. TestSchemaSentinelsCoverAllMigrations enforces this:
// adding a migration without a sentinel fails the suite.
//
// A later migration must never drop or rename a sentinel object; if one
// must, the sentinel entry has to be changed in the same commit, or older
// binaries verifying a newer database will report false drift.
var schemaSentinels = []sentinel{
	{1, "001_init", "runs", "", ""},
	{2, "002_add_role", "pats", "role", ""},
	{3, "003_appsource_managed_resources", "app_sources", "managed_resources", ""},
	{4, "004_audit_logs", "audit_logs", "", ""},
	{5, "005_matrix_variant", "step_reports", "variant", ""},
	{6, "006_appsource_sync_status", "app_sources", "sync_status", ""},
	{7, "007_step_call_link", "step_reports", "child_run_id", ""},
	{8, "008_run_indexes", "runs", "", "runs_job_name_created_idx"},
	{9, "009_agent_capabilities", "agents", "capabilities", ""},
	{10, "010_sidecar_status", "sidecar_status", "", ""},
	{11, "011_runs_terminal_updated_idx", "runs", "", "runs_terminal_updated_idx"},
	{12, "012_run_log_archives_trimmed_at", "run_log_archives", "line_count", ""},
	{13, "013_agent_identity_auth", "agent_credentials", "token_hash", ""},
	{14, "014_agent_enrollment_policies", "agent_enrollment_policies", "", ""},
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
	if dirty {
		return fmt.Errorf(
			"schema verification: schema_migrations is dirty at version %d - either a previous migration attempt crashed midway "+
				"or another replica's migration is currently in flight; if this error persists across restarts, repair the schema "+
				"manually and clear the flag (golang-migrate 'force'), see docs/troubleshooting.md (\"Schema drift\")",
			version)
	}
	for _, s := range schemaSentinels {
		if s.version > version {
			continue
		}
		// The migrations create all objects schema-qualified in public
		// (see internal/store/migrations/*.up.sql), so verification pins
		// that schema deliberately - using current_schema() would
		// false-positive under a custom search_path (e.g. "app, public")
		// and brick startup fleet-wide.
		var exists bool
		switch {
		case s.index != "":
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM pg_indexes
				 WHERE schemaname = 'public' AND tablename = $1 AND indexname = $2)`,
				s.table, s.index).Scan(&exists)
		case s.column != "":
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM information_schema.columns
				 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
				s.table, s.column).Scan(&exists)
		default:
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM information_schema.tables
				 WHERE table_schema = 'public' AND table_name = $1)`,
				s.table).Scan(&exists)
		}
		if err != nil {
			return fmt.Errorf("schema verification probe for %s: %w", s.migration, err)
		}
		if !exists {
			obj := s.table
			switch {
			case s.index != "":
				obj = "index " + s.index
			case s.column != "":
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
