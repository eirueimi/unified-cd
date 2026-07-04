package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListStuckRunIDs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// stuckRun: claimed long ago by an agent that hasn't been seen in a while.
	stuckRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	stuckRunID := stuckRun.ID

	// freshRun: claimed long ago, but the claiming agent has a fresh heartbeat.
	freshRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	freshRunID := freshRun.ID

	// recentRun: claimed just now by a stale agent -- still within the grace window.
	recentRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	recentRunID := recentRun.ID

	// pendingRun: never claimed, still Pending.
	pendingRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	pendingRunID := pendingRun.ID

	require.NoError(t, pg.UpsertAgent(ctx, "agent-stale", "host", "linux", "dev", nil, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "agent-fresh", "host", "linux", "dev", nil, nil))

	_, err = pg.pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = NOW() - interval '5 minutes' WHERE id = $1`, "agent-stale")
	require.NoError(t, err)

	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() - interval '5 minutes' WHERE id = $2`,
		"agent-stale", stuckRunID)
	require.NoError(t, err)

	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() - interval '5 minutes' WHERE id = $2`,
		"agent-fresh", freshRunID)
	require.NoError(t, err)

	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() WHERE id = $2`,
		"agent-stale", recentRunID)
	require.NoError(t, err)

	_ = pendingRunID // stays Pending; no update needed

	ids, err := pg.ListStuckRunIDs(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	assert.Contains(t, ids, stuckRunID)
	assert.NotContains(t, ids, freshRunID)
	assert.NotContains(t, ids, recentRunID)
	assert.NotContains(t, ids, pendingRunID)
}

func TestListStuckRunIDs_MissingAgentCountsAsLost(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	orphanRun, err := pg.CreateRun(ctx, "hello", nil, []byte(`{}`), nil, "")
	require.NoError(t, err)
	orphanRunID := orphanRun.ID

	// claimed_by references an agent that no longer exists (e.g. deleted by DeleteStaleAgents).
	_, err = pg.pool.Exec(ctx,
		`UPDATE runs SET status = 'Running', claimed_by = $1, claimed_at = NOW() - interval '5 minutes' WHERE id = $2`,
		"agent-deleted", orphanRunID)
	require.NoError(t, err)

	ids, err := pg.ListStuckRunIDs(ctx, 90*time.Second, 60*time.Second)
	require.NoError(t, err)
	assert.Contains(t, ids, orphanRunID)
}
