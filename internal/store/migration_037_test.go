package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration037(t *testing.T) {
	dsn := NewEmptyTestDatabase(t)
	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 37))
	pg, err := NewPostgres(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	ctx := context.Background()

	_, err = pg.pool.Exec(ctx, `INSERT INTO pats(name, token_hash, role) VALUES ('orphan', 'h-orphan', 'admin')`)
	require.Error(t, err, "a PAT without account_id must be rejected")
	_, err = pg.pool.Exec(ctx, `INSERT INTO sessions(token_hash, sub, email, role, refresh_token_dek, refresh_token_ct, expires_at)
		VALUES ('t-orphan', 's', 'e@example.com', '', ''::bytea, ''::bytea, now() + interval '1 hour')`)
	require.Error(t, err, "a session without account_id must be rejected")

	for _, col := range []string{"owner_sub", "owner_email", "owner_claims"} {
		var n int
		require.NoError(t, pg.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'pats' AND column_name = $1`, col).Scan(&n))
		assert.Zero(t, n, "pats.%s must be dropped", col)
	}
	var idx int
	require.NoError(t, pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'pats_owner_sub_idx'`).Scan(&idx))
	assert.Zero(t, idx)

	// Session identity columns stay: they are used for refresh.
	for _, col := range []string{"sub", "email", "claims"} {
		var n int
		require.NoError(t, pg.pool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'sessions' AND column_name = $1`, col).Scan(&n))
		assert.Equal(t, 1, n, "sessions.%s must stay", col)
	}
}
