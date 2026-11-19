package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLILoginCode_CreateConsumeOnce(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	id, err := pg.CreateCLILoginCode(ctx, "hash-1", "chal-1", []byte("dek"), []byte("ct"), time.Now().Add(2*time.Minute))
	require.NoError(t, err)
	require.NotEmpty(t, id)

	got, err := pg.ConsumeCLILoginCode(ctx, "hash-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "chal-1", got.Challenge)
	assert.Equal(t, []byte("dek"), got.DEK)
	assert.Equal(t, []byte("ct"), got.CT)

	again, err := pg.ConsumeCLILoginCode(ctx, "hash-1")
	require.NoError(t, err)
	assert.Nil(t, again, "a code is single use")
}

func TestCLILoginCode_UnknownAndExpired(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	got, err := pg.ConsumeCLILoginCode(ctx, "nope")
	require.NoError(t, err)
	assert.Nil(t, got)

	_, err = pg.CreateCLILoginCode(ctx, "old", "c", []byte("d"), []byte("t"), time.Now().Add(-time.Second))
	require.NoError(t, err)
	got, err = pg.ConsumeCLILoginCode(ctx, "old")
	require.NoError(t, err)
	assert.Nil(t, got, "an expired code is not redeemable")
}

func TestCLILoginCode_CreateDeletesExpiredRows(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.CreateCLILoginCode(ctx, "stale", "c", []byte("d"), []byte("t"), time.Now().Add(-time.Minute))
	require.NoError(t, err)
	_, err = pg.CreateCLILoginCode(ctx, "fresh", "c", []byte("d"), []byte("t"), time.Now().Add(time.Minute))
	require.NoError(t, err)
	var n int
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM cli_login_codes WHERE code_hash = 'stale'`).Scan(&n))
	assert.Zero(t, n)
}

func TestCLILoginCode_SetPayload(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	id, err := pg.CreateCLILoginCode(ctx, "h", "c", []byte{}, []byte{}, time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.NoError(t, pg.SetCLILoginCodePayload(ctx, id, []byte("dek2"), []byte("ct2")))
	got, err := pg.ConsumeCLILoginCode(ctx, "h")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []byte("ct2"), got.CT)
}

func TestOIDCState_CarriesCLIFields(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	st, err := pg.CreateCLIOIDCState(ctx, "s-cli", "http://127.0.0.1:5555/callback", "challenge", "confirm-hash", time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:5555/callback", st.CLIRedirect)
	assert.Equal(t, "confirm-hash", st.CLIConfirmHash)

	// GetCLIOIDCState reads without consuming.
	peek, err := pg.GetCLIOIDCState(ctx, "s-cli")
	require.NoError(t, err)
	require.NotNil(t, peek)
	assert.Equal(t, "confirm-hash", peek.CLIConfirmHash)
	assert.Equal(t, "challenge", peek.CLIChallenge)
	got, err := pg.GetAndDeleteOIDCState(ctx, "s-cli")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "challenge", got.CLIChallenge)
	assert.Equal(t, "http://127.0.0.1:5555/callback", got.CLIRedirect)
	assert.Equal(t, "confirm-hash", got.CLIConfirmHash)
	gone, err := pg.GetCLIOIDCState(ctx, "s-cli")
	require.NoError(t, err)
	assert.Nil(t, gone, "consumed state is gone")

	_, err = pg.CreateOIDCState(ctx, "s-web", "/ui/", time.Now().Add(time.Minute))
	require.NoError(t, err)
	web, err := pg.GetAndDeleteOIDCState(ctx, "s-web")
	require.NoError(t, err)
	require.NotNil(t, web)
	assert.Empty(t, web.CLIRedirect)
	assert.Empty(t, web.CLIChallenge)
	assert.Empty(t, web.CLIConfirmHash)

	// Neither a web state nor an expired CLI state is readable.
	_, err = pg.CreateOIDCState(ctx, "s-web2", "/ui/", time.Now().Add(time.Minute))
	require.NoError(t, err)
	nilWeb, err := pg.GetCLIOIDCState(ctx, "s-web2")
	require.NoError(t, err)
	assert.Nil(t, nilWeb)
	_, err = pg.CreateCLIOIDCState(ctx, "s-old", "http://127.0.0.1:5555/callback", "c", "h", time.Now().Add(-time.Second))
	require.NoError(t, err)
	old, err := pg.GetCLIOIDCState(ctx, "s-old")
	require.NoError(t, err)
	assert.Nil(t, old)
}

func TestMigration032CLILoginCodesUpDown(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	down, err := migrationsFS.ReadFile("migrations/032_cli_login_codes.down.sql")
	require.NoError(t, err)
	up, err := migrationsFS.ReadFile("migrations/032_cli_login_codes.up.sql")
	require.NoError(t, err)
	present := func() (tables, cols int) {
		require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'cli_login_codes'`).Scan(&tables))
		require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'oidc_states' AND column_name IN ('cli_redirect','cli_challenge','cli_confirm_hash')`).Scan(&cols))
		return
	}
	tb, cl := present()
	require.Equal(t, 1, tb)
	require.Equal(t, 3, cl)
	_, err = pg.pool.Exec(ctx, string(down))
	require.NoError(t, err)
	tb, cl = present()
	require.Equal(t, 0, tb)
	require.Equal(t, 0, cl)
	_, err = pg.pool.Exec(ctx, string(up))
	require.NoError(t, err)
	tb, cl = present()
	require.Equal(t, 1, tb)
	require.Equal(t, 3, cl)
}
