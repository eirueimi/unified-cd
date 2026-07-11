package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runApprovalReaperAsLeader against a real store marks an expired Pending
// approval as TimedOut (system-decided). The full ticker loop is not tested —
// the leader helper covers the reaper logic.
func TestRunApprovalReaperAsLeader_MarksExpired(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "jrr", "unified-cd/v1", []byte(`{}`))

	run, err := pg.CreateRun(ctx, "jrr", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	past := time.Now().Add(-time.Hour)
	require.NoError(t, pg.CreatePendingApproval(ctx, run.ID, 0, "gate", "ok?", &past))

	runApprovalReaperAsLeader(ctx, pg)

	got, err := pg.GetApproval(ctx, run.ID, 0)
	require.NoError(t, err)
	assert.Equal(t, "TimedOut", got.Status)
	assert.Equal(t, "system", got.DecidedBy)
	require.NotNil(t, got.DecidedAt)
}
