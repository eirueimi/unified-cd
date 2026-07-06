package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetRun_ClaimedByReflectsClaimingAgent(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// Unclaimed run: ClaimedBy is empty.
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	got, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ClaimedBy, "a freshly created run has no claiming agent")

	// After claim: ClaimedBy is the claiming agent's ID.
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	claimed, err := pg.ClaimNextRun(ctx, "agent-xyz", nil)
	require.NoError(t, err)
	require.NotNil(t, claimed)

	got, err = pg.GetRun(ctx, claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent-xyz", got.ClaimedBy)
}
