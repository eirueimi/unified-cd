package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration036Backfill(t *testing.T) {
	dsn := NewEmptyTestDatabase(t)
	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 35))
	pg, err := NewPostgres(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	ctx := context.Background()

	// Owner PAT and session for s1; the session supplies email and claims.
	_, err = pg.pool.Exec(ctx, `INSERT INTO pats(name, token_hash, role, owner_sub, owner_email, owner_claims)
		VALUES ('owned', 'h-owned', '', 's1', 'old@example.com', '{old}')`)
	require.NoError(t, err)
	_, err = pg.pool.Exec(ctx, `INSERT INTO sessions(token_hash, sub, email, role, claims, refresh_token_dek, refresh_token_ct, expires_at)
		VALUES ('t-s1', 's1', 'new@example.com', '', '{g1,g2}', ''::bytea, ''::bytea, now() + interval '1 hour')`)
	require.NoError(t, err)
	// Ownerless PATs: role-only admin, enroll, and the bootstrap row.
	_, err = pg.pool.Exec(ctx, `INSERT INTO pats(name, token_hash, role) VALUES ('roleonly', 'h-role', 'admin')`)
	require.NoError(t, err)
	_, err = pg.pool.Exec(ctx, `INSERT INTO pats(name, token_hash, role, permissions, created_by)
		VALUES ('enroll', 'h-enroll', '', '{agent.enroll}', 'break-glass')`)
	require.NoError(t, err)
	_, err = pg.pool.Exec(ctx, `INSERT INTO pats(name, token_hash, role) VALUES ('env:UNIFIED_TOKEN', 'h-env', 'admin')`)
	require.NoError(t, err)
	// Session with NULL claims for s2.
	_, err = pg.pool.Exec(ctx, `INSERT INTO sessions(token_hash, sub, email, role, refresh_token_dek, refresh_token_ct, expires_at)
		VALUES ('t-s2', 's2', 's2@example.com', 'admin', ''::bytea, ''::bytea, now() + interval '1 hour')`)
	require.NoError(t, err)

	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 36))

	var humans int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE kind = 'human'`).Scan(&humans))
	assert.Equal(t, 1, humans)

	acc, err := pg.GetHumanAccountBySub(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, acc)
	assert.Equal(t, "new@example.com", acc.Email)
	assert.Equal(t, []string{"g1", "g2"}, acc.Claims)

	var patAcc, sessAcc string
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT account_id::text FROM pats WHERE name = 'owned'`).Scan(&patAcc))
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT account_id::text FROM sessions WHERE sub = 's1'`).Scan(&sessAcc))
	assert.Equal(t, acc.ID, patAcc)
	assert.Equal(t, acc.ID, sessAcc)

	var pats int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM pats`).Scan(&pats))
	assert.Equal(t, 1, pats, "ownerless PATs are removed")
	var audits int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE actor = 'migration' AND action = 'token.delete'`).Scan(&audits))
	assert.Equal(t, 3, audits)
	var reason string
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT details->>'reason' FROM audit_logs WHERE details->>'name' = 'enroll'`).Scan(&reason))
	assert.Equal(t, "ownerless PAT removed by the internal-accounts migration", reason)

	var s2 int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE sub = 's2'`).Scan(&s2))
	assert.Equal(t, 0, s2)
	s2acc, err := pg.GetHumanAccountBySub(ctx, "s2")
	require.NoError(t, err)
	assert.Nil(t, s2acc)

	var admin int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE kind = 'service' AND name = 'admin' AND disabled_at IS NULL`).Scan(&admin))
	assert.Equal(t, 1, admin)

	var rows int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM controller_settings WHERE id = 1`).Scan(&rows))
	assert.Equal(t, 1, rows)
}
