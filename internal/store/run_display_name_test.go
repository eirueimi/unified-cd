package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateRunPersistsAndReturnsDisplayName(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "test-job", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)

	run, err := pg.CreateRun(ctx, "test-job", map[string]string{"env": "prod"}, []byte(`{"steps":[]}`), nil, nil, "api", "deploy prod")
	require.NoError(t, err)
	require.Equal(t, "deploy prod", run.DisplayName)

	fetched, err := pg.GetRun(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, "deploy prod", fetched.DisplayName)
}

func TestCreateRunWithEmptyDisplayNameStoresNoneAndListRunsOmitsIt(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, err := pg.UpsertJob(ctx, "test-job2", "unified-cd/v1", []byte(`{"steps":[]}`))
	require.NoError(t, err)

	run, err := pg.CreateRun(ctx, "test-job2", nil, []byte(`{"steps":[]}`), nil, nil, "api", "")
	require.NoError(t, err)
	require.Empty(t, run.DisplayName) // existing-run rendering must be unaffected

	runs, err := pg.ListRunsByJob(ctx, "test-job2", 10)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Empty(t, runs[0].DisplayName)
}
