package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_ScheduleCRUD(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	// prerequisite: create a job (FK constraint)
	_, err := pg.UpsertJob(ctx, "nightly-build", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// Upsert
	s, err := pg.UpsertSchedule(ctx, "nightly", "0 2 * * *", "nightly-build", map[string]string{"env": "prod"})
	require.NoError(t, err)
	assert.Equal(t, "nightly", s.Name)
	assert.Equal(t, "0 2 * * *", s.Cron)
	assert.Equal(t, "nightly-build", s.JobName)
	assert.Equal(t, "prod", s.Params["env"])
	assert.Nil(t, s.LastFiredAt)

	// Get
	got, err := pg.GetSchedule(ctx, "nightly")
	require.NoError(t, err)
	assert.Equal(t, "nightly", got.Name)

	// List
	list, err := pg.ListSchedules(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// UpdateScheduleLastFiredAt
	firedAt := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, pg.UpdateScheduleLastFiredAt(ctx, "nightly", firedAt))
	got, err = pg.GetSchedule(ctx, "nightly")
	require.NoError(t, err)
	require.NotNil(t, got.LastFiredAt)
	assert.WithinDuration(t, firedAt, *got.LastFiredAt, time.Second)

	// Delete
	require.NoError(t, pg.DeleteSchedule(ctx, "nightly"))
	_, err = pg.GetSchedule(ctx, "nightly")
	require.Error(t, err)
}

func TestPostgres_UpsertSchedule_Update(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "job1", "unified-cd/v1", []byte(`{}`))

	_, err := pg.UpsertSchedule(ctx, "s1", "0 2 * * *", "job1", nil)
	require.NoError(t, err)

	// update the cron expression
	s2, err := pg.UpsertSchedule(ctx, "s1", "0 3 * * *", "job1", map[string]string{"k": "v"})
	require.NoError(t, err)
	assert.Equal(t, "0 3 * * *", s2.Cron)
	assert.Equal(t, "v", s2.Params["k"])
}

func TestPostgres_Schedule_DeleteOnJobDelete(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "job2", "unified-cd/v1", []byte(`{}`))
	_, err := pg.UpsertSchedule(ctx, "s2", "0 2 * * *", "job2", nil)
	require.NoError(t, err)

	// delete the job — the schedule is also deleted via ON DELETE CASCADE
	_, err = pg.pool.Exec(ctx, `DELETE FROM jobs WHERE name = $1`, "job2")
	require.NoError(t, err)

	list, err := pg.ListSchedules(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)
}
