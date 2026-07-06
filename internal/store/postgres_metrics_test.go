package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCountRunsByStatus(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	// Create a job first
	_, err := pg.UpsertJob(ctx, "job-a", "v1", []byte(`{}`))
	require.NoError(t, err)

	// Two Pending (CreateRun default), one of them moved to Running,
	// one finished (must not be counted).
	r1, err := pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	_, err = pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	r3, err := pg.CreateRun(ctx, "job-a", nil, []byte(`{}`), nil, "api")
	require.NoError(t, err)
	require.NoError(t, pg.MarkRunRunning(ctx, r1.ID))
	require.NoError(t, pg.MarkRunFinished(ctx, r3.ID, api.RunSucceeded))

	counts, err := pg.CountRunsByStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, counts[api.RunPending])
	assert.Equal(t, 1, counts[api.RunRunning])
	assert.Equal(t, 0, counts[api.RunSucceeded]) // terminal statuses excluded
}

func TestCountAgentsByLiveness(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	require.NoError(t, pg.UpsertAgent(ctx, "agent-fresh", "h1", "linux", "v1", nil, nil))
	require.NoError(t, pg.UpsertAgent(ctx, "agent-stale", "h2", "linux", "v1", nil, nil))
	_, err := pg.pool.Exec(ctx,
		`UPDATE agents SET last_seen_at = NOW() - interval '10 minutes' WHERE id = $1`,
		"agent-stale")
	require.NoError(t, err)

	alive, stale, err := pg.CountAgentsByLiveness(ctx, 90*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, alive)
	assert.Equal(t, 1, stale)
}
