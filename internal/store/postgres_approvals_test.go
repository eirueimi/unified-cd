package store

import (
	"context"
	"testing"
	"time"

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

func TestPostgres_MarkExpiredApprovalsTimedOut(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "jr", "unified-cd/v1", []byte(`{}`))

	// Run with an EXPIRED Pending approval (timeout_at in the past).
	expiredRun, err := pg.CreateRun(ctx, "jr", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	require.NoError(t, pg.CreatePendingApproval(ctx, expiredRun.ID, 0, "gate", "ok?", &past))

	// Run with a FUTURE Pending approval — must NOT be touched.
	futureRun, err := pg.CreateRun(ctx, "jr", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	future := time.Now().Add(time.Hour)
	require.NoError(t, pg.CreatePendingApproval(ctx, futureRun.ID, 0, "gate", "ok?", &future))

	// Run with an already-Approved (expired) approval — must NOT be touched.
	approvedRun, err := pg.CreateRun(ctx, "jr", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.CreatePendingApproval(ctx, approvedRun.ID, 0, "gate", "ok?", &past))
	changed, err := pg.DecideApproval(ctx, approvedRun.ID, 0, "Approved", "alice", "lgtm")
	require.NoError(t, err)
	require.True(t, changed)

	// Only the expired Pending row should be updated.
	n, err := pg.MarkExpiredApprovalsTimedOut(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Expired Pending → TimedOut, decided_by=system, decided_at set.
	got, err := pg.GetApproval(ctx, expiredRun.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "TimedOut", got.Status)
	assert.Equal(t, "system", got.DecidedBy)
	require.NotNil(t, got.DecidedAt)

	// Future Pending untouched.
	got, err = pg.GetApproval(ctx, futureRun.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "Pending", got.Status)

	// Already-Approved untouched.
	got, err = pg.GetApproval(ctx, approvedRun.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "Approved", got.Status)
	assert.Equal(t, "alice", got.DecidedBy)

	// Idempotent: a second reap finds nothing to do.
	n, err = pg.MarkExpiredApprovalsTimedOut(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
