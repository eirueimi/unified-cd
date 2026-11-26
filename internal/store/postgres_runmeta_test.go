package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedMetaJob(t *testing.T, pg *Postgres) {
	t.Helper()
	_, err := pg.UpsertJob(context.Background(), "job", "v1", []byte(`{}`), JobMeta{})
	require.NoError(t, err)
}

func TestCreateRun_StoresOrigin(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	seedMetaJob(t, pg)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	created, err := pg.CreateRun(ctx, "job", nil, []byte(`{}`), nil, nil, "schedule:nightly", "", RunOrigin{
		Trigger:        "schedule",
		JobLabels:      map[string]string{"team": "a"},
		JobAnnotations: map[string]string{"x": "y"},
		ScheduledAt:    &t0,
	})
	require.NoError(t, err)
	assert.Equal(t, "schedule", created.Trigger)

	got, err := pg.GetRun(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "schedule", got.Trigger)
	assert.Equal(t, map[string]string{"team": "a"}, got.JobLabels)
	assert.Equal(t, map[string]string{"x": "y"}, got.JobAnnotations)
	require.NotNil(t, got.ScheduledAt)
	assert.True(t, got.ScheduledAt.Equal(t0))
}

func TestCreateRun_DefaultsTriggerAPI(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	seedMetaJob(t, pg)
	created, err := pg.CreateRun(ctx, "job", nil, []byte(`{}`), nil, nil, "alice", "", RunOrigin{})
	require.NoError(t, err)
	got, err := pg.GetRun(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "api", got.Trigger)
	assert.Nil(t, got.JobLabels)
	assert.Nil(t, got.JobAnnotations)
	assert.Nil(t, got.ScheduledAt)
}

func TestClaimNextRun_CarriesOrigin(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	seedMetaJob(t, pg)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, err := pg.CreateRun(ctx, "job", nil, []byte(`{}`), nil, nil, "schedule:nightly", "", RunOrigin{
		Trigger:        "schedule",
		JobLabels:      map[string]string{"team": "a"},
		JobAnnotations: map[string]string{"x": "y"},
		ScheduledAt:    &t0,
	})
	require.NoError(t, err)

	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	cr, err := pg.ClaimNextRun(ctx, "agent-1", nil, nil)
	require.NoError(t, err)
	require.NotNil(t, cr)
	assert.Equal(t, "schedule", cr.Trigger)
	assert.Equal(t, map[string]string{"team": "a"}, cr.JobLabels)
	assert.Equal(t, map[string]string{"x": "y"}, cr.JobAnnotations)
	require.NotNil(t, cr.ScheduledAt)
	assert.True(t, cr.ScheduledAt.Equal(t0))
}

func TestClaimNextRunDetached_CarriesOrigin(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	seedMetaJob(t, pg)
	_, err := pg.CreateRun(ctx, "job", nil, []byte(`{"detached":true}`), nil, nil, "webhook:h", "", RunOrigin{
		Trigger:   "webhook",
		JobLabels: map[string]string{"team": "a"},
	})
	require.NoError(t, err)
	_, err = pg.TransitionPendingToQueued(ctx, 10)
	require.NoError(t, err)
	cr, err := pg.ClaimNextRunDetached(ctx, "agent-1", nil)
	require.NoError(t, err)
	require.NotNil(t, cr)
	assert.Equal(t, "webhook", cr.Trigger)
	assert.Equal(t, map[string]string{"team": "a"}, cr.JobLabels)
	assert.Nil(t, cr.ScheduledAt)
}

func TestUpsertJob_StoresLabelsAnnotations(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "v1", []byte(`{}`), JobMeta{
		Labels:      map[string]string{"team": "a"},
		Annotations: map[string]string{"x": "y"},
	})
	require.NoError(t, err)
	got, err := pg.GetJob(ctx, "j")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team": "a"}, got.Labels)
	assert.Equal(t, map[string]string{"x": "y"}, got.Annotations)

	_, err = pg.UpsertJob(ctx, "j", "v1", []byte(`{}`), JobMeta{})
	require.NoError(t, err)
	got, err = pg.GetJob(ctx, "j")
	require.NoError(t, err)
	assert.Nil(t, got.Labels)
	assert.Nil(t, got.Annotations)
}

func TestMigration034_BackfillsTrigger(t *testing.T) {
	dsn := NewEmptyTestDatabase(t)
	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 33))
	pg, err := NewPostgres(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	ctx := context.Background()
	_, err = pg.pool.Exec(ctx, `INSERT INTO jobs(name, api_version, spec) VALUES ('job', 'v1', '{}')`)
	require.NoError(t, err)

	want := map[string]string{
		"webhook:a":  "webhook",
		"schedule:b": "schedule",
		"replay:c":   "replay",
		"agent:d":    "call",
		"alice":      "api",
	}
	ids := map[string]string{}
	for by := range want {
		var id string
		require.NoError(t, pg.pool.QueryRow(ctx,
			`INSERT INTO runs(job_name, params, spec, triggered_by) VALUES ('job', '{}', '{}', $1) RETURNING id`, by).Scan(&id))
		ids[by] = id
	}

	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 34))
	for by, trig := range want {
		var got string
		require.NoError(t, pg.pool.QueryRow(ctx, `SELECT trigger FROM runs WHERE id = $1`, ids[by]).Scan(&got))
		assert.Equal(t, trig, got, by)
	}

	// A replica that predates 034 inserts without the column: the row stays
	// NULL and reads back as an empty trigger, never as a wrong 'api'.
	var oldID string
	require.NoError(t, pg.pool.QueryRow(ctx,
		`INSERT INTO runs(job_name, params, spec, triggered_by) VALUES ('job', '{}', '{}', 'alice') RETURNING id`).Scan(&oldID))
	var isNull bool
	require.NoError(t, pg.pool.QueryRow(ctx, `SELECT trigger IS NULL FROM runs WHERE id = $1`, oldID).Scan(&isNull))
	assert.True(t, isNull, "insert without trigger leaves NULL")
	got, err := pg.GetRun(ctx, oldID)
	require.NoError(t, err)
	assert.Equal(t, "", got.Trigger)

	require.NoError(t, (&Postgres{}).MigrateTo(dsn, 33))
	var n int
	require.NoError(t, pg.pool.QueryRow(ctx,
		`SELECT count(*) FROM information_schema.columns WHERE table_name IN ('runs','jobs') AND column_name IN ('trigger','job_labels','job_annotations','scheduled_at','labels','annotations')`).Scan(&n))
	assert.Equal(t, 0, n, "down drops the six columns")
}
