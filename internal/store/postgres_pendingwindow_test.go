package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A run blocked on a held mutex is never written at all — tryQueueRun rolls back
// and leaves it Pending — so with a FIXED head window those runs re-fill every
// snapshot forever, and a fully runnable run past the window is never EXAMINED.
// Measured in production shape as 252.6s and 787.6s of starvation against a 0.185s
// baseline, with an idle agent long-polling for work throughout and no log line
// anywhere.
//
// This test reproduces that exactly: window of 5, 5 mutex-blocked runs ahead of one
// unconstrained run.
func TestTransitionPendingToQueued_FixedWindowStarvesRunsBehindABlockedHead(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	const window = 5
	blockedSpec := []byte(`{"concurrency":{"mutex":"hog"}}`)
	// window+1 runs contending for one mutex: the first takes it and leaves
	// Pending; the remaining `window` stay Pending forever (nothing releases it)
	// and occupy the whole head window on every tick.
	for i := 0; i < window+1; i++ {
		_, err := pg.CreateRun(ctx, "j", nil, blockedSpec, nil, nil, "", "")
		require.NoError(t, err)
	}
	probe, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")
	require.NoError(t, err)

	// The fixed head window: however many ticks pass, the probe is never examined.
	for i := 0; i < 10; i++ {
		n, err := pg.TransitionPendingToQueued(ctx, window)
		require.NoError(t, err)
		if i > 0 {
			assert.Equal(t, 0, n, "only the mutex winner ever leaves the head window")
		}
	}
	got, err := pg.GetRun(ctx, probe.ID)
	require.NoError(t, err)
	require.Equal(t, "Pending", string(got.Status),
		"precondition: with a fixed head window the probe is never even looked at")

	// The moving window reaches it, at unchanged per-tick cost.
	var cursor *PendingCursor
	for i := 0; i < 5; i++ {
		_, cursor, err = pg.TransitionPendingToQueuedFrom(ctx, window, cursor)
		require.NoError(t, err)
		got, err = pg.GetRun(ctx, probe.ID)
		require.NoError(t, err)
		if got.Status == "Queued" {
			break
		}
	}
	assert.Equal(t, "Queued", string(got.Status),
		"a runnable run behind a blocked head must be examined within a few ticks")
}

// The cursor must not skip rows as the Pending set shrinks under it, and must wrap
// back to the head once the end is reached.
func TestTransitionPendingToQueuedFrom_SweepsEveryRunAndWraps(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	const total = 12
	ids := make([]string, 0, total)
	for i := 0; i < total; i++ {
		r, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")
		require.NoError(t, err)
		ids = append(ids, r.ID)
	}

	queued := 0
	var cursor *PendingCursor
	for i := 0; i < 10 && queued < total; i++ {
		n, next, err := pg.TransitionPendingToQueuedFrom(ctx, 5, cursor)
		require.NoError(t, err)
		queued += n
		cursor = next
	}
	assert.Equal(t, total, queued)
	for _, id := range ids {
		r, err := pg.GetRun(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, "Queued", string(r.Status))
	}

	// Nothing Pending left: the sweep reports the end of the set, so the caller
	// wraps to the head rather than paging off into empty space forever.
	n, next, err := pg.TransitionPendingToQueuedFrom(ctx, 5, cursor)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Nil(t, next)
}

// Runs created in the same instant must not be skipped by the cursor: the keyset is
// (created_at, id), not created_at alone.
func TestTransitionPendingToQueuedFrom_TiedCreatedAtIsNotSkipped(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		r, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")
		require.NoError(t, err)
		ids = append(ids, r.ID)
	}
	tie := time.Now().Add(-time.Hour)
	for _, id := range ids {
		_, err := pg.pool.Exec(ctx, `UPDATE runs SET created_at = $1 WHERE id = $2`, tie, id)
		require.NoError(t, err)
	}

	total := 0
	var cursor *PendingCursor
	for i := 0; i < 4; i++ {
		n, next, err := pg.TransitionPendingToQueuedFrom(ctx, 2, cursor)
		require.NoError(t, err)
		total += n
		cursor = next
		if next == nil {
			break
		}
	}
	assert.Equal(t, 4, total, "identical created_at must not lose runs to the cursor")
}

// The batch bound exists to cap how stale the reaper's liveness answer can get while
// it works through the list; without it the window scaled with the backlog (measured
// 475ms for 161 runs against 4ms for one).
func TestListUnclaimableQueuedRuns_RespectsTheBatchBound(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, _ = pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))

	for i := 0; i < 7; i++ {
		_, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "", "")
		require.NoError(t, err)
	}
	_, err := pg.TransitionPendingToQueued(ctx, 20)
	require.NoError(t, err)

	refs, err := pg.ListUnclaimableQueuedRuns(ctx, 0, 60*time.Second, 3)
	require.NoError(t, err)
	assert.Len(t, refs, 3)

	refs, err = pg.ListUnclaimableQueuedRuns(ctx, 0, 60*time.Second, 0)
	require.NoError(t, err)
	assert.Len(t, refs, 7, "a non-positive limit means unbounded")
}
