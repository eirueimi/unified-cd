package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_AppendLog_SealedAfterArchive: once a run_log_archives record
// exists, AppendLog drops the line — (0, nil), nothing stored, no error.
func TestPostgres_AppendLog_SealedAfterArchive(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)

	// Unsealed: insert works and returns a real seq (sequences start at 1).
	seq, err := pg.AppendLog(ctx, run.ID, 0, "stdout", time.Now(), "before seal")
	require.NoError(t, err)
	assert.Positive(t, seq)

	// Seal by creating the archive record (values irrelevant to the seal).
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1, 1, seq))

	seq, err = pg.AppendLog(ctx, run.ID, 0, "stdout", time.Now(), "after seal")
	require.NoError(t, err, "sealed append must not be an error")
	assert.Zero(t, seq, "sealed append must report the dropped sentinel")

	count, _, _, err := pg.CountLogs(ctx, run.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "the sealed line must not be stored")

	// A different, unsealed run is unaffected.
	other, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	seq, err = pg.AppendLog(ctx, other.ID, 0, "stdout", time.Now(), "unsealed run")
	require.NoError(t, err)
	assert.Positive(t, seq)
}
