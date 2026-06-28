package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_EnsureControllerKey_FirstCallPersistsCandidate(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	got, err := pg.EnsureControllerKey(ctx, "candidate-key-hex")
	require.NoError(t, err)
	assert.Equal(t, "candidate-key-hex", got)
}

func TestPostgres_EnsureControllerKey_SubsequentCallsReturnPersistedValue(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	first, err := pg.EnsureControllerKey(ctx, "first-candidate")
	require.NoError(t, err)
	assert.Equal(t, "first-candidate", first)

	// even when a different candidate is supplied, the already-persisted value is returned (a different key generated on each restart is never overwritten)
	second, err := pg.EnsureControllerKey(ctx, "second-candidate")
	require.NoError(t, err)
	assert.Equal(t, "first-candidate", second)
}
