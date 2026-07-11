package store

import (
	"context"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_ListRunsNeedingArchival(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run1, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	_, _ = pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "") // run2 stays Pending

	// transition run1 to a completed state
	_, _ = pg.TransitionPendingToQueued(ctx, 10)
	_, _ = pg.ClaimNextRun(ctx, "agent-1", nil)
	require.NoError(t, pg.MarkRunRunning(ctx, run1.ID))
	require.NoError(t, pg.MarkRunFinished(ctx, run1.ID, api.RunSucceeded))

	runs, err := pg.ListRunsNeedingArchival(ctx, 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, run1.ID, runs[0].ID)
}

func TestPostgres_CreateAndGetLogArchive(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")

	// returns nil when no archive is registered
	arch, err := pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	assert.Nil(t, arch)

	// register an archive
	key := "runs/" + run.ID + "/logs.ndjson"
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, key, 1234))

	arch, err = pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, arch)
	assert.Equal(t, key, arch.ObjectKey)
	assert.Equal(t, int64(1234), arch.SizeBytes)
}
