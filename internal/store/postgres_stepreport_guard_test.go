package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/api"
)

// TestPostgres_UpsertStepReport_RejectsTerminalRun proves the guard lives in the
// write itself: a report on a finalized run (as when finalization lands between a
// caller's status check and the write) returns ErrRunFinalized and neither inserts
// nor updates any step row.
func TestPostgres_UpsertStepReport_RejectsTerminalRun(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, pg.UpsertStepReport(ctx, run.ID, 0, 0, "s0", "", "Running", nil, nil, nil, "", ""))
	updated, err := pg.FinishRun(ctx, run.ID, api.RunFailed)
	require.NoError(t, err)
	require.True(t, updated)

	ec := 0
	end := time.Now().UTC()
	err = pg.UpsertStepReport(ctx, run.ID, 0, 0, "s0", "", "Succeeded", &ec, nil, &end, "", "")
	require.ErrorIs(t, err, ErrRunFinalized)
	err = pg.UpsertStepReport(ctx, run.ID, 1, 0, "s1", "", "Running", nil, nil, nil, "", "")
	require.ErrorIs(t, err, ErrRunFinalized)

	steps, err := pg.GetRunSteps(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, "Running", steps[0].Status)
	assert.Nil(t, steps[0].ExitCode)

	err = pg.UpsertStepReport(ctx, "00000000-0000-0000-0000-000000000000", 0, 0, "s", "", "Running", nil, nil, nil, "", "")
	require.ErrorIs(t, err, ErrRunNotFound)
}
