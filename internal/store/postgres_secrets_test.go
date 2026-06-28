package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_SecretsCRUD(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	encDEK := []byte("encrypted-dek-bytes")
	ct := []byte("ciphertext-bytes")

	// Upsert
	s, err := pg.UpsertSecret(ctx, "AWS_KEY", "global", "", encDEK, ct)
	require.NoError(t, err)
	assert.Equal(t, "AWS_KEY", s.Name)
	assert.Equal(t, "global", s.Scope)
	assert.Equal(t, encDEK, s.EncryptedDEK)

	// Get
	got, err := pg.GetSecret(ctx, "AWS_KEY", "global", "")
	require.NoError(t, err)
	assert.Equal(t, "AWS_KEY", got.Name)
	assert.Equal(t, ct, got.Ciphertext)

	// Upsert update
	newCT := []byte("new-ciphertext")
	_, err = pg.UpsertSecret(ctx, "AWS_KEY", "global", "", encDEK, newCT)
	require.NoError(t, err)
	updated, _ := pg.GetSecret(ctx, "AWS_KEY", "global", "")
	assert.Equal(t, newCT, updated.Ciphertext)

	// List
	_, _ = pg.UpsertSecret(ctx, "DB_PASS", "global", "", encDEK, ct)
	list, err := pg.ListSecrets(ctx, "global", "")
	require.NoError(t, err)
	assert.Len(t, list, 2)

	// Delete
	require.NoError(t, pg.DeleteSecret(ctx, "AWS_KEY", "global", ""))
	list2, _ := pg.ListSecrets(ctx, "global", "")
	assert.Len(t, list2, 1)

	// Get deleted → error
	_, err = pg.GetSecret(ctx, "AWS_KEY", "global", "")
	require.Error(t, err)
}

func TestPostgres_GetSecret_NotFound(t *testing.T) {
	pg := NewTestPostgres(t)
	_, err := pg.GetSecret(context.Background(), "NONEXISTENT", "global", "")
	require.Error(t, err)
}
