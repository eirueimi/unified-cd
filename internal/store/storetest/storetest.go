// Package storetest provides test helpers for packages that need a real
// Postgres-backed store.Postgres. It must only be imported from _test.go files
// so that Docker test dependencies never reach production binaries.
package storetest

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/eirueimi/unified-cd/internal/store/internal/pgtest"
)

var dsns sync.Map // *store.Postgres -> DSN

func migrate(dsn string) error { return (&store.Postgres{}).Migrate(dsn) }

// NewTestPostgres returns a store.Postgres backed by a fresh, fully-migrated
// database cloned from the shared test container. The database is dropped on cleanup.
func NewTestPostgres(t *testing.T) *store.Postgres {
	t.Helper()
	dsn := pgtest.NewDatabase(t, migrate)
	pg, err := store.NewPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	dsns.Store(pg, dsn)
	t.Cleanup(func() {
		dsns.Delete(pg)
		pg.Close() // release pooled connections before the database is dropped
	})
	return pg
}

// NewEmptyTestDatabase creates a brand-new, completely unmigrated database and
// returns its DSN. Upgrade tests use it with (*store.Postgres).MigrateTo.
func NewEmptyTestDatabase(t *testing.T) string {
	t.Helper()
	return pgtest.NewEmptyDatabase(t, migrate)
}

func exec(ctx context.Context, pg *store.Postgres, query string, args ...any) error {
	v, ok := dsns.Load(pg)
	if !ok {
		panic("storetest: Postgres was not created by storetest.NewTestPostgres")
	}
	db, err := sql.Open("pgx", v.(string))
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, query, args...)
	return err
}

// BackdateRunCreatedAt sets a run's created_at to age before now. It uses a side
// connection because store.Postgres keeps its pool unexported.
func BackdateRunCreatedAt(ctx context.Context, pg *store.Postgres, runID string, age time.Duration) error {
	return exec(ctx, pg, `UPDATE runs SET created_at = NOW() - ($1 * interval '1 second') WHERE id = $2`, age.Seconds(), runID)
}

// BackdateRunClaimedAt sets a run's claimed_at to age before now.
func BackdateRunClaimedAt(ctx context.Context, pg *store.Postgres, runID string, age time.Duration) error {
	return exec(ctx, pg, `UPDATE runs SET claimed_at = NOW() - ($1 * interval '1 second') WHERE id = $2`, age.Seconds(), runID)
}
