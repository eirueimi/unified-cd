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
	pat, err := pg.CreatePAT(ctx, "my-token", hash, nil)
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
	_, _ = pg.CreatePAT(ctx, "a", tokenHash("tokenA"), nil)
	_, _ = pg.CreatePAT(ctx, "b", tokenHash("tokenB"), nil)
	list, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
	require.NoError(t, pg.DeletePAT(ctx, list[0].ID))
	list2, _ := pg.ListPATs(ctx)
	assert.Len(t, list2, 1)
}
