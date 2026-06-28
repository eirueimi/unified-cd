package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_UpsertAndGetJob(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	spec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	job, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", spec)
	require.NoError(t, err)
	assert.Equal(t, "hello", job.Name)
	assert.NotEmpty(t, job.ID)

	got, err := pg.GetJob(ctx, "hello")
	require.NoError(t, err)
	assert.Equal(t, "hello", got.Name)
	assert.JSONEq(t, string(spec), string(got.Spec))

	spec2 := []byte(`{"steps":[{"name":"s","run":"echo y"}]}`)
	_, err = pg.UpsertJob(ctx, "hello", "unified-cd/v1", spec2)
	require.NoError(t, err)

	got2, err := pg.GetJob(ctx, "hello")
	require.NoError(t, err)
	assert.JSONEq(t, string(spec2), string(got2.Spec))
}

func TestPostgres_ListJobs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "a", "unified-cd/v1", []byte(`{}`))
	_, _ = pg.UpsertJob(ctx, "b", "unified-cd/v1", []byte(`{}`))

	jobs, err := pg.ListJobs(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 2)
}
