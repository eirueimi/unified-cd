package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// sentinel names one schema object a migration creates, or - for a migration
// whose effect is a removal - one it destroys. verifySchema probes it to
// confirm the migration's effects actually happened, catching databases
// whose schema_migrations.version matched an older file numbering (branch
// renumbering) and silently skipped the current file's contents.
type sentinel struct {
	version     int
	migration   string
	table       string
	column      string // probe a column of table exists (checked when index and indexAbsent are both empty)
	index       string // probe an index on table exists; takes precedence over column
	indexAbsent string // probe an index on table does NOT exist (the migration's effect was to drop it); takes precedence over index and column
}

// schemaSentinels must contain exactly one entry per migrations/*.up.sql,
// in version order. TestSchemaSentinelsCoverAllMigrations enforces this:
// adding a migration without a sentinel fails the suite.
//
// A later migration must never drop or rename a sentinel object; if one
// must, the sentinel entry has to be changed in the same commit, or older
// binaries verifying a newer database will report false drift. Symmetrically,
// a later migration must never recreate an object an indexAbsent sentinel
// asserts is gone, for the same reason.
var schemaSentinels = []sentinel{
	{version: 1, migration: "001_init", table: "runs"},
	{version: 2, migration: "002_add_role", table: "pats", column: "role"},
	{version: 3, migration: "003_appsource_managed_resources", table: "app_sources", column: "managed_resources"},
	{version: 4, migration: "004_audit_logs", table: "audit_logs"},
	{version: 5, migration: "005_matrix_variant", table: "step_reports", column: "variant"},
	{version: 6, migration: "006_appsource_sync_status", table: "app_sources", column: "sync_status"},
	{version: 7, migration: "007_step_call_link", table: "step_reports", column: "child_run_id"},
	{version: 8, migration: "008_run_indexes", table: "runs", index: "runs_job_name_created_idx"},
	{version: 9, migration: "009_agent_capabilities", table: "agents", column: "capabilities"},
	{version: 10, migration: "010_sidecar_status", table: "sidecar_status"},
	{version: 11, migration: "011_runs_terminal_updated_idx", table: "runs", index: "runs_terminal_updated_idx"},
	{version: 12, migration: "012_run_log_archives_trimmed_at", table: "run_log_archives", column: "line_count"},
	{version: 13, migration: "013_agent_identity_auth", table: "agent_credentials", column: "token_hash"},
	{version: 14, migration: "014_agent_enrollment_policies", table: "agent_enrollment_policies"},
	{version: 15, migration: "015_secrets_v2", table: "sessions", column: "refresh_token_dek"},
	{version: 16, migration: "016_drop_secret_scope", table: "secrets", index: "secrets_name_key"},
	{version: 17, migration: "017_run_detached", table: "runs", column: "detached"},
	{version: 18, migration: "018_vars", table: "vars"},
	{version: 19, migration: "019_run_display_name", table: "runs", column: "display_name"},
	{version: 20, migration: "020_run_pod_bindings", table: "run_pod_bindings"},
	{version: 21, migration: "021_drop_redundant_logs_index", table: "logs", indexAbsent: "logs_run_idx"},
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
				"manually and clear the flag (golang-migrate 'force'), see docs/troubleshooting/controller-and-database.md (\"Schema drift\")",
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
		var ok bool
		switch {
		case s.indexAbsent != "":
			err = db.QueryRow(
				`SELECT NOT EXISTS (SELECT 1 FROM pg_indexes
				 WHERE schemaname = 'public' AND tablename = $1 AND indexname = $2)`,
				s.table, s.indexAbsent).Scan(&ok)
		case s.index != "":
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM pg_indexes
				 WHERE schemaname = 'public' AND tablename = $1 AND indexname = $2)`,
				s.table, s.index).Scan(&ok)
		case s.column != "":
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM information_schema.columns
				 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
				s.table, s.column).Scan(&ok)
		default:
			err = db.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM information_schema.tables
				 WHERE table_schema = 'public' AND table_name = $1)`,
				s.table).Scan(&ok)
		}
		if err != nil {
			return fmt.Errorf("schema verification probe for %s: %w", s.migration, err)
		}
		if !ok {
			if s.indexAbsent != "" {
				return fmt.Errorf(
					"schema drift: schema_migrations.version=%d claims %s is applied, but index %s still exists; "+
						"migration files were likely renumbered after this database was migrated - "+
						"see docs/troubleshooting/controller-and-database.md (\"Schema drift\") for recovery",
					version, s.migration, s.indexAbsent)
			}
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
					"see docs/troubleshooting/controller-and-database.md (\"Schema drift\") for recovery",
				version, s.migration, obj)
		}
	}
	return nil
}
