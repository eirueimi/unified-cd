package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultPostgresPoolConfig(t *testing.T) {
	assert.Equal(t, PostgresPoolConfig{
		APIMaxConns:        128,
		BackgroundMaxConns: 32,
		LockMaxConns:       16,
		ListenMaxConns:     128,
	}, DefaultPostgresPoolConfig())
}

func TestPostgresPoolIsolation_LockDoesNotStarveAPI(t *testing.T) {
	pg := newIsolatedPoolTestPostgres(t)

	release, err := pg.AcquireAdvisoryLock(t.Context(), 0x706f6f6c)
	require.NoError(t, err)
	require.NotNil(t, release)
	defer release()

	require.EqualValues(t, 1, pg.lockPool.Stat().AcquiredConns())
	requireAPIResponds(t, pg)
}

func TestPostgresPoolIsolation_ListenDoesNotStarveAPI(t *testing.T) {
	pg := newIsolatedPoolTestPostgres(t)

	listenCtx, cancel := context.WithCancel(t.Context())
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- pg.ListenForNotify(listenCtx, "pool_isolation", func(string) {})
	}()
	t.Cleanup(func() {
		cancel()
		<-listenDone
	})

	require.Eventually(t, func() bool {
		return pg.listenPool.Stat().AcquiredConns() == 1
	}, time.Second, 10*time.Millisecond)
	requireAPIResponds(t, pg)
}

func TestPostgresBackgroundStore_UsesIndependentQueryPool(t *testing.T) {
	pg := newIsolatedPoolTestPostgres(t)
	background := pg.BackgroundStore()

	held, err := pg.backgroundPool.Acquire(t.Context())
	require.NoError(t, err)
	defer held.Release()

	blockedCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, background.Ping(blockedCtx), context.DeadlineExceeded)

	requireAPIResponds(t, pg)
}

func TestPostgresClose_ClosesEveryOwnedPool(t *testing.T) {
	pg := newIsolatedPoolTestPostgres(t)
	background := pg.BackgroundStore()

	pg.Close()

	require.Error(t, pg.Ping(t.Context()))
	require.Error(t, background.Ping(t.Context()))
	require.Error(t, pg.lockPool.Ping(t.Context()))
	require.Error(t, pg.listenPool.Ping(t.Context()))
}

func TestListenForNotify_PoolExhausted_ReturnsSentinelInsteadOfBlocking(t *testing.T) {
	pg := newIsolatedPoolTestPostgres(t) // ListenMaxConns: 1

	// Saturate the listen pool's single connection with a long-lived listen.
	holderCtx, cancelHolder := context.WithCancel(t.Context())
	t.Cleanup(cancelHolder)
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- pg.ListenForNotify(holderCtx, "listen_pool_exhaustion_holder", func(string) {})
	}()
	require.Eventually(t, func() bool {
		return pg.listenPool.Stat().AcquiredConns() == 1
	}, time.Second, 10*time.Millisecond)

	// The pool has zero free connections now. A second ListenForNotify call
	// must not block for the caller's ctx lifetime — it must return
	// ErrListenPoolExhausted on its own, within listenAcquireTimeout. Give
	// the call a ctx that outlives listenAcquireTimeout by a wide margin (so
	// a regression to the OLD "block on ctx alone" behavior would return
	// early for the wrong reason if ctx were the thing bounding it) and
	// separately bound the TEST itself with a select/timeout, so a
	// regression makes this test fail fast instead of hanging the suite
	// forever.
	callerCtx, cancelCaller := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelCaller()

	done := make(chan error, 1)
	go func() {
		done <- pg.ListenForNotify(callerCtx, "listen_pool_exhaustion_probe", func(string) {})
	}()

	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrListenPoolExhausted)
	case <-time.After(listenAcquireTimeout + 5*time.Second):
		t.Fatal("ListenForNotify blocked past listenAcquireTimeout+slack instead of returning ErrListenPoolExhausted")
	}
}

func newIsolatedPoolTestPostgres(t *testing.T) *Postgres {
	t.Helper()

	base := NewTestPostgres(t)
	cfg := PostgresPoolConfig{
		APIMaxConns:        1,
		BackgroundMaxConns: 1,
		LockMaxConns:       1,
		ListenMaxConns:     1,
	}
	pg, err := NewPostgresWithPoolConfig(t.Context(), base.pool.Config().ConnString(), cfg)
	require.NoError(t, err)
	t.Cleanup(pg.Close)
	return pg
}

func requireAPIResponds(t *testing.T, pg *Postgres) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	require.NoError(t, pg.Ping(ctx))
}
