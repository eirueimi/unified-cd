package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/require"
)

// waitQueued polls until the run reaches Queued (or fails the test after timeout).
func waitQueued(t *testing.T, pg *store.Postgres, runID string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		run, err := pg.GetRun(context.Background(), runID)
		require.NoError(t, err)
		if run.Status == api.RunQueued {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s not Queued within %s (status=%s)", runID, within, run.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestHA_SchedulerFailover verifies that when the scheduler leader goes down,
// a surviving replica takes over and no pending runs are lost.
func TestHA_SchedulerFailover(t *testing.T) {
	pg := store.NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// Replica A becomes leader and queues a pending run.
	ctxA, cancelA := context.WithCancel(context.Background())
	go RunScheduler(ctxA, pg, 50*time.Millisecond)

	runA, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	waitQueued(t, pg, runA.ID, 3*time.Second) // A is leader

	// Leader A goes down -> its advisory lock is released.
	cancelA()

	// Replica B takes over and queues a new pending run (no run lost).
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	go RunScheduler(ctxB, pg, 50*time.Millisecond)

	runB, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	waitQueued(t, pg, runB.ID, 5*time.Second) // B took over
}
