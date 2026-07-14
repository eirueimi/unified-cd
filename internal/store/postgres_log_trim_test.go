package store

import (
	"context"
	"testing"

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
