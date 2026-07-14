package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_LogArchive_TrimmedAt(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 2))

	arch, err := pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, arch)
	assert.Nil(t, arch.TrimmedAt, "fresh archive record must not be marked trimmed")

	_, err = pg.pool.Exec(ctx,
		`UPDATE run_log_archives SET trimmed_at = NOW() WHERE run_id = $1`, run.ID)
	require.NoError(t, err)

	arch, err = pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, arch)
	assert.NotNil(t, arch.TrimmedAt)
}

func TestPostgres_TrimRunLogs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// archived: has logs and an archive record -> trimmable.
	archived, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = pg.AppendLog(ctx, archived.ID, 0, "stdout", time.Now(), "line")
		require.NoError(t, err)
	}
	require.NoError(t, pg.CreateLogArchive(ctx, archived.ID, "runs/"+archived.ID+"/logs.ndjson", 10))

	// unarchived: has logs but NO archive record -> must never be trimmed.
	unarchived, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.AppendLog(ctx, unarchived.ID, 0, "stdout", time.Now(), "keep me")
	require.NoError(t, err)

	n, err := pg.TrimRunLogs(ctx, archived.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	count, _, _, err := pg.CountLogs(ctx, archived.ID, nil)
	require.NoError(t, err)
	assert.Zero(t, count, "logs rows must be gone")
	arch, err := pg.GetLogArchive(ctx, archived.ID)
	require.NoError(t, err)
	require.NotNil(t, arch)
	assert.NotNil(t, arch.TrimmedAt, "trim must mark the archive record")

	// Second trim is a no-op.
	n, err = pg.TrimRunLogs(ctx, archived.ID)
	require.NoError(t, err)
	assert.Zero(t, n)

	// No archive record: no-op AND logs untouched (guard ordering).
	n, err = pg.TrimRunLogs(ctx, unarchived.ID)
	require.NoError(t, err)
	assert.Zero(t, n)
	count, _, _, err = pg.CountLogs(ctx, unarchived.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "logs of an unarchived run must never be deleted")
}

func TestPostgres_ListTrimCandidates(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	mkArchived := func(age string, trimmed bool) string {
		t.Helper()
		run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
		require.NoError(t, err)
		require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1))
		if age != "" {
			_, err = pg.pool.Exec(ctx,
				`UPDATE run_log_archives SET archived_at = NOW() - $1::interval WHERE run_id = $2`, age, run.ID)
			require.NoError(t, err)
		}
		if trimmed {
			_, err = pg.pool.Exec(ctx,
				`UPDATE run_log_archives SET trimmed_at = NOW() WHERE run_id = $1`, run.ID)
			require.NoError(t, err)
		}
		return run.ID
	}

	oldest := mkArchived("20 days", false)
	older := mkArchived("10 days", false)
	_ = mkArchived("20 days", true) // already trimmed: excluded
	_ = mkArchived("", false)       // fresh: excluded by cutoff

	cutoff := time.Now().AddDate(0, 0, -7)
	ids, err := pg.ListTrimCandidates(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older}, ids, "untrimmed + old only, oldest archived_at first")

	ids, err = pg.ListTrimCandidates(ctx, cutoff, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest}, ids)
}

func TestPostgres_DeleteLogArchive(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1))

	require.NoError(t, pg.DeleteLogArchive(ctx, run.ID))
	arch, err := pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	assert.Nil(t, arch)
	// Idempotent on a missing record.
	require.NoError(t, pg.DeleteLogArchive(ctx, run.ID))
}
