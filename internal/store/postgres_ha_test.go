package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHA_NoDoubleClaim verifies that concurrent claimers never claim the same
// run twice (FOR UPDATE SKIP LOCKED conflict-free claiming).
func TestHA_NoDoubleClaim(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()

	const runs = 50
	const claimers = 8

	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	spec := []byte(`{"steps":[{"name":"s","run":"echo x"}]}`)
	for i := 0; i < runs; i++ {
		_, err := pg.CreateRun(ctx, "j", nil, spec, nil, "")
		require.NoError(t, err)
	}
	// Move all Pending -> Queued so ClaimNextRun can pick them up.
	n, err := pg.TransitionPendingToQueued(ctx, runs)
	require.NoError(t, err)
	require.Equal(t, runs, n)

	var mu sync.Mutex
	claimedBy := map[string]int{} // runID -> number of times claimed

	var wg sync.WaitGroup
	for c := 0; c < claimers; c++ {
		wg.Add(1)
		go func(agentIdx int) {
			defer wg.Done()
			agentID := "agent-" + string(rune('a'+agentIdx))
			for {
				claimed, err := pg.ClaimNextRun(ctx, agentID, nil)
				if err != nil {
					t.Errorf("claim error: %v", err)
					return
				}
				if claimed == nil {
					return // queue drained
				}
				mu.Lock()
				claimedBy[claimed.ID]++
				mu.Unlock()
			}
		}(c)
	}
	wg.Wait()

	require.Len(t, claimedBy, runs, "every run should be claimed exactly once")
	for id, count := range claimedBy {
		assert.Equal(t, 1, count, "run %s claimed %d times (double claim!)", id, count)
	}
}

// TestHA_AdvisoryLockReleasedOnConnClose verifies PostgreSQL auto-releases a
// session-level advisory lock when the holder's connection dies abruptly
// (the crash-failover path, not a graceful unlock).
func TestHA_AdvisoryLockReleasedOnConnClose(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	const key = int64(0x68617465) // 'hate' — test-only key, distinct from prod keys

	// Open a standalone connection (not from pg's pool) to the same DB and hold the lock.
	dsn := pg.pool.Config().ConnString()
	raw, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	// Ensure the standalone connection is cleaned up on every path (the abrupt
	// close below already kills it; this Close is an idempotent safety net for
	// the error paths before the close).
	t.Cleanup(func() { _ = raw.Close(context.Background()) })
	_, err = raw.Exec(ctx, "SELECT pg_advisory_lock($1)", key)
	require.NoError(t, err)

	// While held, pg cannot acquire it.
	rel, err := pg.AcquireAdvisoryLock(ctx, key)
	require.NoError(t, err)
	require.Nil(t, rel, "lock is held by the standalone connection")

	// Simulate a crash: close the underlying network connection abruptly.
	require.NoError(t, raw.PgConn().Conn().Close())

	// PostgreSQL should release the lock on session end; another acquire succeeds.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rel, err := pg.AcquireAdvisoryLock(ctx, key)
		require.NoError(t, err)
		if rel != nil {
			rel()
			return // released, as expected
		}
		if time.Now().After(deadline) {
			t.Fatal("advisory lock was not released within 5s of connection close")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
