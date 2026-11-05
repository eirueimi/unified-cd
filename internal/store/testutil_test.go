package store

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/store/internal/pgtest"
)

func migrateTemplateDSN(dsn string) error { return (&Postgres{}).Migrate(dsn) }

// NewTestPostgres returns a Postgres backed by a fresh, fully-migrated database
// cloned from the shared test container. Tests in other packages use
// storetest.NewTestPostgres, which wraps the same shared container.
func NewTestPostgres(t *testing.T) *Postgres {
	t.Helper()
	dsn := pgtest.NewDatabase(t, migrateTemplateDSN)
	pg, err := NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	t.Cleanup(pg.Close) // runs before the database drop (LIFO): release pooled connections first
	return pg
}

// NewEmptyTestDatabase creates a brand-new, completely unmigrated database and
// returns its DSN. Upgrade tests use it with (*Postgres).MigrateTo.
func NewEmptyTestDatabase(t *testing.T) string {
	t.Helper()
	return pgtest.NewEmptyDatabase(t, migrateTemplateDSN)
}
