package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

func TestRunLogArchiver_OnlyOneLeaderArchives(t *testing.T) {
	pg := store.NewTestPostgres(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())

	// Create a completed Run.
	_, _ = pg.UpsertJob(t.Context(), "j", "unified-cd/v1", []byte(`{}`))
	run, _ := pg.CreateRun(t.Context(), "j", nil, []byte(`{}`), nil, "")
	_ = pg.MarkRunRunning(t.Context(), run.ID)
	_ = pg.MarkRunFinished(t.Context(), run.ID, api.RunSucceeded)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start two RunLogArchiver instances concurrently.
	go RunLogArchiver(ctx, pg, obj, 50*time.Millisecond)
	go RunLogArchiver(ctx, pg, obj, 50*time.Millisecond)

	// Wait until archival completes.
	require.Eventually(t, func() bool {
		archive, _ := pg.GetLogArchive(t.Context(), run.ID)
		return archive != nil
	}, 3*time.Second, 100*time.Millisecond)

	// There should be exactly one archive record (no duplicates).
	archive, err := pg.GetLogArchive(t.Context(), run.ID)
	require.NoError(t, err)
	require.NotNil(t, archive)
}
