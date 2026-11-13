package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func TestPostgres_PATCreateAndGet(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	hash := tokenHash("exc_testtoken123")
	pat, err := pg.CreatePAT(ctx, "my-token", hash, "admin", nil, "")
	require.NoError(t, err)
	assert.Equal(t, "my-token", pat.Name)
	assert.NotEmpty(t, pat.ID)

	got, err := pg.GetPATByHash(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, pat.ID, got.ID)

	_, err = pg.GetPATByHash(ctx, "wronghash")
	require.Error(t, err)
}

func TestPostgres_UpsertBootstrapPAT_CreatesThenRotates(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	hashA := tokenHash("tokenA")

	pat, err := pg.UpsertBootstrapPAT(ctx, "env:UNIFIED_TOKEN", hashA)
	require.NoError(t, err)
	assert.Equal(t, "env:UNIFIED_TOKEN", pat.Name)

	got, err := pg.GetPATByHash(ctx, hashA)
	require.NoError(t, err)
	assert.Equal(t, pat.ID, got.ID)

	hashB := tokenHash("tokenB")
	rotated, err := pg.UpsertBootstrapPAT(ctx, "env:UNIFIED_TOKEN", hashB)
	require.NoError(t, err)
	assert.Equal(t, pat.ID, rotated.ID, "rotating must update the existing row, not create a new one")

	_, err = pg.GetPATByHash(ctx, hashA)
	assert.Error(t, err, "old hash must no longer authenticate")

	got2, err := pg.GetPATByHash(ctx, hashB)
	require.NoError(t, err)
	assert.Equal(t, pat.ID, got2.ID)

	list, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1, "upserting twice must not create a duplicate row")
}

func TestPostgres_DeleteBootstrapPATByName(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	hash := tokenHash("tokenA")
	_, err := pg.UpsertBootstrapPAT(ctx, "env:UNIFIED_TOKEN", hash)
	require.NoError(t, err)

	require.NoError(t, pg.DeleteBootstrapPATByName(ctx, "env:UNIFIED_TOKEN"))

	_, err = pg.GetPATByHash(ctx, hash)
	assert.Error(t, err, "deleted bootstrap pat must no longer authenticate")

	// deleting a non-existent name is a no-op (no error).
	require.NoError(t, pg.DeleteBootstrapPATByName(ctx, "env:UNIFIED_TOKEN"))
}

func TestPostgres_PATListAndDelete(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.CreatePAT(ctx, "a", tokenHash("tokenA"), "admin", nil, "")
	_, _ = pg.CreatePAT(ctx, "b", tokenHash("tokenB"), "admin", nil, "")
	list, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
	require.NoError(t, pg.DeletePAT(ctx, list[0].ID))
	list2, _ := pg.ListPATs(ctx)
	assert.Len(t, list2, 1)
}

func TestPostgres_SyncBootstrapPAT(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	const name = "env:UNIFIED_TOKEN"
	real := tokenHash("real")

	_, err := pg.CreatePAT(ctx, name, tokenHash("attacker1"), "developer", nil, "")
	require.NoError(t, err)
	_, err = pg.CreatePAT(ctx, name, tokenHash("attacker2"), "admin", nil, "")
	require.NoError(t, err)
	_, err = pg.CreatePAT(ctx, "other", tokenHash("other"), "developer", nil, "")
	require.NoError(t, err)

	removed, err := pg.SyncBootstrapPAT(ctx, name, real)
	require.NoError(t, err)
	require.Len(t, removed, 2)
	assert.ElementsMatch(t, []string{"developer", "admin"}, []string{removed[0].Role, removed[1].Role})
	assert.NotEmpty(t, removed[0].ID)

	pats, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	var boot, other int
	for _, p := range pats {
		switch p.Name {
		case name:
			boot++
		case "other":
			other++
		}
	}
	assert.Equal(t, 1, boot, "exactly one bootstrap row")
	assert.Equal(t, 1, other, "unrelated rows untouched")
	got, err := pg.GetPATByHash(ctx, real)
	require.NoError(t, err)
	assert.Equal(t, "admin", got.Role)
	_, err = pg.GetPATByHash(ctx, tokenHash("attacker1"))
	require.Error(t, err)

	// Idempotent: nothing left to purge, the real row survives.
	removed, err = pg.SyncBootstrapPAT(ctx, name, real)
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestPAT_CreatedBy(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	a, err := pg.CreatePAT(ctx, "a", tokenHash("a"), "admin", nil, "break-glass")
	require.NoError(t, err)
	assert.Equal(t, "break-glass", a.CreatedBy)
	b, err := pg.CreateOwnedPAT(ctx, NewOwnedPAT{Name: "b", TokenHash: tokenHash("b"), Role: "admin",
		Owner: PATOwner{Sub: "s"}, CreatedBy: "oidc:s"})
	require.NoError(t, err)
	assert.Equal(t, "oidc:s", b.CreatedBy)
	c, err := pg.CreateEnrollPAT(ctx, NewEnrollPAT{Name: "c", TokenHash: tokenHash("c"), CreatedBy: "pat:" + a.ID})
	require.NoError(t, err)
	assert.Equal(t, "pat:"+a.ID, c.CreatedBy)
	d, err := pg.CreatePAT(ctx, "d", tokenHash("d"), "admin", nil, "")
	require.NoError(t, err)
	assert.Empty(t, d.CreatedBy)
	boot, err := pg.UpsertBootstrapPAT(ctx, "env:BOOT", tokenHash("boot"))
	require.NoError(t, err)
	assert.Empty(t, boot.CreatedBy)

	got, err := pg.GetPATByHash(ctx, tokenHash("b"))
	require.NoError(t, err)
	assert.Equal(t, "oidc:s", got.CreatedBy)
}

// Migration 030 adds a nullable created_by; the down migration drops it and
// the up migration restores it, keeping existing rows (NULL).
func TestPAT_CreatedByMigrationUpDown(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	down, err := migrationsFS.ReadFile("migrations/030_pat_created_by.down.sql")
	require.NoError(t, err)
	up, err := migrationsFS.ReadFile("migrations/030_pat_created_by.up.sql")
	require.NoError(t, err)
	col := func() int {
		var n int
		require.NoError(t, pg.pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'pats' AND column_name = 'created_by' AND is_nullable = 'YES' AND data_type = 'text'`).Scan(&n))
		return n
	}
	require.Equal(t, 1, col())
	_, err = pg.CreatePAT(ctx, "keep", tokenHash("keep"), "admin", nil, "break-glass")
	require.NoError(t, err)
	_, err = pg.pool.Exec(ctx, string(down))
	require.NoError(t, err)
	require.Equal(t, 0, col())
	_, err = pg.pool.Exec(ctx, string(up))
	require.NoError(t, err)
	require.Equal(t, 1, col())
	pats, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	require.Len(t, pats, 1)
	assert.Empty(t, pats[0].CreatedBy)
}
