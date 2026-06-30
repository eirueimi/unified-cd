package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_Approvals_CreateDecide(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", nil))

	// idempotent: second create does not error or overwrite
	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 1, "gate", "ok?", nil))

	got, err := pg.GetApproval(ctx, run.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, "Pending", got.Status)

	changed, err := pg.DecideApproval(ctx, run.ID, 1, "Approved", "alice", "lgtm")
	require.NoError(t, err)
	assert.True(t, changed)

	// second decision: no change (already decided)
	changed, err = pg.DecideApproval(ctx, run.ID, 1, "Rejected", "bob", "")
	require.NoError(t, err)
	assert.False(t, changed)

	got, _ = pg.GetApproval(ctx, run.ID, 1)
	assert.Equal(t, "Approved", got.Status)
	assert.Equal(t, "alice", got.DecidedBy)

	list, err := pg.ListRunApprovals(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
}

func TestPostgres_Approvals_ListEmpty(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j2", "unified-cd/v1", []byte(`{}`))
	run, err := pg.CreateRun(ctx, "j2", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)

	list, err := pg.ListRunApprovals(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Len(t, list, 0)
}
