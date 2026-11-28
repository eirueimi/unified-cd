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

// testPATAccountID returns the id of a human account that test PATs belong to
// (created on first use).
func testPATAccountID(t testing.TB, pg *Postgres) string {
	t.Helper()
	acc, err := pg.UpsertHumanAccount(context.Background(), "test-pat-owner", "", "owner@example.com", nil)
	require.NoError(t, err)
	return acc.ID
}

// createTestPAT stores a PAT owned by the shared test account.
func createTestPAT(t testing.TB, pg *Postgres, name, hash, role, createdBy string) *PAT {
	t.Helper()
	pat, err := pg.CreatePAT(context.Background(), NewPAT{
		Name: name, TokenHash: hash, Role: role, CreatedBy: createdBy, AccountID: testPATAccountID(t, pg),
	})
	require.NoError(t, err)
	return pat
}

func TestPostgres_PATCreateAndGet(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	hash := tokenHash("exc_testtoken123")
	pat := createTestPAT(t, pg, "my-token", hash, "admin", "")
	assert.Equal(t, "my-token", pat.Name)
	assert.NotEmpty(t, pat.ID)

	got, err := pg.GetPATByHash(ctx, hash)
	require.NoError(t, err)
	assert.Equal(t, pat.ID, got.ID)

	_, err = pg.GetPATByHash(ctx, "wronghash")
	require.Error(t, err)
}

func TestPostgres_PATListAndDelete(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	createTestPAT(t, pg, "a", tokenHash("tokenA"), "admin", "")
	createTestPAT(t, pg, "b", tokenHash("tokenB"), "admin", "")
	list, err := pg.ListPATs(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 2)
	require.NoError(t, pg.DeletePAT(ctx, list[0].ID))
	list2, _ := pg.ListPATs(ctx)
	assert.Len(t, list2, 1)
}

func TestPAT_CreatedBy(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	a := createTestPAT(t, pg, "a", tokenHash("a"), "admin", "pat:parent")
	assert.Equal(t, "pat:parent", a.CreatedBy)
	b := createTestPAT(t, pg, "b", tokenHash("b"), "admin", "oidc:s")
	assert.Equal(t, "oidc:s", b.CreatedBy)
	d := createTestPAT(t, pg, "d", tokenHash("d"), "admin", "")
	assert.Empty(t, d.CreatedBy)

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
	createTestPAT(t, pg, "keep", tokenHash("keep"), "admin", "break-glass")
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
